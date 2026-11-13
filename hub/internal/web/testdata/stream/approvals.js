"use strict";
const assert = require("assert");
const { load } = require("./harness.js");

// A DOM just big enough for the cards.
class Node {
  constructor(tag, cls) {
    this.tag = tag;
    this.className = cls || "";
    this.children = [];
    this.textContent = "";
    this.disabled = false;
    this.parent = null;
    this.attrs = {};
  }
  append(...kids) { for (const k of kids) { if (typeof k === "string") { const n = new Node("#text"); n.textContent = k; this.append(n); } else { k.parent = this; this.children.push(k); } } }
  replaceChildren(...kids) { this.children = []; this.append(...kids); }
  remove() { if (this.parent) this.parent.children = this.parent.children.filter((c) => c !== this); this.parent = null; }
  setAttribute(k, v) { this.attrs[k] = v; }
  walk(fn) { for (const c of this.children) { fn(c); c.walk(fn); } }
  matches(sel) { return sel.split(",").some((s) => this.className.split(" ").includes(s.trim().replace(/^\./, ""))); }
  querySelector(sel) { let r = null; this.walk((n) => { if (!r && n.matches(sel)) r = n; }); return r; }
  querySelectorAll(sel) { const r = []; this.walk((n) => { if (n.matches(sel)) r.push(n); }); return r; }
  text() { let s = this.textContent; for (const c of this.children) s += " " + c.text(); return s.trim(); }
  buttons() { const r = []; this.walk((n) => { if (n.tag === "button") r.push(n); }); return r; }
}
const el = (tag, cls, text) => { const n = new Node(tag, cls); if (text !== undefined && text !== null) n.textContent = text; return n; };

const words = (k) => ({ approval: "Talaria asks to run this", allow: "Allow", deny: "Deny" })[k];

const flush = () => new Promise((r) => setImmediate(r));

function harness(over) {
  const h = load(process.argv.slice(2));
  const log = [];
  const shown = [];
  const banners = [];
  const tracker = h.win.HubApprovals.create(Object.assign({
    el, base: "/api/sessions/b/s", words, banner: (t) => banners.push(t),
    add: (box) => shown.push(box),
    api: async (method, path, body) => { log.push(JSON.parse(JSON.stringify({ method, path, body }))); },
    setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms), clearTimeout: (id) => globalThis.clearTimeout(id),
  }, over || {}));
  return { h, tracker, log, shown, banners };
}

const prompt = (id, cmd) => ({ kind: "approval", prompt_id: id, command: cmd, description: "recursive delete", choices: ["once", "session", "always", "deny"] });

