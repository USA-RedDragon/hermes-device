"use strict";
// Runs plain.js: node run.js plain.js
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {}, String };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const P = sandbox.window.HubPlain;

const cases = {
  web_search: "Web search",
  code_execution: "Code execution",
  "systematic-debugging": "Systematic debugging",
  mcp_servers: "MCP servers",
  "agent.locked_keys": "Agent locked keys",
  "openai-codex": "OpenAI codex",
  "ascii-video": "ASCII video",
  "Already Fine": "Already Fine",
  "Claude": "Claude",
  "": "",
};
for (const [id, want] of Object.entries(cases)) assert.strictEqual(P.text(id), want, id);
assert.strictEqual(P.text(null), "");
assert.strictEqual(P.text(undefined), "");
// An unknown id never comes back as it went in.
for (const id of ["brand_new_toolset", "some.dotted.key", "third_party_tool"]) {
  assert.ok(!/[_.]/.test(P.text(id)), id);
}
// Sentences with ids in them are dropped; plain ones stay.
assert.strictEqual(P.clean("web_search, web_extract"), "");
assert.strictEqual(P.clean("approvals.mode is locked"), "");
assert.strictEqual(P.clean("The model is busy"), "The model is busy");
assert.strictEqual(P.clean(undefined), "");
