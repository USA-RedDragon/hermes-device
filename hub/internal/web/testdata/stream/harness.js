"use strict";
const fs = require("fs");
const vm = require("vm");

// load runs the page scripts in a sandbox with a clock and timers that
// only move when the test says so, and an EventSource that the test drives.
exports.load = (files, extra) => {
  let now = 1000000;
  let seq = 0;
  const timers = new Map();
  const sources = [];
  class EventSource {
    constructor(url) {
      this.url = url;
      this.closed = false;
      sources.push(this);
    }
    close() { this.closed = true; }
    open() { if (!this.closed && this.onopen) this.onopen(); }
    fail() { if (!this.closed && this.onerror) this.onerror(); }
    send(id, data) { if (!this.closed && this.onmessage) this.onmessage({ lastEventId: id, data: data || "{}" }); }
  }
  const sandbox = {
    window: {},
    EventSource,
    Date: { now: () => now },
    setTimeout: (fn, ms) => { const id = ++seq; timers.set(id, { at: now + (ms || 0), fn }); return id; },
    clearTimeout: (id) => { timers.delete(id); },
    Math,
    Promise,
    Array,
    Set,
    Map,
    Uint8Array,
    ...(extra || {}),
  };
  vm.createContext(sandbox);
  for (const f of files) vm.runInContext(fs.readFileSync(f, "utf8"), sandbox);
  const advance = (ms) => {
    const to = now + ms;
    for (;;) {
      let next = null;
      for (const [id, t] of timers) if (t.at <= to && (!next || t.at < next.t.at || (t.at === next.t.at && id < next.id))) next = { id, t };
      if (!next) break;
      timers.delete(next.id);
      now = Math.max(now, next.t.at);
      next.t.fn();
    }
    now = to;
  };
  return { win: sandbox.window, sources, advance, now: () => now, pending: () => timers.size };
};
