"use strict";
// The Activity sidebar's subagent rows: the status line under a running
// subagent (a sentence, the raw text one tap away), the token line of the
// header, and when a row counts as running or finished (only on what Hermes
// says, never on one missing list answer).
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");
const { el, document, El } = require("../fakedom.js");

let now = 1_800_000_000_000;
class D extends Date {
  static now() { return now; }
}
const timers = new Map();
const ticks = [];
const tickClock = () => { for (const f of ticks) f(); };
let tid = 0;
const ids = {};
for (const id of ["actbody", "actbtn", "actclose", "actcount", "actsum", "app", "c-bg", "c-sub", "tab-bg", "tab-sub", "tok", "toksub", "tokturn"]) ids[id] = new El("div");
const doc = Object.assign({}, document, { getElementById: (id) => ids[id], hidden: false, activeElement: null, addEventListener: () => {}, querySelector: () => null, body: new El("body") });
const copied = [];
const sandbox = {
  window: {}, document: doc, Date: D, Math, JSON, Intl, Promise, Array, Set, Map,
  setTimeout: (fn, ms) => { const id = ++tid; timers.set(id, fn); return id; },
  clearTimeout: (id) => { timers.delete(id); },
  setInterval: (fn) => { ticks.push(fn); return 1; }, clearInterval: () => {},
  matchMedia: () => ({ matches: false }), localStorage: { getItem: () => null, setItem: () => {} },
  requestAnimationFrame: (fn) => fn(), CSS: { escape: (s) => s },
  location: { hash: "" }, navigator: { clipboard: { writeText: async (t) => { copied.push(t); } } },
};
sandbox.window = sandbox;
const toasts = [];
sandbox.window.HubToast = { show: (text, opts) => { toasts.push([text, opts]); } };
sandbox.window.HubBackoff = () => ({ next: () => 3000, reset: () => {} });
sandbox.window.HubIcons = { icon: (name) => el("svg", "ico i-" + name), button: (name, label, cls) => { const b = el("button", cls); b.title = label; return b; } };
vm.createContext(sandbox);
const dir = process.argv[2];
for (const f of ["times.js", "toolcard.js", "toolgroup.js", "minichat.js", "activity.js"]) vm.runInContext(fs.readFileSync(dir + "/" + f, "utf8"), sandbox);

// What the hub answers to the sidebar's reads.
let list = [];
let verdict = { live: true, running: true };
let verdictFails = false;
const reads = [];
async function api(method, path) {
  reads.push(path);
  if (path.endsWith("/activity")) return { subagents: list, processes: [] };
  if (/\/subagents\/[^/]+\/events/.test(path)) {
    if (verdictFails) throw new Error("no answer");
    return Object.assign({ events: [], epoch: 1, more: false }, verdict);
  }
  return {};
}
const act = sandbox.window.HubActivity({ api, el, banner: () => {}, showText: () => {} });

