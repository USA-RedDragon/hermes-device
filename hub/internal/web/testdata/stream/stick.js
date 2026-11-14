"use strict";
(async () => {
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

// A scroll nothing announced and the layout did not cause is the reader's:
// find in page, focus moving to a link or a jump to an anchor. It is kept
// for a reader at the end, also while a reply streams, and for one above it.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  x.tick(5000);
  x.log.scrollTop = 200; // Ctrl+F found older text
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), false);
  for (let i = 0; i < 5; i++) {
    x.tick(50);
    x.log.heights[3] += 40; // the reply grows below
    x.s.sync();
  }
  assert.strictEqual(x.log.scrollTop, 200, "streaming does not take the reader back to the end");
  x.log.scrollTop = 1500; // a jump to an anchor lower down
  x.s.scrolled();
  x.log.heights[3] += 40;
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 1500);
}

// A scroll that comes with a change of the log's height, or soon after one,
// is the layout's and is undone: a reader above the end keeps the place.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  wheelTo(x, 500);
  x.tick(5000);
  x.log.client = 400; // the composer grew
  x.log.scrollTop = 1100;
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), false);
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 500);
  x.tick(100);
  x.log.scrollTop = 1400; // the clamp event after the layout settled
  x.s.scrolled();
  assert.strictEqual(x.log.scrollTop, 500);
  x.tick(2000);
  x.log.scrollTop = 1400; // much later it is a find
  x.s.scrolled();
  assert.strictEqual(x.log.scrollTop, 1400);
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

// A scroll up of less than the threshold (a tap's jitter, a sub-pixel
// scrollTop) keeps the reader at the end; a real distance unpins, and
// scrolling down to within the threshold pins again.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  for (const d of [0.5, 1, 3, 20]) {
    wheelTo(x, 1400 - d);
    assert.strictEqual(x.s.pinned(), true, "up by " + d);
  }
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
// A log that grows taller at the end (a sidebar that opens, a keyboard
// that closes) right after a swipe lowers scrollTop: still at the end.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  x.s.heard(3000);
  x.log.client = 900;
  x.log.clamp();
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), true);
}
// Another chat in the log: the swipe heard on the old one does not make
// the clamp of the emptied log a scroll up, and the reader is at the end.
{
  const x = make([500, 500, 500, 500], 600);
  wheelTo(x, 200);
  assert.strictEqual(x.s.pinned(), false);
  x.s.heard(3000);
  x.s.reset();
  assert.strictEqual(x.s.pinned(), true);
  x.log.heights = [100];
  x.log.clamp();
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), true, "the emptied log's clamp is not the reader");
  x.log.heights = [500, 500, 500];
  x.s.sync();
  assert.strictEqual(x.log.scrollTop, 900);
}
// A chat just opened: a scroll up that nobody announced and the layout
// did not cause (the browser still moving the log for the chat shown
// before) does not take the reader off its end; once the chat is drawn
// and a moment has passed, or once the reader acts, scrolls are the
// reader's again.
{
  const x = make([500, 500, 500, 500], 600);
  x.s.pin();
  x.tick(5000);
  x.s.reset();
  x.log.heights = [500, 500, 500, 500, 500];
  x.s.sync();
  x.s.drawn();
  x.tick(16);
  x.log.scrollTop = 900; // the fling of the chat before goes on
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), true, "an unannounced scroll right after the open is not the reader");
  assert.strictEqual(x.log.scrollTop, 1900, "the log is back at its end");
  assert.strictEqual(x.s.settled(), false, "a scroll now would not be the reader's");
  x.tick(1000);
  assert.strictEqual(x.s.settled(), true);
  x.log.scrollTop = 900; // Ctrl+F found older text
  x.s.scrolled();
  assert.strictEqual(x.s.pinned(), false, "later a find is the reader's");
  x.s.reset();
  x.log.heights = [500, 500, 500, 500, 500];
  x.s.sync();
  wheelTo(x, 700);
  assert.strictEqual(x.s.pinned(), false, "the reader's own wheel right after the open counts");
  assert.strictEqual(x.s.acted(), true);
}

// Writes that stream in put a pinned log at its end once, after the last write, and a log
// whose reader moved away is left where it is. Scroll events of one frame count once.
{
  const rafs = [];
  const log = new Log([500, 500, 500], 600);
  let reads = 0;
  Object.defineProperty(log, "scrollHeight", { get() { reads++; return this.heights.reduce((a, b) => a + b, 0); } });
  const s = HubStick(log, { probe: () => null, place: () => null, now: () => 0, raf: (f) => rafs.push(f) });
  s.pin();
  reads = 0;
  for (let i = 0; i < 50; i++) { log.heights[2] += 10; s.pinSoon(); }
  assert.strictEqual(reads, 0, "no layout read after a streamed write");
  assert.strictEqual(rafs.length, 1, "fifty writes queue one pin");
  rafs.shift()();
  assert.strictEqual(log.scrollTop, log.heights.reduce((a, b) => a + b, 0) - 600, "the log is at its end after the writes");
  assert.ok(reads < 6, "one pin read the layout a few times, not per write: " + reads);
  s.release();
  const top = log.scrollTop = 300;
  log.heights[2] += 100;
  s.pinSoon();
  assert.strictEqual(rafs.length, 0, "a released log queues no pin");
  assert.strictEqual(log.scrollTop, top, "a released log is not moved by a streamed write");
  reads = 0;
  for (let i = 0; i < 20; i++) s.scrollSoon();
  assert.strictEqual(rafs.length, 1, "twenty scroll events of a frame queue one pass");
  assert.strictEqual(reads, 0, "no read before the frame");
}

// A finger drags a log at its end up a few pixels a frame while a reply
// streams: no frame puts it back at the end, so it leaves the end; a drag
// that stays within reach of the end is put there once the finger and its
// fling are done.
{
  const rafs = [];
  const log = new Log([500, 500, 500], 600);
  let t = 0;
  const s = HubStick(log, { probe: () => null, place: () => null, now: () => t, raf: (f) => rafs.push(f) });
  const run = () => { const q = rafs.splice(0); for (const f of q) f(); };
  s.pin();
  t = 5000;
  s.drag(true);
  s.heard(8000);
  let last = log.scrollTop;
  for (let i = 0; i < 20; i++) {
    log.scrollTop = log.scrollTop - 4;
    s.scrollSoon();
    log.heights[2] += 10;
    s.pinSoon();
    s.sync();
    run();
    t += 16;
    assert.ok(log.scrollTop <= last, "the drag was undone at frame " + i);
    last = log.scrollTop;
  }
  assert.strictEqual(s.pinned(), false, "eighty pixels up leave the end");
  s.drag(false);
  t += 500;
  run();
  assert.strictEqual(log.scrollTop, last, "a log that left the end stays after the finger lifts");
  // Back at the end, and a small drag that stays within reach of it.
  s.pin();
  s.drag(true);
  log.scrollTop -= 6;
  s.scrollSoon();
  log.heights[2] += 10;
  s.pinSoon();
  run();
  assert.ok(log.scrollHeight - log.scrollTop - log.clientHeight >= 16, "no pin under the finger");
  s.drag(false);
  t += 100;
  log.scrollTop -= 2; // the fling's last event
  s.scrollSoon();
  run();
  assert.ok(log.scrollHeight - log.scrollTop - log.clientHeight > 1, "no pin during the fling");
  t += 200;
  s.pinSoon();
  run();
  run();
  assert.strictEqual(log.scrollTop, log.scrollHeight - log.clientHeight, "the log is at its end once the fling is over");
  assert.strictEqual(s.pinned(), true);
}
console.log("ok");
})();
