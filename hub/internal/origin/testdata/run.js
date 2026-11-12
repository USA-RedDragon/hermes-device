"use strict";
// Runs internal/web/static/origin.js against cases.json, as origin_test.go
// runs Classify.
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const args = process.argv.slice(2);
const cases = args.pop();
const sandbox = { window: {} };
for (const s of args) vm.runInNewContext(fs.readFileSync(s, "utf8"), sandbox);
const { classify } = sandbox.window.HubOrigin;
const all = JSON.parse(fs.readFileSync(cases, "utf8"));
for (const c of all) {
  const got = classify(c.row);
  const want = c.want;
  assert.strictEqual(got.show, want.show, c.name + ": show");
  assert.strictEqual(got.text || "", want.text || "", c.name + ": text");
  assert.strictEqual(got.why, want.why, c.name + ": why");
  if (!want.notice) assert.ok(!got.notice, c.name + ": want no card");
  else {
    assert.ok(got.notice, c.name + ": want a card");
    assert.strictEqual(got.notice.kind, want.notice.kind, c.name + ": card kind");
    assert.strictEqual(got.notice.title, want.notice.title, c.name + ": card title");
  }
  // A typed send is never read by its text.
  const typed = classify({ text: c.row.text, submission_id: "web-typed" });
  assert.strictEqual(typed.show, "user", c.name + ": typed send");
  assert.strictEqual(typed.text, c.row.text, c.name + ": typed text");
}
// The shapes rows come in.
assert.strictEqual(classify({ role: "user", content: [{ type: "text", text: "hi" }], display_metadata: "{\"submission_id\":\"web-1\"}" }).why, "submission");
assert.strictEqual(classify({ text: "x", display_metadata: { submission_id: "hermes-hub-agent-am_1" } }).show, "card");
console.log("ok", all.length);
