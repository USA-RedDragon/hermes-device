"use strict";
// A small DOM for the node tests: elements with classes, attributes, children and text.
class Text {
  constructor(t) { this.data = String(t); this.parentNode = null; }
  get textContent() { return this.data; }
  remove() { if (this.parentNode) this.parentNode._drop(this); }
}
class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.childNodes = [];
    this.parentNode = null;
    this.className = "";
    this.dataset = {};
    this.attrs = {};
    this.hidden = false;
    this.open = false;
    this.listeners = {};
    this.style = {};
    this._top = 0;
    this.clientHeight = 200;
    const self = this;
    this.classList = {
      add: (...c) => { for (const x of c) if (!self._cls().includes(x)) self.className = (self.className + " " + x).trim(); },
      remove: (...c) => { self.className = self._cls().filter((x) => !c.includes(x)).join(" "); },
      contains: (c) => self._cls().includes(c),
      toggle: (c, on) => { if (on === undefined) on = !self._cls().includes(c); if (on) self.classList.add(c); else self.classList.remove(c); return on; },
    };
  }
  _cls() { return this.className.split(/\s+/).filter(Boolean); }
  _drop(n) { this.childNodes = this.childNodes.filter((x) => x !== n); n.parentNode = null; }
  _adopt(n) { if (typeof n === "string" || typeof n === "number") n = new Text(n); if (n.parentNode) n.parentNode._drop(n); n.parentNode = this; return n; }
  get children() { return this.childNodes.filter((n) => n instanceof El); }
  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
  get previousElementSibling() {
    if (!this.parentNode) return null;
    const k = this.parentNode.children;
    return k[k.indexOf(this) - 1] || null;
  }
  get isConnected() { return true; }
  get textContent() { return this.childNodes.map((n) => n.textContent).join(""); }
  set textContent(v) { this.childNodes = []; if (v !== "" && v !== undefined && v !== null) this.append(String(v)); }
  append(...k) { for (const n of k) this.childNodes.push(this._adopt(n)); }
  insertBefore(n, ref) { n = this._adopt(n); const i = ref ? this.childNodes.indexOf(ref) : -1; if (i < 0) this.childNodes.push(n); else this.childNodes.splice(i, 0, n); return n; }
  replaceChildren(...k) { for (const n of this.childNodes) n.parentNode = null; this.childNodes = []; this.append(...k); }
  replaceWith(n) { const p = this.parentNode; p.insertBefore(n, this); p._drop(this); }
  remove() { if (this.parentNode) this.parentNode._drop(this); }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
  dispatch(t) { for (const fn of this.listeners[t] || []) fn({}); }
  _match(part) {
    const m = part.match(/^([a-z]*)((?:\.[\w-]+)*)((?:\[[^\]]+\])*)$/i);
    if (!m) throw new Error("selector " + part);
    if (m[1] && this.tagName !== m[1].toUpperCase()) return false;
    for (const c of m[2].split(".").filter(Boolean)) if (!this._cls().includes(c)) return false;
    for (const a of m[3].match(/\[[^\]]+\]/g) || []) {
      const [, k, v] = a.match(/\[([\w-]+)="?([^"\]]*)"?\]/);
      if (this.getAttribute(k) !== v) return false;
    }
    return true;
  }
  matches(sel) { return this._match(sel); }
  querySelectorAll(sel) {
    const parts = sel.trim().split(/\s+/);
    const out = [];
    const walk = (n, ancestors) => {
      for (const c of n.children) {
        const chain = [...ancestors, c];
        if (c._match(parts[parts.length - 1])) {
          let i = parts.length - 2;
          for (let j = chain.length - 2; j >= 0 && i >= 0; j--) if (chain[j]._match(parts[i])) i--;
          if (i < 0) out.push(c);
        }
        walk(c, chain);
      }
    };
    walk(this, []);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  get scrollHeight() { return Math.max(this.clientHeight, this.children.length * 50); }
  get scrollTop() { return this._top; }
  set scrollTop(v) { this._top = Math.max(0, Math.min(v, this.scrollHeight - this.clientHeight)); }
}
const el = (tag, cls, text) => {
  const n = new El(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null && text !== "") n.append(String(text));
  return n;
};
const document = { createElement: (t) => new El(t), createDocumentFragment: () => new El("fragment") };
module.exports = { Text, El, el, document };
