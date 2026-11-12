"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const [script, cases] = process.argv.slice(2);
const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(script, "utf8"), sandbox);
const { derive, memory } = sandbox.window.HubToolCard;
for (const c of JSON.parse(fs.readFileSync(cases, "utf8"))) {
  const mem = memory();
  for (const l of c.learn || []) mem.learn(l.name, l.args, l.res);
  const call = Object.assign({ done: true, failed: false, out: "", mem }, c.call);
  if (call.res === undefined) call.res = null;
  const got = derive(call);
  const seen = { icon: got.icon, title: got.title, status: got.status, state: got.state };
  if (c.blocks) seen.blocks = got.blocks.map((b) => b.label);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(seen)), c.want, c.name);
}

const { wrapped, take } = sandbox.window.HubToolCard;
const batch = wrapped("tool_call", { calls: [{ name: "process_manage", arguments: { action: "kill" } }, { name: "process_manage", arguments: { action: "list" } }, { name: "terminal", arguments: { command: "ls" } }, 5] });
assert.strictEqual(batch.length, 3);
assert.deepStrictEqual(JSON.parse(JSON.stringify(take(batch, "process_manage"))), { name: "process_manage", args: { action: "kill" } });
assert.deepStrictEqual(JSON.parse(JSON.stringify(take(batch, "process_manage"))), { name: "process_manage", args: { action: "list" } });
assert.strictEqual(take(batch, "process_manage"), null);
assert.strictEqual(wrapped("terminal", { calls: [] }), null);
assert.strictEqual(wrapped("tool_call", "x"), null);
