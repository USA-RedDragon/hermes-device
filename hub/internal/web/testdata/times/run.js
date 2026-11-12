"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const script = process.argv[2];

const ticks = [];
const node = () => ({
  dataset: {},
  children: [],
  isConnected: true,
  title: "",
  _text: "",
  set textContent(v) { this._text = v; this.children = []; },
  get textContent() { return this._text + this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join(""); },
  replaceChildren(...k) { this._text = ""; this.children = k; },
  removeAttribute(n) { if (n === "title") this.title = ""; },
});
const sandbox = {
  window: {},
  document: { createElement: node },
  setInterval: (fn) => { ticks.push(fn); return ticks.length; },
  clearInterval: () => {},
  clearTimeout: () => {},
  setTimeout: () => 0,
};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(script, "utf8"), sandbox);
const T = sandbox.window.HubTimes;
let nowMs = 0;
vm.runInContext("Date.now = () => globalThis.__now", sandbox);
const setNow = (ms) => { nowMs = ms; sandbox.__now = ms; };

const chicago = "America/Chicago";
const at = (iso) => Date.parse(iso);
const now = at("2026-11-12T22:30:00Z");

assert.strictEqual(T.clock(at("2026-11-12T20:14:07Z"), now, chicago), "2:14 PM");
assert.strictEqual(T.clock(at("2026-11-12T06:00:00Z"), now, chicago), "12:00 AM");
assert.strictEqual(T.clock(at("2026-11-12T18:00:00Z"), now, chicago), "12:00 PM");
assert.strictEqual(T.clock(at("2026-11-12T22:30:30Z"), now, chicago), "4:30 PM", "a time a little ahead of the clock still reads as today");
assert.strictEqual(T.clock(at("2026-11-12T05:52:00Z"), now, chicago), "Wed Nov 11, 11:52 PM");
assert.strictEqual(T.clock(at("2026-11-12T05:59:59Z"), now, chicago), "Wed Nov 11, 11:59 PM");
assert.strictEqual(T.clock(at("2026-11-12T06:00:00Z"), now, chicago), "12:00 AM");
assert.strictEqual(T.clock(at("2025-12-31T18:00:00Z"), now, chicago), "Wed Dec 31 2025, 12:00 PM");

assert.strictEqual(T.clock(at("2026-11-12T05:52:00Z"), now, "UTC"), "5:52 AM");
assert.strictEqual(T.clock(at("2026-11-12T05:52:00Z"), now, "Asia/Tokyo"), "Thu Nov 12, 2:52 PM", "it is already Nov 13 in Tokyo");
assert.strictEqual(T.clock(at("2026-11-12T22:00:00Z"), now, "Asia/Tokyo"), "7:00 AM");
assert.strictEqual(T.clock(at("2026-11-11T20:00:00Z"), now, "Asia/Tokyo"), "Thu Nov 12, 5:00 AM", "the day is the day in the zone, not in UTC");
assert.strictEqual(T.clock(at("2026-11-11T14:00:00Z"), now, "Asia/Tokyo"), "Wed Nov 11, 11:00 PM");

assert.strictEqual(T.range(at("2026-11-12T20:14:07Z"), at("2026-11-12T20:15:40Z"), now, chicago), "2:14 to 2:15 PM");
assert.strictEqual(T.range(at("2026-11-12T20:14:07Z"), at("2026-11-12T20:14:50Z"), now, chicago), "2:14 PM");
assert.strictEqual(T.range(at("2026-11-12T17:50:00Z"), at("2026-11-12T20:15:40Z"), now, chicago), "11:50 AM to 2:15 PM");
assert.strictEqual(T.range(at("2026-11-12T05:52:00Z"), at("2026-11-12T05:55:00Z"), now, chicago), "Wed Nov 11, 11:52 to 11:55 PM");
assert.strictEqual(T.range(at("2026-11-12T05:52:00Z"), at("2026-11-12T20:15:00Z"), now, chicago), "Wed Nov 11, 11:52 PM to 2:15 PM");
assert.strictEqual(T.range(0, at("2026-11-12T20:15:00Z"), now, chicago), "2:15 PM");
assert.strictEqual(T.range(0, 0, now, chicago), "");

assert.strictEqual(T.full(at("2026-11-12T20:14:07Z"), chicago), "Thu Nov 12 2026, 2:14:07 PM CST");
assert.strictEqual(T.full(at("2026-07-04T20:14:07Z"), chicago), "Sat Jul 4 2026, 3:14:07 PM CDT");
assert.strictEqual(T.full(at("2026-11-12T20:14:07Z"), "UTC"), "Thu Nov 12 2026, 8:14:07 PM UTC");

for (const bad of [0, -1, NaN, undefined, null, "x"]) {
  assert.strictEqual(T.clock(bad, now, chicago), "");
  assert.strictEqual(T.full(bad, chicago), "");
}

assert.strictEqual(T.span(0.329), "329 ms");
assert.strictEqual(T.span(0.0001), "1 ms");
assert.strictEqual(T.span(3.24), "3.2 s");
assert.strictEqual(T.span(12.4), "12 s");
assert.strictEqual(T.span(240), "4 min");
assert.strictEqual(T.span(245), "4 min 5 s");
assert.strictEqual(T.span(2280), "38 min");
assert.strictEqual(T.span(3780), "1 h 3 min");
assert.strictEqual(T.span(7200), "2 h");
assert.strictEqual(T.span(-1), "");
assert.strictEqual(T.span(undefined), "");

assert.strictEqual(T.counter(0), "0:00");
assert.strictEqual(T.counter(192000), "3:12");
assert.strictEqual(T.counter(59999), "0:59");
assert.strictEqual(T.counter(3792000), "1:03:12");
assert.strictEqual(T.counter(-5000), "0:00");

const done = node();
T.finished(done, at("2026-11-12T20:14:07Z"), 0.329);
assert.strictEqual(done.children.length, 3);
assert.strictEqual(done.children[2], "329 ms");
assert.strictEqual(done.children[0].dataset.at, String(at("2026-11-12T20:14:07Z")));
assert.ok(done.children[0].title.endsWith("PM " + done.children[0].title.split(" ").pop()), done.children[0].title);
const onlyTime = node();
T.finished(onlyTime, at("2026-11-12T20:14:07Z"), undefined, "Failed");
assert.strictEqual(onlyTime.children[0], "Failed");
assert.strictEqual(onlyTime.children.length, 3);
const nothing = node();
T.finished(nothing, 0, undefined);
assert.strictEqual(nothing.children.length, 0, "no time and no length shows nothing");
const lengthOnly = node();
T.finished(lengthOnly, 0, 4);
assert.deepStrictEqual(lengthOnly.children, ["4.0 s"]);

const started = now - 192000;
setNow(now);
const run = node();
T.running(run, started);
assert.strictEqual(run.children[0], "started ");
assert.strictEqual(run.children[2], " · running ");
const counter = run.children[3];
assert.strictEqual(counter.textContent, "3:12");
assert.strictEqual(ticks.length, 1);
setNow(now + 5000);
ticks[0]();
assert.strictEqual(counter.textContent, "3:17", "the counter follows the clock");
counter.isConnected = false;
setNow(now + 9000);
ticks[0]();
assert.strictEqual(counter.textContent, "3:17", "a counter that left the page stops");

const unknown = node();
T.running(unknown, 0);
assert.strictEqual(unknown.children.length, 0, "a start that is not known shows nothing");