(async () => {
  // Three approvals at once: three cards, each with its own buttons.
  {
    const { tracker, shown } = harness();
    for (const id of ["a", "b", "c"]) tracker.offer(prompt(id, "cleanup " + id));
    tracker.offer(prompt("b", "cleanup b again")); // a repeat adds no second card
    assert.strictEqual(shown.length, 3);
    for (const box of shown) assert.strictEqual(box.buttons().length, 4, "each card has Allow, chat, always and Deny");
    // Answering one touches no other card.
    shown[0].buttons()[0].onclick();
    await flush();
    assert.ok(shown[0].buttons().every((b) => b.disabled));
    assert.ok(shown[1].buttons().every((b) => !b.disabled));
    assert.ok(shown[2].buttons().every((b) => !b.disabled));
  }

  // An answer the hub took is "Sent" until Hermes settles it; then it is answered.
  {
    const { tracker, log, shown } = harness();
    tracker.offer(prompt("a", "cleanup a"));
    shown[0].buttons()[0].onclick();
    await flush();
    assert.deepStrictEqual(log, [{ method: "POST", path: "/api/sessions/b/s/prompts/a/answer", body: { choice: "once" } }]);
    assert.ok(shown[0].text().includes("Sent. Waiting for Hermes to confirm."));
    tracker.settled("a");
    assert.ok(shown[0].text().includes("Answered"));
    assert.strictEqual(shown[0].buttons().length, 0);
    tracker.outcome("a", "Allowed once (in the hub)");
    assert.ok(shown[0].text().includes("Allowed once (in the hub)"));
  }

  // A prompt Hermes closed that this page did not answer is not "Answered".
  {
    const { tracker, shown } = harness();
    tracker.offer(prompt("a", "cleanup a"));
    tracker.settled("a");
    const t = shown[0].text();
    assert.ok(!t.includes("Answered"), t);
    assert.ok(t.includes("Closed by Hermes"), t);
  }

  // An answer Hermes does not confirm re-offers the buttons with a note.
  {
    const timers = [];
    const { tracker, shown } = harness({ setTimeout: (fn, ms) => timers.push({ fn, ms }) - 1, clearTimeout: () => {}, ackMs: 7000 });
    tracker.offer(prompt("a", "cleanup a"));
    shown[0].buttons()[0].onclick();
    await flush();
    assert.strictEqual(timers[0].ms, 7000);
    assert.ok(shown[0].buttons().every((b) => b.disabled));
    timers[0].fn();
    assert.ok(shown[0].buttons().every((b) => !b.disabled));
    assert.ok(shown[0].text().includes("Hermes has not confirmed this answer"));
  }

  // A confirmation that comes in time cancels the retry offer.
  {
    const cleared = [];
    const { tracker, shown } = harness({ setTimeout: () => 42, clearTimeout: (id) => cleared.push(id) });
    tracker.offer(prompt("a", "cleanup a"));
    shown[0].buttons()[1].onclick();
    await flush();
    tracker.settled("a");
    assert.deepStrictEqual(cleared, [42]);
  }

  // A failed send says so on the card and keeps the buttons; a 404 closes it.
  {
    let fail = { message: "network down" };
    const { tracker, shown, banners } = harness({ api: async () => { throw fail; } });
    tracker.offer(prompt("a", "cleanup a"));
    shown[0].buttons()[0].onclick();
    await flush();
    assert.ok(shown[0].buttons().every((b) => !b.disabled));
    assert.ok(shown[0].text().includes("Not delivered: network down"));
    assert.strictEqual(banners.length, 1);
    fail = { status: 404, message: "gone" };
    shown[0].buttons()[0].onclick();
    await flush();
    assert.ok(shown[0].text().includes("No longer waiting"));
    assert.strictEqual(shown[0].buttons().length, 0);
  }

  // Prompts the hub says Hermes lost close without an answer; others stay.
  {
    const { tracker, shown } = harness();
    for (const id of ["a", "b", "c"]) tracker.offer(prompt(id, "cleanup " + id));
    tracker.lost(["a", "c"]);
    assert.ok(shown[0].text().includes("Hermes no longer waits"));
    assert.ok(shown[2].text().includes("Hermes no longer waits"));
    assert.strictEqual(shown[0].buttons().length, 0);
    assert.strictEqual(shown[1].buttons().length, 4);
    tracker.cancelled(["b"]);
    assert.ok(shown[1].text().includes("Cancelled"));
  }

  // Questions keep their answer box and Skip.
  {
    const { tracker, shown, log } = harness();
    tracker.offer({ kind: "clarify", prompt_id: "q", question: "Which?", choices: ["x", "y"] });
    const names = shown[0].buttons().map((b) => b.textContent);
    assert.deepStrictEqual(names, ["Answer", "x", "y", "Skip"]);
    shown[0].buttons()[1].onclick();
    await flush();
    assert.deepStrictEqual(log[0].body, { picks: ["x"] });
  }

  // Plain approvals with no choices get Allow and Deny, sent as allow.
  {
    const { tracker, shown, log } = harness();
    tracker.offer({ kind: "approval", prompt_id: "p", command: "ls" });
    assert.deepStrictEqual(shown[0].buttons().map((b) => b.textContent), ["Allow", "Deny"]);
    shown[0].buttons()[1].onclick();
    await flush();
    assert.deepStrictEqual(log[0], { method: "POST", path: "/api/sessions/b/s/approvals/p", body: { allow: false } });
  }
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
