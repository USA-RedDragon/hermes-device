"use strict";
// HubSend sends a message so that losing the connection cannot send it
// twice or lose it. Every message gets an ID of its own (newID), and the
// hub runs a message with an ID it has already seen once. So a request
// that failed without an answer, which may or may not have arrived, is
// sent again under the same ID, after the waits of HubBackoff, until it
// gets an answer. An answer with an error status ends it: the hub has
// looked at the message and said no.
window.HubSend = (() => {
  const hex = (n) => Array.from({ length: n }, () => Math.floor(Math.random() * 16).toString(16)).join("");

  function newID() {
    const c = typeof crypto !== "undefined" ? crypto : null;
    if (c && c.randomUUID) return "hub:" + c.randomUUID();
    if (c && c.getRandomValues) {
      const b = c.getRandomValues(new Uint8Array(16));
      b[6] = (b[6] & 0x0f) | 0x40;
      b[8] = (b[8] & 0x3f) | 0x80;
      const x = Array.from(b, (v) => v.toString(16).padStart(2, "0")).join("");
      return "hub:" + [x.slice(0, 8), x.slice(8, 12), x.slice(12, 16), x.slice(16, 20), x.slice(20)].join("-");
    }
    return "hub:" + [hex(8), hex(4), "4" + hex(3), "8" + hex(3), hex(12)].join("-");
  }

  // answered tells an error that came from the hub, or a login that is
  // needed, from one where no answer came.
  const answered = (e) => !!e && (e.status !== undefined || e.message === "login required");

  // post runs call() until it gets an answer. o.stop() ends it early,
  // o.retry(n, e) hears about each new try, o.limit is how long (ms) it
  // tries before it gives up with the last error, o.backoff the waits.
  async function post(call, o) {
    const opt = o || {};
    const backoff = opt.backoff || window.HubBackoff();
    const limit = opt.limit || 10 * 60000;
    const from = Date.now();
    for (let n = 1; ; n++) {
      try {
        return await call();
      } catch (e) {
        if (answered(e) || (opt.stop && opt.stop()) || Date.now() - from >= limit) {
          if (!answered(e)) e.mayBeSent = true;
          throw e;
        }
        if (opt.retry) opt.retry(n, e);
        await new Promise((done) => setTimeout(done, backoff.next()));
        if (opt.stop && opt.stop()) {
          e.mayBeSent = true;
          throw e;
        }
      }
    }
  }

  return { newID, post, answered };
})();
