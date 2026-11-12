"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const script = process.argv[2];

const node = (tag) => ({
  tag,
  children: [],
  append(...k) { this.children.push(...k); },
  set textContent(v) { this.text = v; },
  get textContent() { return this.text; },
});
const sandbox = {
  window: {},
  URL,
  document: { createElement: node, createTextNode: (text) => ({ text }) },
};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(script, "utf8"), sandbox);
const L = sandbox.window.HubLinkify;

const plainly = (v) => JSON.parse(JSON.stringify(v));
const links = (text) => plainly(L.split(text).filter((p) => typeof p !== "string").map((p) => p.text));
const hrefs = (text) => plainly(L.split(text).filter((p) => typeof p !== "string").map((p) => p.href));

const cases = [
  ["see https://example.com/a.", ["https://example.com/a"]],
  ["go to https://example.com/a, then", ["https://example.com/a"]],
  ["really? https://example.com/a?!", ["https://example.com/a"]],
  ["(https://example.com/a)", ["https://example.com/a"]],
  ["[https://example.com/a]", ["https://example.com/a"]],
  ["{https://example.com/a}", ["https://example.com/a"]],
  ['"https://example.com/a"', ["https://example.com/a"]],
  ["'https://example.com/a'.", ["https://example.com/a"]],
  ["https://en.wikipedia.org/wiki/Go_(language)", ["https://en.wikipedia.org/wiki/Go_(language)"]],
  ["(see https://en.wikipedia.org/wiki/Go_(language)).", ["https://en.wikipedia.org/wiki/Go_(language)"]],
  ["https://example.com/a;", ["https://example.com/a"]],
  ["https://example.com/a:", ["https://example.com/a"]],
  ["https://example.com/q?a=1&b=2#frag", ["https://example.com/q?a=1&b=2#frag"]],
  ["one https://a.example.com two http://b.example.com", ["https://a.example.com", "http://b.example.com"]],
  ["www.example.com/x.", ["www.example.com/x"]],
  ["https://example.com\nhttps://example.org", ["https://example.com", "https://example.org"]],
  ["http://localhost:8080/x", ["http://localhost:8080/x"]],
  ["no links here, just a.b and 3.5", []],
  ["https://", []],
  ["https://nodot", []],
  ["javascript:alert(1)", []],
  ["data:text/html,<script>alert(1)</script>", []],
  ["file:///etc/passwd", []],
  ["ftp://example.com/x", []],
  ["xhttps://example.com", []],
  ["mail me@www.example.com", []],
  ["JAVASCRIPT:alert(1) https://ok.example.com", ["https://ok.example.com"]],
];
for (const [text, want] of cases) assert.deepStrictEqual(links(text), want, text);

assert.deepStrictEqual(hrefs("www.example.com/x."), ["https://www.example.com/x"]);
assert.deepStrictEqual(hrefs("HTTPS://Example.com/A"), ["https://example.com/A"]);

const rest = L.split("a (https://example.com/a). b");
assert.deepStrictEqual(JSON.parse(JSON.stringify(rest)), ["a (", { text: "https://example.com/a", href: "https://example.com/a" }, "). b"]);
assert.strictEqual(L.split("plain text").join(""), "plain text");
assert.strictEqual(L.split("").length, 0);

const hostile = '<img src=x onerror=alert(1)> https://example.com/"><script>alert(1)</script>';
const root = node("div");
L.append(root, hostile);
const flat = [];
const walk = (n) => { flat.push(n); (n.children || []).forEach(walk); };
walk(root);
assert.ok(flat.every((n) => n.tag === undefined || n.tag === "div" || n.tag === "a"), "only anchors are created");
assert.ok(!flat.some((n) => n.tag === "script" || n.tag === "img"));
const a = flat.filter((n) => n.tag === "a");
assert.strictEqual(a.length, 1);
assert.ok(a[0].href.startsWith("https://example.com/"));
assert.strictEqual(a[0].target, "_blank");
assert.strictEqual(a[0].rel, "noopener noreferrer");
const shown = root.children.map((c) => (c.tag === "a" ? c.textContent : c.text)).join("");
assert.strictEqual(shown, hostile);

const big = "https://example.com/" + "a".repeat(5000);
assert.deepStrictEqual(links(big), [big]);
const plain = "x ".repeat(20000);
assert.deepStrictEqual(links(plain), []);
