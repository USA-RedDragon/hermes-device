"use strict";
const assert = require("assert");
const { load } = require("./harness.js");

const w = load([process.argv[2]]);
const R = w.win.HubReconcile;

const id = (n) => "hub:0000000" + n + "-aaaa-4bbb-8ccc-dddddddddddd";

// A snapshot lands while the page is partway: one message it sent started
// already, one is queued at the hub, one is steering the running turn, one
// the hub does not know yet, and one was answered and is no longer carried.
const local = [
  { id: id(1), inflight: true, text: "started" },
  { id: id(2), inflight: true, text: "queued" },
  { id: id(3), inflight: true, text: "steering" },
  { id: id(4), inflight: true, text: "unknown" },
  { id: id(5), inflight: false, text: "answered" },
];
const snap = {
  messages: [{ role: "user", text: "old" }, { role: "user", text: "started", submission_id: id(1) }],
  pending: [
    { id: "adm-9", status: "queued", text: "other tab" },
    { id: "adm-2", status: "queued", text: "queued", submission_id: id(2) },
    { id: "adm-3", status: "steering", text: "steering", submission_id: id(3) },
  ],
};
const got = JSON.parse(JSON.stringify(R.carry(local, snap).map((c) => [c.p.text, c.act, c.at === undefined ? null : c.at])));
assert.deepStrictEqual(got, [["started", "done", null], ["queued", "adopt", 1], ["steering", "adopt", 2], ["unknown", "again", null]]);

// A message the hub accepted (a steer into the running turn, a queued one)
// is not in Hermes's transcript until its turn ends. A snapshot that does
// not list it, the hub's older copy or another replica's, must not take
// its bubble away: it is carried while it is recent, unless the transcript
// has it by now.
const now = 1000000;
const accepted = [
  { id: id(6), inflight: false, text: "steered in", at: now - 5000 },
  { id: id(7), inflight: false, text: "stored by now", at: now - 5000 },
  { id: id(8), inflight: false, text: "long ago", at: now - 11 * 60 * 1000 },
  { id: id(9), inflight: false, text: "listed", at: now - 5000 },
  { id: id(10), inflight: false, text: "no clock" },
];
const snap2 = {
  messages: [{ role: "user", text: "stored by now " }],
  pending: [{ id: "adm-9", status: "steering", text: "listed", submission_id: id(9) }],
};
const got2 = JSON.parse(JSON.stringify(R.carry(accepted, snap2, now).map((c) => [c.p.text, c.act])));
assert.deepStrictEqual(got2, [["steered in", "again"], ["stored by now", "done"], ["listed", "adopt"]]);

// A pending input is known by the ID the page sent it under, else by the hub's.
assert.strictEqual(R.idOf({ id: "adm-2", submission_id: id(2) }), id(2));
assert.strictEqual(R.idOf({ id: "adm-9" }), "adm-9");

// An older hub that has no submission_id still maps the one the page knows by the hub's id.
assert.strictEqual(R.carry([{ id: "adm-2", inflight: true }], { pending: [{ id: "adm-2" }] })[0].act, "adopt");

// Scroll: a reader at the end is not anchored, one above is kept on the row in view.
const log = { scrollHeight: 5000, scrollTop: 1200 };
assert.strictEqual(R.anchor(log, true, { row: "7", off: -40 }), null);
const kept = R.anchor(log, false, { row: "7", off: -40 });
assert.strictEqual(kept.dist, 3800);
log.scrollHeight = 5400;
log.scrollTop = 0;
// The row is drawn again 300 px lower than it was: the reader follows it, whatever came after it.
assert.strictEqual(R.restore(log, kept, (p) => (p.row === "7" ? -40 + 300 : null)), true);
assert.strictEqual(log.scrollTop, 300);
// Without the row, the distance to the end stands.
log.scrollTop = 0;
assert.strictEqual(R.restore(log, kept, () => null), true);
assert.strictEqual(log.scrollTop, 1600);
// A page with no row to name keeps the distance too.
const bare = R.anchor({ scrollHeight: 5000, scrollTop: 1200 }, false, null);
log.scrollTop = 0;
assert.strictEqual(R.restore(log, bare), true);
assert.strictEqual(log.scrollTop, 1600);
assert.strictEqual(R.restore(log, null), false);
log.scrollHeight = 100;
R.restore(log, bare);
assert.strictEqual(log.scrollTop, 0);
console.log("ok");
