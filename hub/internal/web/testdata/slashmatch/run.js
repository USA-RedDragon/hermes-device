"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const { match } = sandbox.window.HubSlash;

const cmds = [
  { name: "/title", description: "Set a title for the current session", args: "[name]", aliases: [] },
  { name: "/compress", description: "Compress conversation context", args: "[focus]", aliases: ["/compact"] },
  { name: "/context", description: "Show how full the context is", aliases: ["/ctx"] },
  { name: "/background", description: "Run a prompt in the background", aliases: ["/bg"] },
  { name: "/competitor-news-monitor", description: "Watch rivals", skill: true },
  { name: "/computer-use", description: "Drive the desktop", skill: true },
  { name: "/summarize", description: "Compress a long page into notes", skill: true },
];
const names = (q) => Array.from(match(cmds, q), (m) => m.c.name);

assert.deepStrictEqual(names("comp"), ["/compress", "/competitor-news-monitor", "/computer-use", "/summarize"]);
assert.deepStrictEqual(names("compa"), ["/compress"]);
assert.strictEqual(match(cmds, "compa")[0].alias, "/compact");
assert.strictEqual(match(cmds, "compress")[0].alias, "");
assert.deepStrictEqual(names("compact"), ["/compress"]);
assert.strictEqual(match(cmds, "compact")[0].alias, "/compact");
assert.deepStrictEqual(names("bg"), ["/background"]);
assert.deepStrictEqual(names("ctx"), ["/context"]);
assert.deepStrictEqual(names("COMPRESS"), ["/compress", "/summarize"]);
assert.deepStrictEqual(names("ntext"), ["/context", "/compress"]);
assert.deepStrictEqual(names("notes"), ["/summarize"]);
assert.deepStrictEqual(names("title"), ["/title"]);
assert.deepStrictEqual(names("zzz"), []);
assert.deepStrictEqual(names("").length, cmds.length);
assert.deepStrictEqual(names("c").slice(0, 3), ["/compress", "/context", "/competitor-news-monitor"]);
assert.strictEqual(match(null, "x").length, 0);
console.log("ok");
