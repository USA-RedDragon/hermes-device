"use strict";
// The subagent's steps as a small chat: the pure parts of minichat.js and
// toolgroup.js, then a view drawn into a small fake DOM: what it shows, how
// calls fold into the chat's groups, when it follows new steps and when
// the pill counts them, and the state of the messages the user sent.
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

// A small DOM: elements with classes, attributes, children and text, and a
// scroll box whose content is 50 px per child.
class Text {
  constructor(t) { this.data = String(t); this.parentNode = null; }
  get textContent() { return this.data; }
  remove() { if (this.parentNode) this.parentNode._drop(this); }
}
class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.childNodes = [];
    this.parentNode = null;
    this.className = "";
    this.dataset = {};
    this.attrs = {};
    this.hidden = false;
    this.open = false;
    this.listeners = {};
    this.style = {};
    this._top = 0;
    this.clientHeight = 200;
    const self = this;
    this.classList = {
      add: (...c) => { for (const x of c) if (!self._cls().includes(x)) self.className = (self.className + " " + x).trim(); },
      remove: (...c) => { self.className = self._cls().filter((x) => !c.includes(x)).join(" "); },
      contains: (c) => self._cls().includes(c),
      toggle: (c, on) => { if (on === undefined) on = !self._cls().includes(c); if (on) self.classList.add(c); else self.classList.remove(c); return on; },
    };
  }
  _cls() { return this.className.split(/\s+/).filter(Boolean); }
  _drop(n) { this.childNodes = this.childNodes.filter((x) => x !== n); n.parentNode = null; }
  _adopt(n) { if (typeof n === "string" || typeof n === "number") n = new Text(n); if (n.parentNode) n.parentNode._drop(n); n.parentNode = this; return n; }
  get children() { return this.childNodes.filter((n) => n instanceof El); }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
  get previousElementSibling() {
    if (!this.parentNode) return null;
    const k = this.parentNode.children;
    return k[k.indexOf(this) - 1] || null;
  }
  get isConnected() { return true; }
  get textContent() { return this.childNodes.map((n) => n.textContent).join(""); }
  set textContent(v) { this.childNodes = []; if (v !== "" && v !== undefined && v !== null) this.append(String(v)); }
  append(...k) { for (const n of k) this.childNodes.push(this._adopt(n)); }
  insertBefore(n, ref) { n = this._adopt(n); const i = ref ? this.childNodes.indexOf(ref) : -1; if (i < 0) this.childNodes.push(n); else this.childNodes.splice(i, 0, n); return n; }
  replaceChildren(...k) { for (const n of this.childNodes) n.parentNode = null; this.childNodes = []; this.append(...k); }
  replaceWith(n) { const p = this.parentNode; p.insertBefore(n, this); p._drop(this); }
  remove() { if (this.parentNode) this.parentNode._drop(this); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  dispatch(t) { for (const fn of this.listeners[t] || []) fn({}); }
  _match(part) {
    const m = part.match(/^([a-z]*)((?:\.[\w-]+)*)((?:\[[^\]]+\])*)$/i);
    if (!m) throw new Error("selector " + part);
    if (m[1] && this.tagName !== m[1].toUpperCase()) return false;
    for (const c of m[2].split(".").filter(Boolean)) if (!this._cls().includes(c)) return false;
    for (const a of m[3].match(/\[[^\]]+\]/g) || []) {
      const [, k, v] = a.match(/\[([\w-]+)="?([^"\]]*)"?\]/);
      if (this.getAttribute(k) !== v) return false;
    }
    return true;
  }
  matches(sel) { return this._match(sel); }
  querySelectorAll(sel) {
    const parts = sel.trim().split(/\s+/);
    const out = [];
    const walk = (n, ancestors) => {
      for (const c of n.children) {
        const chain = [...ancestors, c];
        if (c._match(parts[parts.length - 1])) {
          let i = parts.length - 2;
          for (let j = chain.length - 2; j >= 0 && i >= 0; j--) if (chain[j]._match(parts[i])) i--;
          if (i < 0) out.push(c);
        }
        walk(c, chain);
      }
    };
    walk(this, []);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  get scrollHeight() { return Math.max(this.clientHeight, this.children.length * 50); }
  get scrollTop() { return this._top; }
  set scrollTop(v) { this._top = Math.max(0, Math.min(v, this.scrollHeight - this.clientHeight)); }
}
const el = (tag, cls, text) => {
  const n = new El(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null && text !== "") n.append(String(text));
  return n;
};
const document = { createElement: (t) => new El(t), createDocumentFragment: () => new El("fragment") };

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
console.log("ok");
