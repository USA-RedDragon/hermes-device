"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

// A DOM just big enough for the panel, the menu row and the chips.
class Node {
  constructor(tag, cls) { this.tag = tag; this.className = cls || ""; this.children = []; this.textContent = ""; this.disabled = false; this.parent = null; this.attrs = {}; this.value = ""; }
  get isConnected() { let n = this; while (n.parent) n = n.parent; return n === doc.body; }
  append(...kids) { for (const k of kids) { if (typeof k === "string") { const n = new Node("#text"); n.textContent = k; this.append(n); } else { k.parent = this; this.children.push(k); } } }
  prepend(k) { k.parent = this; this.children.unshift(k); }
  replaceChildren(...kids) { this.children = []; this.append(...kids); }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter((c) => c !== this); this.parent = null; }
  setAttribute(k, v) { this.attrs[k] = v; }
  focus() {}
  contains(n) { for (; n; n = n.parent) if (n === this) return true; return false; }
  walk(fn) { for (const c of this.children) { fn(c); c.walk(fn); } }
  matches(sel) { return sel.split(",").some((s) => this.className.split(" ").includes(s.trim().replace(/^[.:a-z-]*\./, "").replace(/^\./, ""))); }
  querySelector(sel) { let r = null; this.walk((n) => { if (!r && n.matches(sel)) r = n; }); return r; }
  querySelectorAll(sel) { const r = []; this.walk((n) => { if (n.matches(sel)) r.push(n); }); return r; }
  text() { let s = this.textContent; for (const c of this.children) s += " " + c.text(); return s.trim(); }
}
const el = (tag, cls, text) => { const n = new Node(tag, cls); if (text !== undefined && text !== null) n.textContent = text; return n; };
const doc = { body: new Node("body"), addEventListener() {}, removeEventListener() {} };

const timers = [];
const sandbox = {
  window: {}, document: doc, setTimeout: (fn) => { timers.push(fn); return timers.length; }, Date, Promise, Array, Set, Map, Math, String, encodeURIComponent,
};
vm.createContext(sandbox);
sandbox.window.HubIcons = { icon: (n) => el("svg", "ico " + n), button: (n, label, cls) => { const b = el("button", cls); b.attrs["aria-label"] = label; b.append(el("svg", "ico " + n)); return b; } };
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const W = sandbox.window.HubWorksWith;
const eq = (a, b) => assert.strictEqual(JSON.stringify(a), JSON.stringify(b));
const flush = async () => { while (timers.length) await timers.shift()(); await new Promise((r) => setImmediate(r)); };

// Pure helpers.
assert.strictEqual(W.countText(0), "None");
assert.strictEqual(W.countText(1), "1 chat");
assert.strictEqual(W.countText(3), "3 chats");
assert.strictEqual(W.hopText({ hop: 4, max_hop: 30 }), "hop 4 of 30");
assert.strictEqual(W.hopText({ hop: 5, max_hop: 6 }), "hop 5 of 6");
assert.strictEqual(W.hopText({ hop: 0, max_hop: 6 }), "");
assert.strictEqual(W.hopText(null), "");
eq(W.chips({ partner: true, hop: 4, max_hop: 30 }).map((c) => c.text), ["Works with this chat", "hop 4 of 30"]);
eq(W.chips({ partner: false, hop: 2, max_hop: 6 }).map((c) => c.text), ["hop 2 of 6"]);
eq(W.chips(null), []);
assert.ok(W.mentions("Stop. If these chats work together, the user can mark these chats as working together to raise the limit"));
assert.ok(!W.mentions("rate limited"));
eq(W.partnersOf([{ a: "s1", b: "s2" }, { a: "s0", b: "s1" }, { a: "s3", b: "s4" }], "s1"), ["s2", "s0"]);
const sessions = [
  { backend_id: "pc", id: "s1", title: "Build", updated_at: "2026-01-01T00:00:00Z" },
  { backend_id: "pc", id: "s2", title: "Research", updated_at: "2026-01-02T00:00:00Z" },
  { backend_id: "pc", id: "s3", title: "Docs pass", updated_at: "2026-01-03T00:00:00Z" },
  { backend_id: "pc", id: "s4", title: "Family trip", updated_at: "2026-01-04T00:00:00Z" },
  { backend_id: "pc", id: "s5", title: "Old", archived: true },
  { backend_id: "home", id: "h1", title: "Other backend" },
];
eq(W.candidates(sessions, "pc", "s1", ["s2"], "").map((s) => s.id), ["s4", "s3"]);
eq(W.candidates(sessions, "pc", "s1", ["s2"], "DOCS").map((s) => s.id), ["s3"]);

