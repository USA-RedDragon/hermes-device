"use strict";
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");

const sandbox = { window: {}, Math, Date };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const HubStick = sandbox.window.HubStick;

// A log of messages with heights, a viewport, and the browser's clamp of
// scrollTop to what the content allows.
class Log {
  constructor(heights, client) {
    this.heights = heights;
    this.client = client;
    this.hidden = false;
    this.top = 0;
  }
  get scrollHeight() { return this.heights.reduce((a, b) => a + b, 0); }
  get clientHeight() { return this.client; }
  get scrollTop() { return this.top; }
  set scrollTop(v) { this.top = Math.max(0, Math.min(v, this.scrollHeight - this.client)); }
  clamp() { this.top = Math.max(0, Math.min(this.top, this.scrollHeight - this.client)); }
  start(i) { return this.heights.slice(0, i).reduce((a, b) => a + b, 0); }
}

function make(heights, client) {
  const log = new Log(heights, client);
  let t = 0;
  const calls = [];
  // Messages are named by a tag so that heights can change above them.
  const tags = heights.map((_, i) => i);
  const probe = (l) => {
    for (let i = 0; i < l.heights.length; i++) {
      if (l.start(i) + l.heights[i] > l.top) return { n: tags[i], off: l.start(i) - l.top };
    }
    return null;
  };
  const place = (n, l) => {
    const i = tags.indexOf(n);
    return i < 0 ? null : l.start(i) - l.top;
  };
  const s = HubStick(log, { probe, place, now: () => t, hold: 1000, changed: () => calls.push(s.pinned()) });
  return { log, s, tags, calls, tick: (ms) => { t += ms; } };
}

const wheelTo = (x, top) => {
  x.s.heard();
  x.log.scrollTop = top;
  x.s.scrolled();
};

// A reader at the end stays there when content grows.
{
  const x = make([500, 500, 500], 600);
  x.s.pin();
  assert.strictEqual(x.log.scrollTop, 900);
  x.log.heights[2] += 300;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 1200);
  assert.strictEqual(x.s.pinned(), true);
}

// ... and when the log shrinks under a composer that grows, then grows back.
{
  const x = make([500, 500, 500], 600);
  x.s.pin();
  x.log.client = 450;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 1050);
  x.log.client = 600;
  x.log.clamp();
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 900);
  assert.strictEqual(x.s.pinned(), true);
}

// The clamp the browser makes while the composer is measured is not the
// reader scrolling up, even when the event arrives after the layout settled.
{
  const x = make([500, 500, 500], 600);
  x.s.pin();
  x.log.client = 642;
  x.log.clamp();
  x.log.client = 578;
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), true);
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, x.log.scrollHeight - x.log.client);
  assert.strictEqual(x.s.pinned(), true);
}

// A clamp that leaves the same size behind (the composer measured at no height and back) is undone by the scroll event alone.
{
  const x = make([500, 500, 500], 600);
  x.s.pin();
  x.log.top = 800;
  x.s.scrolled();
  assert.strictEqual(x.log.scrollTop, 900);
  assert.strictEqual(x.s.pinned(), true);
}

// Our own scroll to the end fires an event that changes nothing.
{
  const x = make([500, 500, 500], 600);
  x.s.pin();
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), true);
}

// The reader scrolls up: nothing moves them after that.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  wheelTo(x, 700);
  assert.strictEqual(x.s.pinned(), false);
  const before = x.log.start(1) - x.log.top;
  x.log.heights[3] += 800;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 700, "growth below");
  x.log.client = 400;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 700, "viewport shrinks");
  x.log.client = 900;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 700, "viewport grows");
  x.log.heights[0] += 250;
  x.s.sync();
  assert.strictEqual(x.log.start(1) - x.log.top, before, "growth above");
  assert.strictEqual(x.s.pinned(), false);
}

// Older messages put above the first one in view do not move it.
{
  const x = make([400, 400, 400, 400], 500);
  x.s.pin();
  wheelTo(x, 300);
  const n = x.tags[0];
  x.log.heights.unshift(1200, 1200);
  x.tags.unshift("a", "b");
  x.s.sync();
  assert.strictEqual(x.log.start(x.tags.indexOf(n)) - x.log.top, -300, "first message in view keeps its place");
}

// A change of scrollTop that is not the reader's is undone for a reader above the end.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  wheelTo(x, 500);
  assert.strictEqual(x.s.pinned(), false);
  x.tick(5000);
  x.log.scrollTop = 1400;
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), false);
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 500);
}

// A reader that scrolls back to within the threshold is at the end again.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  wheelTo(x, 100);
  assert.strictEqual(x.s.pinned(), false);
  wheelTo(x, 1390);
  assert.strictEqual(x.s.pinned(), true);
  x.log.heights[3] += 200;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, x.log.scrollHeight - x.log.client);
}

// Any scroll up unpins at once; scrolling down to within the threshold pins again.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  wheelTo(x, 1400 - 20);
  assert.strictEqual(x.s.pinned(), false);
  wheelTo(x, 1400 - 80);
  assert.strictEqual(x.s.pinned(), false);
  wheelTo(x, 1400 - 20);
  assert.strictEqual(x.s.pinned(), true);
}

// pin and release are the page's own clicks and jumps.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  x.log.scrollTop = 300;
  x.s.release();
  assert.strictEqual(x.s.pinned(), false);
  x.log.heights[3] += 100;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 300);
  x.s.pin();
  assert.strictEqual(x.s.pinned(), true);
  assert.strictEqual(x.log.scrollTop, x.log.scrollHeight - x.log.client);
}

// A message that was the anchor and is gone: the reader stays where they are.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  wheelTo(x, 800);
  x.tags.splice(1, 1, "new");
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 800);
  x.log.heights[3] += 100;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 800);
}

// A log that is not shown is left alone, and the page told.
{
  const x = make([500, 500, 500], 600);
  x.s.pin();
  x.log.hidden = true;
  x.log.heights[2] += 300;
  const n = x.calls.length;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 900);
  assert.strictEqual(x.calls.length, n + 1);
}

// The page hears of each change of what the reader sees.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  x.calls.length = 0;
  wheelTo(x, 200);
  assert.deepStrictEqual(x.calls, [false]);
  x.s.pin();
  assert.deepStrictEqual(x.calls, [false, true]);
}
// Content arrives every 50 ms while the reader wheels up: the view never moves down.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  let last = x.log.scrollTop;
  for (let i = 0; i < 40; i++) {
    if (i % 2 === 0) {
      x.s.heard(400);
      x.s.release();
      x.log.scrollTop = x.log.scrollTop - 20;
      x.s.scrolled();
    }
    x.log.heights[3] += 30;
    x.s.sync();
    x.tick(50);
    assert.ok(x.log.scrollTop <= last, "moved down at " + i);
    last = x.log.scrollTop;
  }
  assert.strictEqual(x.s.pinned(), false);
}
console.log("ok");
