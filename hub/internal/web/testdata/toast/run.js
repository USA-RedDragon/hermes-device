"use strict";
// Runs the toast model of toast.js with a fake clock: node run.js toast.js
// Timing (4 s + 60 ms a word, double for an error), the name clamp, at most
// 3 shown with the oldest dropped first, identical text refreshing instead
// of stacking, and pause on hover or focus with 2 s at least afterwards.
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {}, Array, String, Math, Date, setTimeout, clearTimeout };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const T = sandbox.window.HubToast;

// duration
assert.strictEqual(T.duration("Copied", false), 4060);
assert.strictEqual(T.duration("one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty", false), 5200);
assert.strictEqual(T.duration("Could not send: no", true), (4000 + 60 * 4) * 2);
assert.strictEqual(T.duration("  spaced   out  ", false), 4000 + 60 * 2);

// clamp
assert.strictEqual(T.clamp("short"), "short");
assert.strictEqual(T.clamp("a".repeat(40)), "a".repeat(40));
const cut = T.clamp("a".repeat(41));
assert.strictEqual(Array.from(cut).length, 40);
assert.ok(cut.endsWith("\u2026"));
assert.strictEqual(T.clamp("Build  the\n first\tthing"), "Build the first thing");
assert.strictEqual(Array.from(T.clamp("\u{1F600}".repeat(50))).length, 40);
assert.ok(!T.clamp("word ".repeat(20)).includes(" \u2026"), "no space before the ellipsis");

// the stack, on a clock the test moves
let now = 0;
let seq = 0;
const timers = new Map();
const log = [];
const env = {
  now: () => now,
  set: (fn, ms) => { const id = ++seq; timers.set(id, { fn, at: now + ms }); return id; },
  clear: (id) => { timers.delete(id); },
  change: (kind, it) => log.push(kind + ":" + it.text),
};
const advance = (ms) => {
  const end = now + ms;
  for (;;) {
    let next = null;
    for (const [id, t] of timers) if (t.at <= end && (!next || t.at < next[1].at)) next = [id, t];
    if (!next) break;
    now = next[1].at;
    timers.delete(next[0]);
    next[1].fn();
  }
  now = end;
};
const texts = (s) => [...s.items].map((i) => i.text);

{
  const s = T.stack(env);
  s.push({ text: "Copied" });
  assert.deepStrictEqual(texts(s), ["Copied"]);
  advance(4059);
  assert.deepStrictEqual(texts(s), ["Copied"], "still there just before its time");
  advance(1);
  assert.deepStrictEqual(texts(s), [], "gone at 4 s + 60 ms");
  assert.strictEqual(timers.size, 0);
}

{
  // newest on top, at most 3, oldest dropped first
  const s = T.stack(env);
  for (const t of ["one", "two", "three", "four"]) s.push({ text: t });
  assert.deepStrictEqual(texts(s), ["four", "three", "two"]);
  assert.ok(log.includes("remove:one"));
  assert.strictEqual(timers.size, 3, "a dropped toast leaves no timer");
  s.items.slice().forEach((i) => s.close(i));
  assert.strictEqual(timers.size, 0);
}

{
  // identical text refreshes: no copy, the clock starts again, to the top it does not move
  const s = T.stack(env);
  const a = s.push({ text: "Copied" });
  advance(3000);
  s.push({ text: "Other" });
  const again = s.push({ text: "Copied" });
  assert.strictEqual(again, a);
  assert.deepStrictEqual(texts(s), ["Other", "Copied"], "no second Copied");
  advance(3000);
  assert.ok(texts(s).includes("Copied"), "refreshed at 3 s, still there at 6 s");
  advance(1100);
  assert.ok(!texts(s).includes("Copied"));
  // an error and a plain toast with the same words are not the same toast
  const t = T.stack(env);
  t.push({ text: "Same" });
  t.push({ text: "Same", bad: true });
  assert.strictEqual(t.items.length, 2);
  t.items.slice().forEach((i) => t.close(i));
}

{
  // an error stays twice as long
  const s = T.stack(env);
  s.push({ text: "Could not send: no", bad: true });
  advance(8000);
  assert.strictEqual(s.items.length, 1);
  advance(500);
  assert.strictEqual(s.items.length, 0);
}

{
  // hover pauses; after it the toast stays at least 2 s
  const s = T.stack(env);
  const a = s.push({ text: "Copied" });
  advance(3500);
  s.hold(a);
  advance(60000);
  assert.strictEqual(s.items.length, 1, "held for a minute");
  s.release(a);
  advance(1999);
  assert.strictEqual(s.items.length, 1, "2 s of grace after the pointer left");
  advance(1);
  assert.strictEqual(s.items.length, 0);

  // with more than 2 s left, the rest of the time is kept
  const b = s.push({ text: "Copied" });
  advance(500);
  s.hold(b);
  advance(10000);
  s.release(b);
  advance(3559);
  assert.strictEqual(s.items.length, 1);
  advance(1);
  assert.strictEqual(s.items.length, 0);

  // hover and focus both count; the clock starts when the last one lets go
  const c = s.push({ text: "Copied" });
  s.hold(c);
  s.hold(c);
  s.release(c);
  advance(60000);
  assert.strictEqual(s.items.length, 1, "still focused");
  s.release(c);
  advance(4060);
  assert.strictEqual(s.items.length, 0);

  // a refresh while held waits for the release
  const d = s.push({ text: "Copied" });
  s.hold(d);
  s.push({ text: "Copied" });
  advance(60000);
  assert.strictEqual(s.items.length, 1);
  s.release(d);
  advance(4060);
  assert.strictEqual(s.items.length, 0);
}

{
  // closing by hand and by a late timer
  const s = T.stack(env);
  const a = s.push({ text: "Copied" });
  s.close(a);
  s.close(a);
  assert.strictEqual(s.items.length, 0);
  assert.strictEqual(timers.size, 0);
}
console.log("ok");
