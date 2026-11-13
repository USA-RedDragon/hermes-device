"use strict";
// The subagent's steps as a small chat: the pure parts of minichat.js and
// toolgroup.js, then a view drawn into a small fake DOM: what it shows, how
// calls fold into the chat's groups, when it follows new steps and when
// the pill counts them, and the state of the messages the user sent.
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const { el, document } = require("../fakedom.js");
const sandbox = { window: {}, document, setInterval: () => 1, clearInterval: () => {}, Date, Math, JSON, Intl };
sandbox.window.HubIcons = { icon: (name) => el("svg", "ico i-" + name), button: (name, label, cls) => el("button", cls) };
vm.createContext(sandbox);
for (const f of process.argv.slice(2)) vm.runInContext(fs.readFileSync(f, "utf8"), sandbox);
const { HubMiniChat: M, HubToolGroup: G } = sandbox.window;

const T0 = Date.UTC(2026, 10, 12, 22, 0, 0);
const iso = (s) => new Date(T0 + s * 1000).toISOString();
let seq = 0;
const ev = (o) => Object.assign({ seq: ++seq }, o, { ts: iso(o.at) });

// Pure parts.
assert.strictEqual(M.pill(1), "1 new step");
assert.strictEqual(M.pill(3), "3 new steps");
{
  const box = { scrollHeight: 1000, clientHeight: 200, scrollTop: 760 };
  assert.ok(M.near(box));
  box.scrollTop = 700;
  assert.ok(!M.near(box));
}
{
  const a = ev({ type: "message", at: 5 });
  const b = ev({ type: "steer", at: 2 });
  assert.deepStrictEqual(M.order([a, b]).map((e) => e.seq), [b.seq, a.seq]);
  const steer = ev({ type: "steer", at: 10 });
  assert.strictEqual(M.steerState(steer, [steer], false), "waits");
  assert.strictEqual(M.steerState(steer, [steer, ev({ type: "tool_end", at: 9 })], false), "waits");
  assert.strictEqual(M.steerState(steer, [steer, ev({ type: "tool_end", at: 11 })], false), "read");
  assert.strictEqual(M.steerState(steer, [steer, ev({ type: "message", at: 12 })], true), "missed");
  // A result of a call already shown adds nothing to read.
  const evs = [ev({ type: "tool_end", call_id: "x", at: 1 }), ev({ type: "tool_start", call_id: "y", at: 2 }), ev({ type: "tool_end", call_id: "y", at: 3 }), ev({ type: "message", at: 4 })];
  assert.strictEqual(M.steps(evs, ["x"]), 2);
  assert.strictEqual(M.end({ state: "ok", durationS: 1300, result: "Added it." }).head, "Done in 21 min 40 s");
  assert.strictEqual(M.end({ state: "bad", durationS: 723 }).head, "Failed after 12 min 3 s");
  assert.ok(M.end({ state: "bad" }).bad && !M.end({ state: "ok" }).bad);
}
// The group line uses the chat's words and total time.
{
  const card = (toolName, duration, failed) => ({ toolName, duration, failed, call: {} });
  const s = G.summary([card("terminal", 12.4), card("read_file", 0.1), card("terminal", 100.3, true)]);
  assert.strictEqual(s.text, "Ran 2 commands, read a file (1 min 53 s)");
  assert.strictEqual(s.failed, 1);
  assert.strictEqual(G.summary([card("browser_click"), card("my_tool")]).text, "Used the browser, used my_tool");
}

