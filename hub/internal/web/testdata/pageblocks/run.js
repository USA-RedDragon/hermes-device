"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {} };
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const { pageBlocks } = sandbox.window.HubPageBlocks;

// A 30 KB auth-screen mockup: template literals with ${}, nested backticks,
// lines that start with a backtick, and <template> tags.
const tick = "`";
let js = "";
for (let i = 0; i < 320; i++) {
  js += "  const row" + i + " = " + tick + "<li class=\"r\">${user" + i + ".name} " + tick + "${a" + i + "}" + tick + "</li>" + tick + ";\n";
  js += tick + "x" + i + " starts with a backtick ${n}" + tick + ";\n";
}
const html = "<!doctype html><title>Auth</title><template id=\"t\"><p>${x}</p></template>\n<script>\n" + js + "</script>\n";
assert.ok(html.length > 30000, "page is " + html.length);
const head = "Here is the mockup.\n\n";
const tail = "\n\nTell me what to change.";
const reply = head + "```page\n" + html + "```" + tail;

// Streamed in small chunks: the card is there as soon as the closing fence
// has arrived, before the text after it, and the wait note is not.
const open = head.length + "```page\n".length;
const close = open + html.length + 3;
let sawWriting = false;
let cardAt = -1;
for (let n = 1; n <= reply.length; n += 37) {
  const pg = pageBlocks(reply.slice(0, n), true);
  if (n > head.length && n < close) assert.strictEqual(pg.pages.length, 0, "no card before the fence at " + n);
  if (n > open && n < close) { assert.ok(pg.writing, "writing at " + n); sawWriting = true; }
  if (pg.pages.length && cardAt < 0) cardAt = n;
  if (pg.pages.length) assert.strictEqual(pg.writing, false, "no wait note once closed");
  assert.ok(!pg.text.includes("```"), "fence never leaks at " + n);
}
assert.ok(sawWriting);
assert.ok(cardAt >= close && cardAt < close + 37, "card at " + cardAt + " fence ends " + close);

// The closing fence arriving in the very chunk that ends the reply, and as
// the final snapshot.
for (const streaming of [true, false]) {
  const pg = pageBlocks(reply, streaming);
  assert.strictEqual(pg.pages.length, 1); assert.ok(pg.pages[0] === html.replace(/\n$/, ""), "page body " + pg.pages[0].length + " vs " + html.length);
  assert.strictEqual(pg.writing, false);
  assert.strictEqual(pg.text, "Here is the mockup.\n\nTell me what to change.");
}

// A fence ending the reply with no trailing newline, with CRLF, and with
// trailing spaces.
assert.strictEqual(pageBlocks("a\n```page\n<p>x</p>\n```", true).pages.length, 1);
assert.strictEqual(pageBlocks("a\r\n```page\r\n<p>x</p>\r\n```  \r\n", true).pages.length, 1);

// Still open: the wait note while streaming; as text once final.
assert.strictEqual(pageBlocks("a\n```page\n<p>x</p>\n", true).writing, true);
const un = pageBlocks("a\n```page\n<p>x</p>\n", false);
assert.deepStrictEqual([un.pages.length, un.writing], [0, false]);
assert.ok(un.text.includes("```page"));

// A longer fence holds a bare ``` line inside the page; a shorter one ends it.
const long = pageBlocks("````page\n<script>x = `\n```\n`</script>\n````\nafter", true);
assert.strictEqual(long.pages.length, 1);
assert.ok(long.pages[0].includes("\n```\n"));
assert.strictEqual(long.text, "after");
const short = pageBlocks("```page\n<script>x = `\n```\n`</script>\n```\nafter", false);
assert.strictEqual(short.pages[0], "<script>x = `");

// A second page after the first, still typing.
const two = pageBlocks("```page\n<p>1</p>\n```\nmid\n```page\n<p>2", true);
assert.strictEqual(two.pages.length, 1);
assert.strictEqual(two.writing, true);

// open: the reply ends inside a fence that is not closed.
const { open: isOpen } = sandbox.window.HubPageBlocks;
assert.strictEqual(isOpen("plain text"), false);
assert.strictEqual(isOpen("a\n```page\n<p>x"), true);
assert.strictEqual(isOpen("a\n```page\n<p>x</p>\n```\nb"), false);
assert.strictEqual(isOpen("a\n```js\nx\n```\n~~~page\ny"), true);
assert.strictEqual(isOpen("````md\n```\nx\n```\n"), true);

// A mockup with a template literal, "$ " content, a script close tag and a
// line that starts with a backtick, cut in two by a message that arrives
// while the reply streams. The second half alone has a closing fence and no
// opening one: that is the raw text and the empty code block. Together the
// halves are one page.
const mock = "<!doctype html><style>.p::before{content:\"$ \"}</style>\n<script>\nconst r = `<li>${a}</li>\n`;\n`x ${n}` + \"\u25cb \u00b7\";\n</script></body></html>";
const text = "Here:\n\n```page\n" + mock + "\n```\n\nDone.";
const at = text.indexOf("const r") + 10;
assert.strictEqual(isOpen(text.slice(0, at)), true, "the first half is inside the page block");
assert.strictEqual(pageBlocks(text.slice(0, at), true).writing, true);
const whole = pageBlocks(text, false);
assert.strictEqual(JSON.stringify(whole.pages), JSON.stringify([mock]));
assert.strictEqual(whole.text, "Here:\n\nDone.");
const rest = pageBlocks(text.slice(at), false);
assert.strictEqual(rest.pages.length, 0, "the second half alone is not a page");
console.log("ok");
