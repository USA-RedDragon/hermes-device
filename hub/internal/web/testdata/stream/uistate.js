"use strict";
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");

// A small DOM: elements with a tag, classes, data and attributes, and the
// few selectors HubUIState uses.
class Node {
  constructor(tag, cls, parent) {
    this.tagName = tag.toUpperCase();
    this.classList = { s: new Set(cls ? cls.split(" ") : []), contains(c) { return this.s.has(c); } };
    this.dataset = {};
    this.attrs = {};
    this.kids = [];
    this.nodeType = 1;
    this.text = "";
    this.open = false;
    this.parentNode = parent || null;
    if (parent) parent.kids.push(this);
  }
  get parentElement() { return this.parentNode; }
  get textContent() { return this.text + this.kids.map((k) => k.textContent).join(""); }
  getAttribute(n) { return this.attrs[n] === undefined ? null : this.attrs[n]; }
  setAttribute(n, v) { this.attrs[n] = v; }
  is(sel) {
    return sel.split(",").some((p) => {
      const m = p.trim().match(/^([a-z]*)(\.[\w-]+)?(\[[\w-]+\])?$/);
      if (!m) throw new Error("selector " + p);
      if (m[1] && this.tagName !== m[1].toUpperCase()) return false;
      if (m[2] && !this.classList.contains(m[2].slice(1))) return false;
      if (m[3]) {
        const a = m[3].slice(1, -1);
        if (a === "data-ukey" ? this.dataset.ukey === undefined : this.attrs[a] === undefined) return false;
      }
      return true;
    });
  }
  matches(sel) { return this.is(sel); }
  closest(sel) { for (let n = this; n; n = n.parentNode) if (n.is(sel)) return n; return null; }
  all() { return this.kids.flatMap((k) => [k, ...k.all()]); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) { return this.all().filter((n) => n.is(sel)); }
  contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; }
  click() { if (this.onclick) this.onclick(); this.fire(); }
  fire() {}
}

const timers = [];
const sandbox = { window: {}, setTimeout: (f) => timers.push(f), Node };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const flush = () => { while (timers.length) timers.shift()(); };

const log = new Node("div", "log");
const S = sandbox.window.HubUIState(log);
const click = (n) => { if (n.tagName === "SUMMARY") n.parentNode.open = !n.parentNode.open; else n.click(); S.record({ target: n }); flush(); };

// A tool card: its head toggles aria-expanded.
function card(parent, id) {
  const c = new Node("div", "tool", parent);
  c.dataset.ukey = "t:" + id;
  const head = new Node("button", "", c);
  head.attrs["aria-expanded"] = "false";
  head.onclick = () => { head.attrs["aria-expanded"] = head.attrs["aria-expanded"] === "true" ? "false" : "true"; };
  return { c, head };
}
function group(parent, firstID) {
  const d = new Node("details", "toolgroup", parent);
  d.dataset.ukey = "g:t:" + firstID;
  const sum = new Node("summary", "", d);
  const body = new Node("div", "group-body", d);
  return { d, sum, body };
}

// The reader opens a group and a card in it; the page draws both again and they open again.
{
  const g1 = group(log, "a");
  const c1 = card(g1.body, "a");
  const c2 = card(g1.body, "b");
  click(g1.sum);
  click(c2.head);
  assert.strictEqual(g1.d.open, true);
  assert.strictEqual(c2.head.getAttribute("aria-expanded"), "true");
  log.kids.length = 0;
  const g2 = group(log, "a");
  const d1 = card(g2.body, "a");
  const d2 = card(g2.body, "b");
  const d3 = card(g2.body, "c");
  S.apply(g2.d);
  assert.strictEqual(g2.d.open, true);
  assert.strictEqual(d2.head.getAttribute("aria-expanded"), "true");
  assert.strictEqual(d1.head.getAttribute("aria-expanded"), "false");
  assert.strictEqual(d3.head.getAttribute("aria-expanded"), "false", "a card that joined later has no state");
  click(d2.head);
  const g3 = group(log, "a");
  card(g3.body, "a");
  const e2 = card(g3.body, "b");
  S.apply(g3.d);
  assert.strictEqual(e2.head.getAttribute("aria-expanded"), "false", "the reader closed it again");
  assert.strictEqual(g3.d.open, true);
}

// A card moved into a group keeps what it shows, and the apply does not count as the reader.
{
  log.kids.length = 0;
  S.saved.clear();
  const c = card(log, "x");
  click(c.head);
  assert.strictEqual(S.expanded(c.c), true);
  const g = group(log, "x");
  g.body.kids.push(c.c);
  c.c.parentNode = g.body;
  assert.strictEqual(S.expanded(g.body), true);
  const before = S.saved.size;
  S.apply(g.d);
  flush();
  assert.strictEqual(S.saved.size, before);
}

// A thinking block is known by the start of its text; the page's own close leaves an opened one.
{
  log.kids.length = 0;
  S.saved.clear();
  const mk = () => {
    const d = new Node("details", "reasoning", log);
    d.open = true;
    const s = new Node("summary", "", d);
    s.text = "Thinking";
    const b = new Node("div", "body", d);
    b.text = "I should look at the first file and then the second one.";
    return { d, s };
  };
  const a = mk();
  S.closeUnlessKept(a.d);
  assert.strictEqual(a.d.open, false, "closed by the page");
  a.d.open = true;
  click(a.s);
  click(a.s);
  assert.strictEqual(a.d.open, true, "toggled twice: still as the reader left it, open");
  const again = mk();
  again.d.open = false;
  S.apply(again.d);
  assert.strictEqual(again.d.open, true);
  const kept = mk();
  S.closeUnlessKept(kept.d);
  assert.strictEqual(kept.d.open, true, "the reader's open state survives the page's close");
  log.kids.length = 0;
  const closed = mk();
  click(closed.s);
  assert.strictEqual(closed.d.open, false);
  const next = mk();
  S.closeUnlessKept(next.d);
  S.apply(next.d);
  assert.strictEqual(next.d.open, false);
}

// A notice card and its Show full result button, by a hash of the text.
{
  log.kids.length = 0;
  S.saved.clear();
  const mk = () => {
    const n = new Node("div", "notice", log);
    n.dataset.ukey = "n:" + S.hash("the notice text");
    const head = new Node("button", "notice-head", n);
    head.attrs["aria-expanded"] = "false";
    head.onclick = () => { head.attrs["aria-expanded"] = head.attrs["aria-expanded"] === "true" ? "false" : "true"; };
    const more = new Node("button", "notice-more", n);
    more.text = "Show full result";
    more.onclick = () => { more.text = more.text === "Show less" ? "Show full result" : "Show less"; };
    return { n, head, more };
  };
  const a = mk();
  click(a.head);
  click(a.more);
  log.kids.length = 0;
  const b = mk();
  S.apply(b.n);
  assert.strictEqual(b.head.getAttribute("aria-expanded"), "true");
  assert.strictEqual(b.more.text, "Show less");
}
assert.strictEqual(S.hash("a"), S.hash("a"));
assert.notStrictEqual(S.hash("a"), S.hash("b"));
console.log("ok");
