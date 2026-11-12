"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const [script, cases] = process.argv.slice(2);
const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(script, "utf8"), sandbox);
const split = sandbox.window.HubAttach.split;
for (const c of JSON.parse(fs.readFileSync(cases, "utf8"))) {
  assert.deepStrictEqual(JSON.parse(JSON.stringify(split(c.text))), c.want, c.name);
}