// The view.
const v = M.create({ el, toast: () => {} });
const box = v.node;
const kids = () => box.children.map((n) => n.className.split(" ")[0] + (n.classList.contains("running") ? ".running" : ""));
v.set({ model: "claude-opus-5-5" });
v.add([
  ev({ type: "goal", text: "Count the rows", at: 0 }),
  ev({ type: "thinking", text: "I need the log.", at: 1 }),
  ev({ type: "message", text: "I'll capture the screen.", at: 2 }),
  ev({ type: "tool_start", call_id: "c1", name: "terminal", args: { command: "hermes model > pick.log" }, at: 2 }),
  ev({ type: "tool_end", call_id: "c1", name: "terminal", result: '{"output": "", "exit_code": 0}', result_size: 30, duration_ms: 2500, at: 5 }),
  ev({ type: "tool_start", call_id: "c2", name: "read_file", args: { path: "a.py" }, at: 6 }),
  ev({ type: "tool_end", call_id: "c2", name: "read_file", result: "def clean(buf): ...", result_size: 9000, at: 6.1 }),
  ev({ type: "message", text: "The filter keeps duplicates.", at: 7 }),
  ev({ type: "tool_start", call_id: "c3", name: "terminal", args: { command: "grep -c x pick.log" }, at: 8 }),
  ev({ type: "tool_end", call_id: "c3", name: "terminal", result: '{"output": "0", "exit_code": 1}', result_size: 31, duration_ms: 100300, at: 108 }),
  ev({ type: "tool_start", call_id: "c4", name: "terminal", args: { command: "sed -i s/a/b/ x" }, at: 109 }),
]);
assert.ok(box.textContent.startsWith("Started "), box.textContent);
assert.ok(box.children[0].textContent.endsWith(" · claude-opus-5-5"));
// Finished calls before newer text fold into a group with the chat's
// words; the newest calls of a running subagent stay open as cards, the
// failed one red, the running one running.
assert.deepStrictEqual(kids(), ["note", "reasoning", "msg", "toolgroup", "msg", "tool", "tool.running", "jump"]);
const group = box.querySelector("details.toolgroup");
assert.strictEqual(group.firstChild.textContent.split(" \u00B7 ")[0], "Ran a command, read a file (2.6 s)");
assert.strictEqual(group.querySelectorAll(".tool").length, 2);
const failed = box.children[5];
assert.ok(failed.classList.contains("failed"), failed.className);
assert.strictEqual(failed.dataset.state, "bad");
// A cut result says how long it was.
const readCard = group.querySelectorAll(".tool")[1];
assert.ok(readCard.call.out.endsWith("(9000 characters in all)"), readCard.call.out);
// It started at the end, so it followed.
assert.strictEqual(box.scrollTop, box.scrollHeight - box.clientHeight);
assert.ok(box.querySelector(".jump").hidden);

// The running call ends and a reply comes: the reader at the end follows.
v.add([ev({ type: "tool_end", call_id: "c4", name: "terminal", result: '{"output": "", "exit_code": 0}', duration_ms: 400, at: 110 }),
  ev({ type: "message", text: "Done with the filter.", at: 111 })]);
assert.deepStrictEqual(kids(), ["note", "reasoning", "msg", "toolgroup", "msg", "toolgroup", "msg", "jump"]);
assert.match(box.children[5].firstChild.textContent, /^Ran 2 commands \(1 min 41 s\)/);
assert.match(box.children[5].textContent, /1 step failed/);
assert.strictEqual(box.scrollTop, box.scrollHeight - box.clientHeight);

// Scrolled up, new steps stay below and the pill counts them.
box.scrollTop = 0;
box.dispatch("scroll");
v.add([ev({ type: "message", text: "Row count is 14.", at: 120 }), ev({ type: "tool_start", call_id: "c5", name: "terminal", args: { command: "wc -l" }, at: 121 })]);
const jump = box.querySelector(".jump");
assert.strictEqual(box.scrollTop, 0);
assert.ok(!jump.hidden);
assert.strictEqual(jump.lastChild.textContent, "2 new steps");
// The result of a call already shown is not a new step.
v.add([ev({ type: "tool_end", call_id: "c5", name: "terminal", result: '{"output": "14", "exit_code": 0}', at: 122 })]);
assert.strictEqual(jump.lastChild.textContent, "2 new steps");
// Back at the end, the pill goes.
box.scrollTop = box.scrollHeight;
box.dispatch("scroll");
assert.ok(jump.hidden);
// Or the pill takes the reader there.
box.scrollTop = 0;
v.add([ev({ type: "message", text: "One more.", at: 130 })]);
assert.strictEqual(jump.lastChild.textContent, "1 new step");
jump.onclick();
assert.ok(jump.hidden);
assert.strictEqual(box.scrollTop, box.scrollHeight - box.clientHeight);

// A message the user sent waits for the subagent's next step, and reads as
// read once a tool result comes after it.
v.add([ev({ type: "steer", text: "Also print the count.", at: 140 })]);
const steerLine = () => box.querySelectorAll(".queued").pop().textContent;
assert.match(steerLine(), / · waits for its next step$/);
assert.ok(box.querySelectorAll(".msg.user.sm").pop().classList.contains("pending"));
v.add([ev({ type: "tool_start", call_id: "c6", name: "terminal", args: { command: "echo 14" }, at: 141 }),
  ev({ type: "tool_end", call_id: "c6", name: "terminal", result: '{"output": "14", "exit_code": 0}', at: 142 })]);
assert.match(steerLine(), / · read by the subagent$/);
assert.ok(!box.querySelectorAll(".msg.user.sm").pop().classList.contains("pending"));
// One sent after the last step, then the subagent ends: never read.
v.add([ev({ type: "steer", text: "Stop soon.", at: 150 })]);
v.set({ model: "claude-opus-5-5", end: { state: "ok", durationS: 1300, result: "Added the flag.", tokens: "3.1k in · 920 out", endedAt: T0 + 160000 } });
assert.match(steerLine(), / · the subagent ended before reading it$/);
// Ended: every call is folded, and the Done card closes the chat.
assert.strictEqual(box.querySelectorAll(".tool").filter((c) => c.parentNode === box).length, 0);
const endCard = box.children[box.children.length - 2];
assert.ok(endCard.classList.contains("mc-end") && !endCard.classList.contains("bad"));
assert.match(endCard.textContent, /^Done in 21 min 40 s/);
assert.match(endCard.textContent, /Added the flag\.\n3\.1k in · 920 out/);

