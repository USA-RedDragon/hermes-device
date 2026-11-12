"use strict";
const assert = require("assert");
const { load } = require("./harness.js");

const [backoff, send] = [process.argv[2], process.argv[3]];
const w = load([backoff, send], { crypto: undefined });
const S = w.win.HubSend;

// IDs look like the ones the hub accepts: "hub:" and 8 to 60 letters, digits and dashes.
const ok = (id) => /^hub:[A-Za-z0-9-]{8,60}$/.test(id) && id.length <= 64;
const ids = new Set();
for (let i = 0; i < 50; i++) { const id = S.newID(); assert.ok(ok(id), id); ids.add(id); }
assert.strictEqual(ids.size, 50);

const net = () => new TypeError("NetworkError when attempting to fetch resource.");
const http = (status, msg) => Object.assign(new Error(msg), { status });

(async () => {
  // The answer was lost twice: the same body, with the same ID, goes out three
  // times, and the page gets the one answer.
  {
    const id = S.newID();
    const sent = [];
    let fails = 2;
    const call = async () => {
      sent.push(JSON.stringify({ id, text: "hi" }));
      if (fails-- > 0) throw net();
      return { id, status: "sent" };
    };
    const retries = [];
    const p = S.post(call, { retry: (n) => retries.push(n), backoff: w.win.HubBackoff({ base: 500, rand: () => 1 }) });
    await Promise.resolve();
    for (let i = 0; i < 5; i++) { w.advance(1000); await Promise.resolve(); await Promise.resolve(); }
    const r = await p;
    assert.deepStrictEqual(r, { id, status: "sent" });
    assert.strictEqual(sent.length, 3);
    assert.strictEqual(new Set(sent).size, 1, "every try carries the same ID");
    assert.deepStrictEqual(retries, [1, 2]);
  }
  // An answer with an error status ends it at once: no retry.
  for (const e of [http(400, "bad message id"), http(502, "Bad Gateway"), Object.assign(new Error("login required"))]) {
    let n = 0;
    await assert.rejects(S.post(async () => { n++; throw e; }), (x) => x === e && !x.mayBeSent);
    assert.strictEqual(n, 1);
  }
  // It gives up after its limit, and says the message may have been sent.
  {
    let n = 0;
    const p = S.post(async () => { n++; throw net(); }, { limit: 3000, backoff: w.win.HubBackoff({ base: 500, rand: () => 1 }) });
    const done = p.catch((e) => e);
    for (let i = 0; i < 20; i++) { w.advance(1000); await Promise.resolve(); await Promise.resolve(); }
    const e = await done;
    assert.ok(e instanceof TypeError && e.mayBeSent === true);
    assert.ok(n >= 2 && n <= 6, "tries " + n);
  }
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
