"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const S = sandbox.window.HubSidebars;

function make(width, saved, shown) {
  const store = Object.assign({}, saved || {});
  const sides = shown || { left: true, right: true };
  const c = S.create({
    width: () => width,
    shown: (s) => sides[s],
    storage: { getItem: (k) => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = v; } },
  });
  return { c, store, sides };
}
const eff = (c) => JSON.parse(JSON.stringify(c.eff()));

// Defaults, and the limits on a wide window.
{
  const { c } = make(1440);
  assert.deepStrictEqual(eff(c), { left: 272, right: 352 });
  assert.deepStrictEqual(JSON.parse(JSON.stringify(c.limits("left"))), [192, 448]);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(c.limits("right"))), [288, 640]);
}

// Clamping to the side's own range.
{
  const { c } = make(1440);
  c.set("left", 10); assert.strictEqual(c.eff().left, 192);
  c.set("left", 9999); assert.strictEqual(c.eff().left, 448);
  c.set("right", 10); assert.strictEqual(c.eff().right, 288);
  c.set("right", 9999); assert.strictEqual(c.eff().right, 608); // 1440 - 448 - 384
  c.set("left", 272);
  c.set("right", 9999); assert.strictEqual(c.eff().right, 640);
  c.drag("left", 300.4); assert.strictEqual(c.eff().left, 300);
}

// The chat column keeps 384 px: 1000 - 384 = 616 for both sides.
{
  const { c } = make(1000);
  c.set("right", 640);
  assert.ok(c.eff().left + c.eff().right <= 616);
  c.set("left", 448);
  assert.ok(c.eff().left + c.eff().right <= 616);
  assert.ok(c.eff().left >= 192 && c.eff().right >= 288);
  // The limits never fall under the minimum, even in a window too narrow for both.
  const n = make(700).c;
  assert.deepStrictEqual(JSON.parse(JSON.stringify(n.limits("left"))), [192, 192]);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(n.limits("right"))), [288, 288]);
}

// A side that is not beside the chat does not take room from the other.
{
  const { c } = make(900, null, { left: true, right: false });
  assert.deepStrictEqual(JSON.parse(JSON.stringify(c.limits("left"))), [192, 448]);
}

// A saved width that is too wide for a narrow window is held, not lost.
{
  const { c, store } = make(1000, { "hub.width.left": "448", "hub.width.right": "640" });
  assert.strictEqual(c.widths.left, 448);
  assert.ok(c.eff().left + c.eff().right <= 616);
  assert.strictEqual(store["hub.width.left"], "448");
  const wide = make(1800, { "hub.width.left": "448", "hub.width.right": "640" }).c;
  assert.deepStrictEqual(eff(wide), { left: 448, right: 640 });
}

// Persistence: reset and keys save, bad saved values are ignored.
{
  const { c, store } = make(1440);
  c.set("left", 330); c.save("left");
  assert.strictEqual(store["hub.width.left"], "330");
  c.reset("left");
  assert.strictEqual(store["hub.width.left"], "272");
  assert.strictEqual(c.eff().left, 272);
  const again = make(1440, store).c;
  assert.strictEqual(again.eff().left, 272);
  const bad = make(1440, { "hub.width.left": "abc", "hub.width.right": "5" }).c;
  assert.deepStrictEqual(eff(bad), { left: 272, right: 352 });
  const out = make(1440, { "hub.width.left": "9000", "hub.width.right": "-3" }).c;
  assert.deepStrictEqual(eff(out), { left: 272, right: 352 });
  // Storage that throws does not break it.
  const broken = S.create({ width: () => 1440, shown: () => true, storage: { getItem() { throw new Error("no"); }, setItem() { throw new Error("no"); } } });
  broken.key("left", "ArrowRight", false);
  assert.strictEqual(broken.eff().left, 288);
}

// Keyboard.
{
  const { c, store } = make(1440);
  assert.strictEqual(c.key("left", "ArrowRight", false), true); assert.strictEqual(c.eff().left, 288);
  c.key("left", "ArrowRight", true); assert.strictEqual(c.eff().left, 352);
  c.key("left", "ArrowLeft", false); assert.strictEqual(c.eff().left, 336);
  c.key("left", "Home", false); assert.strictEqual(c.eff().left, 192);
  c.key("left", "End", false); assert.strictEqual(c.eff().left, 448);
  c.key("left", "Enter", false); assert.strictEqual(c.eff().left, 272);
  assert.strictEqual(store["hub.width.left"], "272");
  // The right handle grows to the left: ArrowLeft widens it.
  c.key("right", "ArrowLeft", false); assert.strictEqual(c.eff().right, 368);
  c.key("right", "ArrowRight", true); assert.strictEqual(c.eff().right, 304);
  c.key("right", "Home", false); assert.strictEqual(c.eff().right, 288);
  c.key("right", "End", false); assert.strictEqual(c.eff().right, 640);
  assert.strictEqual(store["hub.width.right"], "640");
  c.key("right", "Enter", false); assert.strictEqual(c.eff().right, 352);
  // Other keys are left alone and nothing is saved for them.
  const before = JSON.stringify(store);
  assert.strictEqual(c.key("left", "a", false), false);
  assert.strictEqual(JSON.stringify(store), before);
  // Steps stop at the limits.
  c.key("left", "Home", false); c.key("left", "ArrowLeft", true); assert.strictEqual(c.eff().left, 192);
  // End on a tight window.
  const t = make(1000).c;
  t.key("right", "End", false);
  assert.ok(t.eff().left + t.eff().right <= 616);
  assert.strictEqual(t.eff().right, t.limits("right")[1]);
}
console.log("ok");