(async () => {
  const calls = [];
  let pairs = [{ a: "s1", b: "s2" }];
  const msgs = { am_0000000000000001: { from_session: "s2", hop: 4, max_hop: 30, partner: true }, am_0000000000000002: { from_session: "s3", hop: 5, max_hop: 6, partner: false } };
  const api = async (method, path, body) => {
    calls.push(method + " " + path);
    if (path.includes("/agent-messages")) return msgs;
    if (path.startsWith("/api/backends/home/")) { const e = new Error("none"); e.status = 404; throw e; }
    if (method === "POST") { pairs = pairs.concat([{ a: body.a, b: body.b }]); return body; }
    if (method === "DELETE") { pairs = []; return null; }
    return pairs;
  };
  let redraws = 0;
  let cur = { b: "pc", s: "s1" };
  const me = { partners: true };
  const ww = W.create({ api, el, toast() {}, banner() {}, me: () => me, backends: () => [{ id: "pc" }, { id: "home" }], sessions: () => sessions,
    where: (s) => (s.id === "s2" ? "upstairs-pc, ~/Projects/research" : ""), current: () => cur, redraw: () => { redraws++; }, phone: () => false });

  eq(ww.menuItems(sessions[0], () => {}), [], "no row before the pairs are known");
  await ww.load();
  assert.ok(redraws > 0);
  assert.ok(ww.available("pc") && !ww.available("home"), "a backend that answers 404 has no setting");
  const row = ww.menuItems(sessions[0], () => {});
  assert.strictEqual(row.length, 1);
  assert.ok(row[0].text().includes("Works with") && row[0].text().includes("1 chat"), row[0].text());
  eq(ww.menuItems(sessions[5], () => {}), []);
  assert.strictEqual(ww.mark(sessions[1]).length, 1, "partner of the open chat has the link icon");
  assert.strictEqual(ww.mark(sessions[2]).length, 0);
  assert.strictEqual(ww.mark(sessions[0]).length, 0);

  // Cards.
  const box = el("div", "notice");
  const head = el("button", "notice-head");
  box.append(head);
  doc.body.append(box);
  ww.decorate(box, "am_0000000000000001", "pc", "s1");
  ww.decorate(el("div"), "not-an-id", "pc", "s1");
  await flush();
  assert.ok(calls.some((c) => c === "GET /api/backends/pc/agent-messages?session=s1&ids=am_0000000000000001"), calls.join("\n"));
  assert.ok(head.text().includes("Works with this chat") && head.text().includes("hop 4 of 30"), head.text());
  const box2 = el("div", "notice");
  const head2 = el("button", "notice-head");
  box2.append(head2);
  doc.body.append(box2);
  ww.decorate(box2, "am_0000000000000002", "pc", "s1");
  await flush();
  assert.ok(!head2.text().includes("Works with this chat") && head2.text().includes("hop 5 of 6"), head2.text());

  // Refusal link.
  const tool = el("div", "tool");
  doc.body.append(tool);
  ww.refusal(tool, "rate_limited: wait. If these chats work together, the user can mark these chats as working together to raise the limit", "pc", "s1");
  ww.refusal(tool, "the user can mark these chats as working together", "pc", "s1");
  assert.strictEqual(tool.querySelectorAll(".ww-refusal").length, 1);
  const none = el("div");
  ww.refusal(none, "all fine", "pc", "s1");
  ww.refusal(none, "the user can mark these chats as working together", "home", "h1");
  assert.strictEqual(none.querySelectorAll(".ww-refusal").length, 0);
  tool.querySelector(".ww-refusal").children[0].onclick();
  const panel = doc.body.querySelector(".ww-panel");
  assert.ok(panel, "the link opens the panel");
  assert.ok(panel.text().includes("up to 30 back-and-forths, not 6."));
  assert.ok(panel.text().includes("Research") && panel.text().includes("upstairs-pc, ~/Projects/research"), panel.text());

  // Add and remove.
  const opt = panel.querySelectorAll("ww-opt").find((o) => o.text().includes("Docs pass"));
  assert.ok(opt, "the picker offers another chat");
  assert.ok(!panel.querySelectorAll("ww-opt").some((o) => o.text().includes("Research")), "not a partner again");
  opt.onclick();
  await flush();
  assert.ok(calls.includes("POST /api/backends/pc/partners"));
  assert.strictEqual(pairs.length, 2);
  const rm = doc.body.querySelector(".ww-rm");
  assert.ok(rm.attrs["aria-label"].startsWith("Remove "));
  rm.onclick();
  await flush();
  assert.ok(calls.includes("DELETE /api/backends/pc/partners/s1/s2"), calls.join("\n"));
  ww.close();
  assert.ok(!doc.body.querySelector(".ww-panel"), "closed");

  // Setting off: nothing shows.
  me.partners = false;
  eq(ww.menuItems(sessions[0], () => {}), []);
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
