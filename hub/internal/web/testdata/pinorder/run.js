"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const raw = sandbox.window.HubPinOrder.plan;
const plan = (...a) => JSON.parse(JSON.stringify(raw(...a)));
const pins = ["a", "b", "c"];

// A Recent row dropped on Pinned is pinned at the place it was dropped.
assert.deepStrictEqual(plan("recent", "pinned", 0, "x", pins), { kind: "pin", order: ["x", "a", "b", "c"], index: 0 });
assert.deepStrictEqual(plan("recent", "pinned", 2, "x", pins), { kind: "pin", order: ["a", "b", "x", "c"], index: 2 });
assert.deepStrictEqual(plan("recent", "pinned", 3, "x", pins), { kind: "pin", order: ["a", "b", "c", "x"], index: 3 });
assert.deepStrictEqual(plan("recent", "pinned", 9, "x", pins), { kind: "pin", order: ["a", "b", "c", "x"], index: 3 });
assert.deepStrictEqual(plan("recent", "pinned", 0, "x", []), { kind: "pin", order: ["x"], index: 0 });

// A pinned row dropped on Recent is unpinned.
assert.deepStrictEqual(plan("pinned", "recent", 1, "b", pins), { kind: "unpin" });

// A pinned row dropped on Pinned moves, or does nothing at its own place.
assert.deepStrictEqual(plan("pinned", "pinned", 2, "a", pins), { kind: "reorder", order: ["b", "c", "a"], index: 2 });
assert.deepStrictEqual(plan("pinned", "pinned", 0, "c", pins), { kind: "reorder", order: ["c", "a", "b"], index: 0 });
assert.deepStrictEqual(plan("pinned", "pinned", 1, "b", pins), { kind: "none" });

// Anywhere else cancels, and a Recent row dropped on Recent does nothing.
assert.deepStrictEqual(plan("recent", null, 1, "x", pins), { kind: "none" });
assert.deepStrictEqual(plan("pinned", null, 1, "b", pins), { kind: "none" });
assert.deepStrictEqual(plan("recent", "recent", 1, "x", pins), { kind: "none" });
console.log("ok");