(async () => {
const chat = { base: "/api/sessions/b/s", backend: "b" };
act.show(chat);
// Runs what the page would run after its timers, until nothing is left.
async function settle() {
  for (let i = 0; i < 5 && timers.size; i++) {
    const fns = [...timers.values()];
    timers.clear();
    for (const fn of fns) await fn();
  }
}
async function round() {
  const fns = [...timers.values()];
  timers.clear();
  for (const fn of fns) await fn();
}
const body = () => ids.actbody;
const text = () => body().textContent;
const sumText = () => ids.actsum.textContent;

// A subagent starts and runs a tool: the line is a sentence, not the raw text.
act.event("subagent.start", { subagent_id: "sa-1", goal: "Check the cluster alerts", model: "m" });
act.event("subagent.tool", { subagent_id: "sa-1", tool_name: "process_manage", tool_preview: "wait proc_283637f9d51 180s" });
let line = body().querySelector("button.now2");
assert.ok(line, "the status line is a button");
assert.match(line.textContent, /^Waiting on a background command/);
assert.ok(!text().includes("process_manage"), "the raw text is not shown closed: " + text());
assert.ok(!text().includes("proc_283637f9d51"));
assert.strictEqual(line.getAttribute("aria-expanded"), "false");
assert.ok(line.querySelector("svg.i-clock"), "the icon fits the sentence");
// An elapsed time follows the sentence.
now += 185000;
act.event("subagent.progress", { subagent_id: "sa-1", text: "" });
now += 0;
act.event("subagent.tool", { subagent_id: "sa-1", tool_name: "process_manage", tool_preview: "wait proc_283637f9d51 180s" });
now += 185000;
act.event("subagent.progress", { subagent_id: "sa-1", text: "" });
// Tapping the line opens the raw text with a copy button, and not the row.
const stops = [];
let tap = { stopPropagation: () => stops.push(1) };
body().querySelector("button.now2").onclick(tap);
assert.ok(stops.length, "the tap stays on the line");
assert.ok(!body().querySelector(".row.open"), "the row did not open");
line = body().querySelector("button.now2");
assert.strictEqual(line.getAttribute("aria-expanded"), "true");
const raw = body().querySelector(".rawbox");
assert.ok(raw && raw.textContent === "wait proc_283637f9d51 180s", raw && raw.textContent);
raw.querySelector("button").onclick(tap);
assert.deepStrictEqual(copied, ["wait proc_283637f9d51 180s"]);
// It stays open through the redraw every second, and a tap closes it.
act.event("subagent.progress", { subagent_id: "sa-1", text: "" });
assert.ok(body().querySelector(".rawbox"));
body().querySelector("button.now2").onclick(tap);
assert.ok(!body().querySelector(".rawbox"));
// The rest of the row still opens the small chat.
body().querySelector(".row-head").onclick();
assert.ok(body().querySelector(".row.open"));
body().querySelector(".row-head").onclick();
// A subagent's own words stay plain text, and a tool it does not know is "Using ...".
act.event("subagent.thinking", { subagent_id: "sa-1", text: "Looking at the flux drift" });
assert.ok(!body().querySelector("button.now2"));
assert.ok(text().includes("Looking at the flux drift"));
act.event("subagent.tool", { subagent_id: "sa-1", tool_name: "some_new_tool", tool_preview: "{...}" });
assert.match(body().querySelector("button.now2").textContent, /^Using some new tool/);
// A batched call arrives as progress text with Hermes's emoji: a sentence with a
// line icon, no emoji, and not the monospace text.
const pict = /\p{Extended_Pictographic}|\uFE0F/u;
act.event("subagent.progress", { subagent_id: "sa-1", text: "\u{1F500} [set 5] terminal, terminal, process_manage" });
line = body().querySelector("button.now2");
assert.ok(line, "a batch is a status line");
assert.match(line.textContent, /^Running 5 tools: terminal, process_manage/);
assert.ok(line.querySelector("svg.i-tool"));
assert.ok(!body().querySelector(".now"), "no raw monospace line: " + text());
// Hermes events with emoji everywhere: nothing rendered carries one, closed or open.
const fixtures = [
  ["subagent.tool", { tool_name: "\u{1F9E0}memory", tool_preview: "\u{1F4BE} save" }],
  ["subagent.tool", { tool_name: "\u{1F5A5}\uFE0F brand_new_tool", tool_preview: "\u2705 {...}" }],
  ["subagent.tool", { tool_name: "tool_call", tool_preview: "\u{1F500} [set 3] web_search, web_search, read_file" }],
  ["subagent.progress", { text: "\u26A1 Working on it" }],
  ["subagent.tool", { tool_name: "delegate_task", tool_preview: "\u{1F500} go" }],
];
for (const [kind, p] of fixtures) {
  act.event(kind, Object.assign({ subagent_id: "sa-1" }, p));
  assert.ok(!pict.test(text()), kind + " leaked an emoji: " + text());
  const l = body().querySelector("button.now2");
  if (l && l.getAttribute("aria-expanded") !== "true") l.onclick({ stopPropagation() {} });
  assert.ok(!pict.test(text()), kind + " leaked an emoji when open: " + text());
  const l2 = body().querySelector("button.now2");
  if (l2 && l2.getAttribute("aria-expanded") === "true") l2.onclick({ stopPropagation() {} });
}
assert.match(body().querySelector("button.now2").textContent, /^Starting a subagent/);
assert.ok(!text().includes("{...}"));

// The header does not claim 0 tokens when none are counted yet.
assert.match(sumText(), /Subagents in this chat: tokens appear when each finishes · 1 running, 0 finished/, sumText());
act.event("subagent.complete", { subagent_id: "sa-1", status: "completed", summary: "Fixed it.", input_tokens: 3100, output_tokens: 920, duration_seconds: 400 });
assert.match(sumText(), /Subagents in this chat: 3\.1k in · 920 out$/, sumText());
assert.ok(!sumText().includes("0 in"));

// Running or finished comes from Hermes. One list without the subagent says nothing.
const chat2 = { base: "/api/sessions/b/s2", backend: "b" };
act.show(chat2);
act.event("subagent.start", { subagent_id: "sa-2", goal: "Second", model: "m" });
act.event("subagent.start", { subagent_id: "sa-3", goal: "Third", model: "m" });
const rowState = (id) => { const r = body().querySelectorAll(".row").find((x) => x.dataset.act === id); return r ? r.querySelector(".meta").children[0].textContent : "none"; };
now += 60000;
list = [{ id: "sa-2", goal: "Second", status: "running" }, { id: "sa-3", goal: "Third", status: "running" }];
await round();
assert.strictEqual(rowState("sa-2"), "Running");
// sa-2 leaves the list for a long while (a slow or failed read, a restart): still running.
list = [{ id: "sa-3", goal: "Third", status: "running" }];
for (let i = 0; i < 4; i++) { now += 9000; await round(); }
assert.strictEqual(rowState("sa-2"), "Running", "Hermes said it still runs");
assert.ok(reads.some((p) => /subagents\/sa-2\/events/.test(p)), "it asked before it decided");
// No answer from Hermes: it stays as it is.
verdictFails = true;
for (let i = 0; i < 4; i++) { now += 9000; await round(); }
assert.strictEqual(rowState("sa-2"), "Running");
// Only Hermes saying it does not run ends the row.
verdictFails = false;
verdict = { live: true, running: false };
for (let i = 0; i < 4; i++) { now += 9000; await round(); }
assert.strictEqual(rowState("sa-2"), "Done");
assert.strictEqual(rowState("sa-3"), "Running");
// A stored copy without a live Hermes answer is no proof either.
act.event("subagent.start", { subagent_id: "sa-4", goal: "Fourth", model: "m" });
verdict = { live: false, running: false };
for (let i = 0; i < 4; i++) { now += 9000; await round(); }
assert.strictEqual(rowState("sa-4"), "Running");
// The list says what became of a subagent: a status it gives wins.
list = [{ id: "sa-3", goal: "Third", status: "completed" }, { id: "sa-4", goal: "Fourth", status: "running" }];
await round();
assert.strictEqual(rowState("sa-3"), "Done");
assert.strictEqual(rowState("sa-4"), "Running");
list = [{ id: "sa-4", goal: "Fourth", status: "failed" }];
await round();
assert.strictEqual(rowState("sa-4"), "Failed");
// A row the complete event ended stays ended even when a stale list shows it running.
act.event("subagent.start", { subagent_id: "sa-5", goal: "Fifth", model: "m" });
act.event("subagent.complete", { subagent_id: "sa-5", status: "interrupted" });
list = [{ id: "sa-5", goal: "Fifth", status: "running" }];
await round();
assert.strictEqual(rowState("sa-5"), "Stopped");
// A restart hands a running subagent to a new Hermes that lists its own start and a fresh tool count: the row
// keeps its earliest start and its highest count, and its timer keeps counting from the real start.
const rowTime = (id) => { const r = body().querySelectorAll(".row").find((x) => x.dataset.act === id); return r.querySelector(".time").textContent; };
const rowTools = (id) => { const r = body().querySelectorAll(".row").find((x) => x.dataset.act === id); return r.querySelector(".meta").textContent; };
const realStart = now - 17 * 60 * 1000;
act.event("subagent.start", { subagent_id: "sa-6", goal: "Sixth", model: "m" });
list = [{ id: "sa-6", goal: "Sixth", status: "running", started_at: realStart / 1000, tool_count: 41 }];
await round();
assert.strictEqual(rowTime("sa-6"), "17:00");
assert.ok(rowTools("sa-6").includes("41 tools"), rowTools("sa-6"));
now += 60 * 1000;
list = [{ id: "sa-6", goal: "Sixth", status: "running", started_at: (now - 5000) / 1000, tool_count: 0 }];
await round();
assert.strictEqual(rowTime("sa-6"), "18:00");
assert.ok(rowTools("sa-6").includes("41 tools"), rowTools("sa-6"));
// A draw that changes one row builds that row only: the nodes of the others stay the same ones, and a
// draw that changes nothing keeps all of them. The clock of a running row moves without a build.
const rowNode = (id) => body().querySelectorAll(".row").find((x) => x.dataset.act === id);
const sa6 = rowNode("sa-6");
const sa5 = rowNode("sa-5");
act.event("subagent.start", { subagent_id: "sa-9", goal: "Ninth", model: "m" });
assert.strictEqual(rowNode("sa-6"), sa6, "an unchanged running row kept its node after another row came");
assert.strictEqual(rowNode("sa-5"), sa5, "an unchanged finished row kept its node");
assert.ok(rowNode("sa-9"), "the new row is there");
act.event("subagent.complete", { subagent_id: "sa-9", status: "completed" });
assert.strictEqual(rowNode("sa-6"), sa6);
assert.strictEqual(rowNode("sa-5"), sa5);
assert.strictEqual(rowState("sa-9"), "Done");
const before = rowTime("sa-6");
now += 5000;
tickClock();
assert.strictEqual(rowNode("sa-6"), sa6, "the clock tick did not build the row");
assert.notStrictEqual(rowTime("sa-6"), before, "the clock moved");
// A page that missed the tool's event has only the list: it names the tool and what it was given.
// With the command, the line opens to it; with the name alone there is no box and no raw tool name.
const rowOf = (id) => body().querySelectorAll(".row").find((x) => x.dataset.act === id);
const nowText = (id) => body().querySelectorAll(".row").find((x) => x.dataset.act === id).textContent;
list = [{ id: "sa-7", goal: "Seventh", status: "running", started_at: now / 1000, tool_count: 15, last_tool: "terminal", last_preview: "make test ARGS=-race" }];
await round();
let l7 = rowOf("sa-7").querySelector("button.now2");
assert.ok(l7, "the line opens when the command is known");
assert.match(l7.textContent, /^Running make test/);
l7.onclick({ stopPropagation() {} });
assert.strictEqual(rowOf("sa-7").querySelector(".rawbox").textContent, "make test ARGS=-race");
list = [{ id: "sa-8", goal: "Eighth", status: "running", started_at: now / 1000, tool_count: 15, last_tool: "terminal" }];
await round();
assert.ok(!rowOf("sa-8").querySelector("button.now2"), "no button without a command");
assert.ok(!rowOf("sa-8").querySelector(".rawbox"), "no empty box");
assert.ok(nowText("sa-8").includes("Running a command") && !nowText("sa-8").includes("terminal"), nowText("sa-8"));
// The command arrives in a later list: the same row gets it.
list = [{ id: "sa-8", goal: "Eighth", status: "running", started_at: now / 1000, tool_count: 15, last_tool: "terminal", last_preview: "ls -la" }];
await round();
assert.match(rowOf("sa-8").querySelector("button.now2").textContent, /^Running ls -la/);
// A subagent that ended its turn waiting on a background job (Hermes status "waiting") is not
// failed and not finished: it shows what it waits on, counts as running, and shows its elapsed time.
const liveCount = () => Number(ids.actcount.textContent || 0);
const runningBefore = liveCount();
const longCmd = "bash /home/reddragon/Projects/dmrgo/wait.sh " + "--flag ".repeat(40) + "done";
list = [{ id: "sa-w", goal: "Build the thing", status: "waiting", waiting_on: [longCmd, "sleep 99"], started_at: now / 1000 - 3 * 3600 - 13 * 60 }];
await round();
const wrow = rowOf("sa-w");
assert.ok(wrow, "the waiting subagent has a row");
assert.ok(!/Failed/.test(wrow.textContent), "a waiting subagent is not failed: " + wrow.textContent);
assert.ok(!/Done/.test(wrow.textContent), "a waiting subagent is not done: " + wrow.textContent);
assert.ok(wrow.querySelector(".state.hold"), "its own state color");
assert.ok(!wrow.querySelector(".state.bad") && !wrow.querySelector(".state.ok"));
assert.match(wrow.textContent, /Waiting on bash \/home\/reddragon\/Projects\/dmrgo\/wait\.sh/);
assert.ok(wrow.textContent.includes("Waiting on ") && !wrow.textContent.includes("done"), "the command is clamped: " + wrow.textContent);
assert.ok(wrow.textContent.includes("+1 more") || wrow.textContent.includes("1 more"), "a second command is counted: " + wrow.textContent);
assert.match(wrow.querySelector(".time").textContent, /3\s*h|3:13|193/, "elapsed since start: " + wrow.querySelector(".time").textContent);
assert.strictEqual(liveCount(), runningBefore + 1, "a waiting subagent counts as running");
assert.ok(/\d+ running/.test(ids["c-sub"].textContent), ids["c-sub"].textContent);
// Many polls later, and with the list missing it once, nothing marks it failed or finished.
for (let i = 0; i < 4; i++) { now += 60000; await round(); }
assert.ok(!/Failed|Done/.test(rowOf("sa-w").textContent), "still waiting: " + rowOf("sa-w").textContent);
// A stray complete event that says waiting does not end it either.
act.event("subagent.complete", { subagent_id: "sa-w", status: "waiting" });
assert.ok(!/Failed|Done/.test(rowOf("sa-w").textContent) && rowOf("sa-w").querySelector(".state.hold"));
// It resumes: the list says running and the row runs again.
list = [{ id: "sa-w", goal: "Build the thing", status: "running", started_at: now / 1000 - 3 * 3600 - 20 * 60 }];
await round();
assert.ok(rowOf("sa-w").querySelector(".state.run"), "running again");
assert.ok(!/Waiting on/.test(rowOf("sa-w").textContent));
assert.strictEqual(liveCount(), runningBefore + 1);
// And it waits again, then finishes once.
list = [{ id: "sa-w", goal: "Build the thing", status: "waiting", waiting_on: ["make all"], started_at: now / 1000 - 3 * 3600 - 20 * 60 }];
await round();
assert.match(rowOf("sa-w").textContent, /Waiting on make all/);
list = [{ id: "sa-w", goal: "Build the thing", status: "completed", started_at: now / 1000 - 3 * 3600 - 20 * 60 }];
await round();
assert.ok(rowOf("sa-w").querySelector(".state.ok"), "done");
assert.strictEqual(liveCount(), runningBefore);
assert.strictEqual(rowState("sa-w"), "Done");

console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
