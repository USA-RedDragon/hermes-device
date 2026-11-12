"use strict";
// HubPageSend handles what a page sends to the chat. mark(title, text) puts
// the line that names the page before the text, so the model knows where
// the answer came from; split(text) takes the line off again and answers
// the page's title, for the tag on the message. sender(opts) is the hub
// side of hermes.send: it delivers one message at a time and answers the
// page with the result.
window.HubPageSend = (() => {
  const head = /^\[Answer from the page "([^"\n]*)"\]\n/;
  const clean = (title) => String(title || "").replace(/\s+/g, " ").replace(/"/g, "'").trim().slice(0, 80) || "untitled";

  const mark = (title, text) => "[Answer from the page \"" + clean(title) + "\"]\n" + text;

  function split(text) {
    const s = String(text || "");
    const m = s.match(head);
    return m ? { page: m[1], text: s.slice(m[0].length) } : { page: "", text: s };
  }

  // sender calls deliver(markedText) for a page's send. A send while one is
  // in flight is ignored, so a double click sends once. state(s) shows
  // { sending }, { sent: Date } or { failed: Error, text, retry }; reply
  // answers the page's call by its id.
  function sender({ title, deliver, state, reply }) {
    let busy = false;
    async function send(id, text) {
      if (!text.trim()) { reply(id, { ok: false, error: "Nothing to send" }); return; }
      if (busy) { reply(id, { ok: false, error: "Already sending" }); return; }
      busy = true;
      state({ sending: true });
      try {
        await deliver(mark(title(), text));
        state({ sent: new Date() });
        reply(id, { ok: true });
      } catch (e) {
        const err = e instanceof Error ? e : new Error(String(e));
        state({ failed: err, text, retry: () => send(0, text) });
        reply(id, { ok: false, error: err.message });
      } finally {
        busy = false;
      }
    }
    return { send };
  }

  return { mark, split, sender };
})();