// A failed end is red.
v.set({ end: { state: "bad", durationS: 723, result: "Timed out." } });
assert.ok(box.querySelector(".mc-end").classList.contains("bad"));
assert.match(box.querySelector(".mc-end").textContent, /^Failed after 12 min 3 s/);
// Reset clears it all for a transcript Hermes rewrote.
v.reset();
assert.deepStrictEqual(kids(), ["jump"]);

// A tool_call carries several calls. The small chat draws each as the card of
// its tool (not one wrench), keys each result to its own call (the results all
// carry the wrapper's call id and must not overwrite one another), and the
// cards fold with their neighbors like any others.
{
  const w = M.create({ el, toast: () => {} });
  const b = w.node;
  const titles = () => b.querySelectorAll(".notice-title").map((n) => n.textContent);
  const wrapperArgs = { calls: [
    { name: "process_manage", arguments: { action: "wait", session_id: "proc_283637f9d51", timeout: 180 } },
    { name: "session_search", arguments: { query: "dns split horizon" } },
    { name: "browser_vault", arguments: { action: "list", url: "https://github.com" } },
  ] };
  w.set({ model: "m" });
  w.add([ev({ type: "tool_start", call_id: "w1", name: "tool_call", args: wrapperArgs, at: 10 })]);
  assert.deepStrictEqual(titles(), ["Waiting for background command proc_283637f9d51", "Searching past chats for dns split horizon", "Looking for a saved login on github.com"]);
  assert.strictEqual(b.querySelectorAll(".tool").length, 3);
  assert.ok(b.querySelectorAll(".tool").every((c) => c.classList.contains("running")));
  // One result row per inner call, all under the wrapper's id, out of order.
  w.add([ev({ type: "tool_end", call_id: "w1", name: "session_search", result: '{"results": [1, 2, 3, 4]}', duration_ms: 320, at: 11 })]);
  assert.strictEqual(b.querySelectorAll(".tool").filter((c) => c.classList.contains("running")).length, 2);
  w.add([ev({ type: "tool_end", call_id: "w1", name: "process_manage", result: '{"status": "running"}', duration_ms: 180000, at: 190 }),
    ev({ type: "tool_end", call_id: "w1", name: "browser_vault", result: '{"logins": [{}]}', duration_ms: 88, at: 190 })]);
  const cards = b.querySelectorAll(".tool");
  assert.ok(cards.every((c) => !c.classList.contains("running") && c.finished), "every inner call finished");
  assert.deepStrictEqual(cards.map((c) => c.call.res && Object.keys(c.call.res)[0]), ["status", "results", "logins"]);
  assert.deepStrictEqual(titles(), ["Waited for background command proc_283637f9d51", "Searched past chats for dns split horizon", "Looked for a saved login on github.com"]);
  assert.deepStrictEqual(cards.map((c) => c.dataset.ukey), ["t:w1:0", "t:w1:1", "t:w1:2"]);
  // The batch ran together: the fold shows the real time from the first
  // start to the last end, not the 3 min 0.4 s the three times add up to.
  w.set({ model: "m", end: { state: "ok", durationS: 190, result: "Done." } });
  const g = b.querySelectorAll("details.toolgroup");
  assert.strictEqual(g.length, 1, "the calls fold into one group");
  const line = g[0].firstChild.textContent.split(" \u00B7 ")[0];
  assert.match(line, /^Managed a background command, searched past chats, used a saved login \(3 min\)$/, line);

  // A result that names no tool goes to the first call that has none.
  const w2 = M.create({ el, toast: () => {} });
  w2.add([ev({ type: "tool_start", call_id: "w2", name: "tool_call", args: { calls: [{ name: "terminal", arguments: { command: "a" } }, { name: "terminal", arguments: { command: "b" } }] }, at: 1 }),
    ev({ type: "tool_end", call_id: "w2", name: "tool_call", result: '{"output": "A", "exit_code": 0}', at: 2 }),
    ev({ type: "tool_end", call_id: "w2", name: "terminal", result: '{"output": "B", "exit_code": 0}', at: 3 })]);
  assert.deepStrictEqual(w2.node.querySelectorAll(".tool").map((c) => c.call.out), ["A", "B"]);
  // The same results again change nothing.
  w2.add([ev({ type: "tool_end", call_id: "w2", name: "terminal", result: '{"output": "C", "exit_code": 0}', at: 4 })]);
  assert.deepStrictEqual(w2.node.querySelectorAll(".tool").map((c) => c.call.out), ["A", "B"]);

  // One call in the wrapper is a plain card, no batch.
  const w3 = M.create({ el, toast: () => {} });
  w3.add([ev({ type: "tool_start", call_id: "w3", name: "tool_call", args: { calls: [{ name: "terminal", arguments: { command: "ls" } }] }, at: 1 }),
    ev({ type: "tool_end", call_id: "w3", name: "terminal", result: '{"output": "", "exit_code": 0}', duration_ms: 1500, at: 3 })]);
  assert.strictEqual(w3.node.querySelectorAll(".tool").length, 1);
  assert.strictEqual(w3.node.querySelector(".tool").batch, "");
  assert.deepStrictEqual(w3.node.querySelectorAll(".notice-title").map((n) => n.textContent), ["Ran ls"]);
  w3.set({ end: { state: "ok" } });
  assert.match(w3.node.querySelector("details.toolgroup").firstChild.textContent, /^Ran a command \(1\.5 s\)/);
}

