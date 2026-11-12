"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const [script, cases] = process.argv.slice(2);
const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(script, "utf8"), sandbox);
const parse = sandbox.window.HubNotice.parse;
for (const c of JSON.parse(fs.readFileSync(cases, "utf8"))) {
  const got = parse(c.text);
  assert.deepStrictEqual(JSON.parse(JSON.stringify(got === undefined ? null : got)), c.want, c.name);
}
