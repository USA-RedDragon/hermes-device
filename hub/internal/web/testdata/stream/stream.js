"use strict";
const assert = require("assert");
const path = require("path");
const { load } = require("./harness.js");

const script = process.argv[2];
const fresh = (rand) => load([script]);

// Backoff: doubles from base to max, each wait in the upper half of its step.
{
  const { win } = fresh();
  const b = win.HubBackoff({ base: 500, max: 8000, rand: () => 1 });
  assert.deepStrictEqual([1, 2, 3, 4, 5, 6].map(() => b.next()), [500, 1000, 2000, 4000, 8000, 8000]);
  b.reset();
  assert.strictEqual(b.next(), 500);
  const lo = win.HubBackoff({ base: 500, max: 8000, rand: () => 0 });
  assert.deepStrictEqual([1, 2, 3].map(() => lo.next()), [250, 500, 1000]);
}

const harness = (opt) => {
  const w = fresh();
  const log = { messages: [], lost: 0, back: 0, healthy: 0, failed: 0 };
  const s = w.win.HubStream("/api/sessions/pc/s1/events", {
    message: (m) => log.messages.push(m.lastEventId),
    lost: () => { log.lost++; },
    back: () => { log.back++; },
    healthy: () => { log.healthy++; },
    failed: () => { log.failed++; },
  }, Object.assign({ rand: () => 1 }, opt));
  return { ...w, s, log, last: () => w.sources[w.sources.length - 1] };
};

// One stream, opened at once and not touched until it is closed.
{
  const h = harness();
  assert.strictEqual(h.sources.length, 1);
  assert.strictEqual(h.last().url, "/api/sessions/pc/s1/events");
  h.last().open();
  h.advance(60000);
  assert.strictEqual(h.sources.length, 1, "a healthy stream is not reopened");
  assert.strictEqual(h.log.healthy, 0, "no version check without a failure");
  h.s.close();
  assert.ok(h.last().closed);
  h.advance(60000);
  assert.strictEqual(h.sources.length, 1, "a closed follower opens nothing");
}

// The last event id comes back as ?since=, and an id from another replica
// is sent as it is (the hub answers it with a snapshot).
{
  const h = harness();
  h.last().open();
  h.last().send("a.12");
  h.last().send("a.13");
  h.last().fail();
  h.advance(500);
  assert.strictEqual(h.sources.length, 2);
  assert.strictEqual(h.last().url, "/api/sessions/pc/s1/events?since=a.13");
}

// The incident: every stream dies 50 ms after it opened, for 2 minutes. The
// waits grow to the cap, there are few tries, the banner shows once after
// 5 seconds and stays, and nothing asks for a version check.
{
  const h = harness({ base: 500, max: 15000 });
  const opened = [];
  let t = 0;
  while (t < 120000) {
    const src = h.last();
    src.open();
    h.advance(50);
    src.send("a." + t, "{}");
    src.fail();
    const before = h.now();
    const n = h.sources.length;
    for (let ms = 0; h.sources.length === n && ms < 20000; ms += 10) h.advance(10);
    opened.push(h.now() - before + 50);
    t += h.now() - before + 50;
  }
  assert.ok(h.sources.length <= 20, "tries in 2 minutes: " + h.sources.length);
  assert.ok(h.sources.length >= 8, "tries in 2 minutes: " + h.sources.length);
  const gaps = opened.slice(0, 6);
  assert.deepStrictEqual(gaps.map((g) => Math.round(g / 10) * 10), [550, 1050, 2050, 4050, 8050, 15050], "waits double up to the cap: " + gaps);
  assert.ok(opened.slice(6).every((g) => g >= 14000), "the cap holds: " + opened);
  assert.strictEqual(h.log.lost, 1, "the banner shows once");
  assert.strictEqual(h.log.back, 0);
  assert.strictEqual(h.log.healthy, 0, "no version check while streams die young");
  assert.ok(h.log.failed >= 1 && h.log.failed <= 2, "failed() rings once a minute: " + h.log.failed);
  const urls = new Set(h.sources.map((s) => s.url));
  assert.ok(urls.size > 1);

  // The stream is healthy again: it stays open 2 seconds. The banner goes,
  // the waits start over and the version is checked once.
  h.advance(20000);
  h.last().open();
  h.advance(1900);
  assert.strictEqual(h.log.back, 0, "not healthy yet");
  h.advance(200);
  assert.strictEqual(h.log.back, 1);
  assert.strictEqual(h.log.healthy, 1);
  const n = h.sources.length;
  h.last().fail();
  h.advance(500);
  assert.strictEqual(h.sources.length, n + 1, "after a healthy stream the first wait is short");
  h.s.close();
}

// Banner timing: not before 5 seconds without a healthy stream, counted
// from the first failure, and once for a run of failures.
{
  const h = harness({ base: 500, max: 15000 });
  h.last().open();
  h.advance(3000);
  h.last().fail();
  h.advance(4900);
  assert.strictEqual(h.log.lost, 0, "4.9 s down");
  h.advance(150);
  assert.strictEqual(h.log.lost, 1, "5 s down");
  h.advance(30000);
  assert.strictEqual(h.log.lost, 1);
  h.s.close();
}

// A short outage (a hub restart) never shows the banner, and the hub's
// own reconnect request is answered after 250 ms.
{
  const h = harness();
  h.last().open();
  h.advance(5000);
  h.last().send("a.1", "{}");
  h.s.moving();
  h.last().fail();
  h.advance(249);
  assert.strictEqual(h.sources.length, 1);
  h.advance(2);
  assert.strictEqual(h.sources.length, 2);
  h.last().open();
  h.advance(2100);
  assert.strictEqual(h.log.lost, 0);
  assert.strictEqual(h.log.back, 0);
  assert.strictEqual(h.log.healthy, 1);
  h.s.close();
}

// A stream that never opens: it retries on the backoff, shows the banner
// at 5 seconds, and rings failed() at 30 seconds, then goes on trying.
{
  const h = harness({ base: 500, max: 15000 });
  for (let t = 0; t < 70000; t += 10) {
    const src = h.last();
    if (!src.closed) src.fail();
    h.advance(10);
  }
  assert.strictEqual(h.log.lost, 1);
  assert.ok(h.log.failed >= 1 && h.log.failed <= 3, "failed " + h.log.failed);
  assert.ok(h.sources.length >= 5 && h.sources.length < 25, "tries " + h.sources.length);
  h.s.close();
}
console.log("ok");