// Times that are not known are not shown: no "1 ms" for a call of 0 ms, no
// time for a step Hermes gave none (the hub sends year 1 or leaves it out),
// and such steps keep their order.
{
  const w = M.create({ el, toast: () => {} });
  const none = (o) => Object.assign(ev(Object.assign({ at: 0 }, o)), { ts: o.ts });
  w.add([none({ type: "tool_start", call_id: "z1", name: "terminal", args: { command: "ls" }, ts: "0001-01-01T00:00:00Z" }),
    none({ type: "tool_end", call_id: "z1", name: "terminal", result: '{"output": "", "exit_code": 0}', duration_ms: 0, ts: "0001-01-01T00:00:00Z" }),
    none({ type: "tool_start", call_id: "z2", name: "terminal", args: { command: "pwd" } }),
    none({ type: "tool_end", call_id: "z2", name: "terminal", result: '{"output": "", "exit_code": 0}' })]);
  const cs = w.node.querySelectorAll(".tool");
  assert.strictEqual(cs.length, 2);
  for (const c of cs) {
    assert.strictEqual(c.call.startedAt, 0);
    assert.strictEqual(c.call.endedAt, 0);
    assert.strictEqual(c.call.duration, undefined);
    assert.ok(!/\bms\b/.test(c.textContent), c.textContent);
  }
  assert.deepStrictEqual(w.node.querySelectorAll(".notice-title").map((n) => n.textContent), ["Ran ls", "Ran pwd"]);
  // order keeps a step with no time next to the step before it.
  const a = ev({ type: "message", at: 5 });
  const u = Object.assign(ev({ type: "message", at: 0 }), { ts: undefined });
  const c2 = ev({ type: "message", at: 7 });
  assert.deepStrictEqual(M.order([c2, u, a]).map((e) => e.seq), [a.seq, u.seq, c2.seq]);
  // A message the user sent before a result with no time of its own counts as read.
  const steer = ev({ type: "steer", at: 10 });
  const later = Object.assign(ev({ type: "tool_end", at: 0 }), { ts: undefined });
  assert.strictEqual(M.steerState(steer, [steer, later], false), "read");
}

// The group line: calls that ran together show no sum.
{
  const T = sandbox.window.HubTimes;
  const call = (s, e, d) => ({ toolName: "terminal", duration: d, failed: false, batch: "", call: { startedAt: T0 + s * 1000, endedAt: T0 + e * 1000, duration: d } });
  // Crossing times: 100 s and 100 s inside 110 s of real time.
  const x = G.summary([call(0, 100, 100), call(10, 110, 100)]).text;
  assert.match(x, /^Ran 2 commands \(1 min 50 s\)/, x);
  // One after the other: the sum stands.
  const y = G.summary([call(0, 10, 10), call(10, 25, 15)]).text;
  assert.match(y, /^Ran 2 commands \(25 s\)/, y);
  // One batch with no times: nothing is summed.
  const z = G.summary([{ toolName: "terminal", duration: 5, batch: "w", call: {} }, { toolName: "terminal", duration: 5, batch: "w", call: {} }]).text;
  assert.strictEqual(z, "Ran 2 commands");
  // The deferred tools have words, not "used session_search".
  assert.strictEqual(G.summary([{ toolName: "session_search", call: {} }, { toolName: "browser_vault", call: {} }, { toolName: "mcp_device_call", call: {} }, { toolName: "tool_search", call: {} }, { toolName: "project_memory", call: {} }]).text,
    "Searched past chats, used a saved login, called a tool on your computer, looked up a tool, used the project notes");
}
console.log("ok");
