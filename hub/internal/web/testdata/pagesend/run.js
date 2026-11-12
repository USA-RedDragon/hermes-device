"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const [script, bridge] = process.argv.slice(2);
const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(script, "utf8"), sandbox);
const { mark, split, sender } = sandbox.window.HubPageSend;
const plain = (v) => JSON.parse(JSON.stringify(v));

async function main() {
  assert.strictEqual(mark("Tool cards glow-up", "picked 2"), "[Answer from the page \"Tool cards glow-up\"]\npicked 2");
  assert.strictEqual(mark("A \"quoted\"\n title", "x"), "[Answer from the page \"A 'quoted' title\"]\nx");
  assert.strictEqual(mark("", "x"), "[Answer from the page \"untitled\"]\nx");
  assert.deepStrictEqual(plain(split(mark("Report", "line1\nline2"))), { page: "Report", text: "line1\nline2" });
  assert.deepStrictEqual(plain(split("[Answer from the page \"Report\"] not at line end")), { page: "", text: "[Answer from the page \"Report\"] not at line end" });
  assert.deepStrictEqual(plain(split("hello")), { page: "", text: "hello" });

  const run = (deliver) => {
    const log = { sent: [], states: [], replies: [] };
    const s = sender({
      title: () => "Pick",
      deliver: (t) => { log.sent.push(t); return deliver(t); },
      state: (st) => log.states.push(st),
      reply: (id, r) => log.replies.push([id, r]),
    });
    return { s, log };
  };

  {
    const { s, log } = run(async () => {});
    await s.send(1, "go");
    assert.deepStrictEqual(plain(log.sent), ["[Answer from the page \"Pick\"]\ngo"]);
    assert.deepStrictEqual(plain(log.replies), [[1, { ok: true }]]);
    assert.ok(log.states[0].sending && typeof log.states[1].sent.getTime() === "number");
  }

  {
    let release;
    const { s, log } = run(() => new Promise((r) => { release = r; }));
    const first = s.send(1, "a");
    await s.send(2, "a");
    assert.deepStrictEqual(plain(log.replies), [[2, { ok: false, error: "Already sending" }]]);
    release();
    await first;
    assert.strictEqual(log.sent.length, 1);
    await s.send(3, "b");
    release();
    assert.strictEqual(log.sent.length, 2);
  }

  {
    let fail = true;
    const { s, log } = run(async () => { if (fail) throw new Error("backend down"); });
    await s.send(1, "go");
    const st = log.states.at(-1);
    assert.strictEqual(st.failed.message, "backend down");
    assert.strictEqual(st.text, "go");
    assert.deepStrictEqual(plain(log.replies[0]), [1, { ok: false, error: "backend down" }]);
    fail = false;
    await st.retry();
    assert.strictEqual(log.sent.length, 2);
    assert.ok(log.states.at(-1).sent);
  }

  {
    const { s, log } = run(async () => {});
    await s.send(1, "  \n");
    assert.strictEqual(log.sent.length, 0);
    assert.strictEqual(log.replies[0][1].ok, false);
  }

  bridgeTests();
}

function bridgeTests() {
  const html = fs.readFileSync(bridge, "utf8");
  const code = html.match(/<script>([\s\S]*?)<\/script>/)[1];
  const make = (embedded) => {
    const listeners = {};
    const win = { parent: null };
    win.addEventListener = (t, f) => { (listeners[t] = listeners[t] || []).push(f); };
    win.document = { createElement: () => ({ setAttribute() {}, style: {}, select() {}, remove() {} }), body: { appendChild() {} }, execCommand: () => true };
    win.parent = embedded ? {} : win;
    const ctx = Object.assign(win, { window: win, parent: win.parent, document: win.document, addEventListener: win.addEventListener, Object, Promise, String, Object });
    vm.runInNewContext(code, ctx);
    return { win, listeners };
  };

  const { win, listeners } = make(true);
  const posted = [];
  const port = { postMessage: (m) => posted.push(m), onmessage: null };
  const parent = win.parent;
  listeners.message[0]({ source: parent, data: { type: "hermes-page:init" }, ports: [port], stopImmediatePropagation() {} });
  const p1 = win.hermes.send("one");
  win.hermes.draft("two");
  assert.deepStrictEqual(plain(posted), [{ type: "send", id: 1, text: "one" }, { type: "draft", id: 2, text: "two" }]);
  port.onmessage({ data: { type: "result", id: 1, ok: false, error: "boom" } });
  return p1.then((r) => {
    assert.deepStrictEqual(plain(r), { ok: false, error: "boom" });
    const click = listeners.click[0];
    const el = (attr, v) => ({ closest: () => ({ hasAttribute: (a) => a === attr, getAttribute: () => v }) });
    click({ target: el("data-hermes-send", "s"), preventDefault() {} });
    click({ target: el("data-hermes-draft", "d"), preventDefault() {} });
    assert.deepStrictEqual(plain(posted.slice(2)), [{ type: "send", id: 3, text: "s" }, { type: "draft", id: 4, text: "d" }]);

    const alone = make(false);
    return alone.win.hermes.send("lost").then((r2) => {
      assert.strictEqual(r2.ok, false);
      assert.match(r2.error, /not open in the Talaria hub/);
    });
  });
}

main().catch((e) => { console.error(e); process.exit(1); });
