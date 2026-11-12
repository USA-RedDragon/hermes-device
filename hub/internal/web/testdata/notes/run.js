"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const [script, cases] = process.argv.slice(2);
const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(script, "utf8"), sandbox);
const list = JSON.parse(fs.readFileSync(cases, "utf8"));
assert.ok(list.length > 0, "no cases");
for (const c of list) {
  assert.strictEqual(sandbox.window.HubAttach.cutNotes(c.text), c.want, c.name);
  assert.strictEqual(sandbox.window.HubAttach.split(c.text).text, sandbox.window.HubAttach.split(c.want).text, "split: " + c.name);
}
