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

// The status line under a running subagent: a plain sentence and an icon, in
// the words of the cards, never the raw text.
{
  const { statusLine } = sandbox.window.HubToolCard;
  const mem = memory();
  mem.learn("terminal", { command: "npm test", background: true }, { session_id: "proc_283637f9d51" });
  const say = (n, p, m) => { const l = statusLine(n, p, m); return l.icon + "|" + l.text; };
  assert.strictEqual(say("process_manage", "wait proc_283637f9d51 180s"), "clock|Waiting on a background command");
  assert.strictEqual(say("process_manage", "wait proc_283637f9d51 180s", mem), "clock|Waiting on background command npm test");
  assert.strictEqual(say("process_manage", "kill proc_283637f9d51", mem), "stop|Stopping background command npm test");
  assert.strictEqual(say("process_manage", "list"), "list|Listing background commands");
  assert.strictEqual(say("terminal", "npm test"), "terminal|Running npm test");
  assert.strictEqual(say("read_file", "/srv/app/hub/web/static/app.js"), "file|Reading app.js");
  assert.strictEqual(say("session_search", "dns split horizon"), "search|Searching past chats for dns split horizon");
  assert.strictEqual(say("browser_vault", "list"), "key|Looking for a saved login");
  assert.strictEqual(say("mcp_device_call", "github.create_issue"), "plug|Calling github.create_issue on your computer");
  assert.strictEqual(say("tool_call", "3 calls"), "tool|Running 3 tools");
  assert.strictEqual(say("todo_list", ""), "checkcircle|Updating the to-do list");
  assert.strictEqual(say("tool_search", "gmail"), "search|Looking up a tool");
  assert.strictEqual(say("browser_navigate", "https://example.com/a"), "globe|Opening example.com");
  assert.strictEqual(say("some_new_tool", "{...}"), "tool|Using some_new_tool");
  assert.strictEqual(say("", ""), "tool|Working");
  // Whatever the tool, the raw preview is not the sentence.
  assert.ok(!say("some_new_tool", "{...}").includes("{...}"));
}
// The vault's call is shown with its secrets hidden, and its output not at all.
{
  const d = derive({ name: "browser_vault", args: { action: "fill", password: "hunter2", url: "https://x.com" }, res: { password: "hunter2" }, out: "hunter2", done: true, failed: false });
  assert.ok(!JSON.stringify(d.blocks).includes("hunter2"), JSON.stringify(d.blocks));
  assert.deepStrictEqual(JSON.parse(JSON.stringify(d.blocks.map((b) => b.label))), ["Call"]);
}

// A live tool_call (the stream sends one start and one end for all of its
// calls) draws a card for each call, not one wrench, and splits its result.
{
  const { calls, split } = sandbox.window.HubToolCard;
  const got = JSON.parse(JSON.stringify(calls("t9", "tool_call", { calls: [{ name: "terminal", arguments: { command: "ls" } }, { name: "session_search", arguments: { query: "x" } }] })));
  assert.deepStrictEqual(got, [
    { key: "t9:0", name: "terminal", args: { command: "ls" }, batch: "t9" },
    { key: "t9:1", name: "session_search", args: { query: "x" }, batch: "t9" },
  ]);
  assert.strictEqual(calls("t9", "tool_call", { calls: [{ name: "terminal", arguments: {} }] })[0].batch, "");
  assert.strictEqual(calls("t9", "terminal", { command: "ls" }), null);
  assert.strictEqual(calls("t9", "tool_call", { calls: [] }), null);
  assert.strictEqual(JSON.stringify(split('[{"a":1},{"b":2}]', 2)), '[{"a":1},{"b":2}]');
  assert.strictEqual(JSON.stringify(split({ results: [1, 2, 3] }, 3)), "[1,2,3]");
  assert.strictEqual(split("[1]", 2), null);
  assert.strictEqual(split("not json", 2), null);
  assert.strictEqual(split(undefined, 2), null);
}
