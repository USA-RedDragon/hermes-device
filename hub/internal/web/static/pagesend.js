"use strict";
// HubPageSend handles what a page sends to the chat. mark(title, text) puts
// the line that names the page before the text, so the model knows where
// the answer came from; split(text) takes the line off again and answers
// the page's title, for the tag on the message. sender(opts) is the hub
// side of hermes.send: it delivers one message at a time, only for a send
// that follows the user's own click or key press in the page (activation),
// and answers the page with the result.
window.HubPageSend = (() => {
  const head = /^\[Answer from the page "([^"\n]*)"\]\n/;
  const clean = (title) => String(title || "").replace(/\s+/g, " ").replace(/"/g, "'").trim().slice(0, 80) || "untitled";

  const mark = (title, text) => "[Answer from the page \"" + clean(title) + "\"]\n" + text;

  function split(text) {
    const s = String(text || "");
    const m = s.match(head);
    return m ? { page: m[1], text: s.slice(m[0].length) } : { page: "", text: s };
  }

  const noClick = "A page can send only right after your click or key press in it. Click its button again.";

  // sender calls deliver(markedText) for a page's send. send(id, text, fresh)
  // delivers only when fresh is true: the hub saw the user's own click or key
  // press in the page for this send (see activation). Else the send is
  // refused with an error, and the hub's line shows it without Retry, so
  // the user is never asked to press a button that sends text they have not
  // seen. A send while one is in flight is refused. state(s) shows
  // { sending }, { sent: Date } or { failed: Error, text, retry }; retry is
  // missing for a refused send. reply answers the page's call by its id.
  function sender({ title, deliver, state, reply }) {
    let busy = false;
    let refusing = false;
    async function send(id, text, fresh) {
      if (!text.trim()) { reply(id, { ok: false, error: "Nothing to send" }); return; }
      if (!fresh) {
        if (!refusing) {
          refusing = true;
          state({ failed: new Error(noClick), text });
        }
        reply(id, { ok: false, error: noClick });
        return;
      }
      if (busy) { reply(id, { ok: false, error: "Already sending" }); return; }
      await post(id, text);
    }
    async function post(id, text) {
      busy = true;
      refusing = false;
      state({ sending: true });
      try {
        await deliver(mark(title(), text));
        state({ sent: new Date() });
        reply(id, { ok: true });
      } catch (e) {
        const err = e && typeof e.message === "string" ? e : new Error(String(e));
        state({ failed: err, text, retry: () => (busy ? undefined : post(0, text)) });
        reply(id, { ok: false, error: err.message });
      } finally {
        busy = false;
      }
    }
    return { send };
  }

  // activation tells the hub when a page's send follows the user's own
  // click or key press in the page. A user gesture in a frame also activates
  // the frames above it (HTML, "activation notification"), so the hub reads
  // its own navigator.userActivation: the page cannot set that, and nothing
  // the page reports is trusted. take() answers true for one send per
  // activation: it ends the hub's activation (consume) and checks that this
  // worked, so a browser that cannot end it refuses every send.
  //
  // The hub's own clicks and key presses activate it too. The hub sees
  // those as events (the frame's gestures never reach the hub's document),
  // so while one is going on, and for a moment after it, take() answers
  // false, and the hub's activation is ended once the gesture is over. So an
  // activation that is left when a send arrives came from the frame.
  function activation({ win, active, consume, later, cancel }) {
    let held = false;
    let end = 0;
    let cap = 0;
    const release = () => {
      cancel(end);
      cancel(cap);
      end = 0;
      cap = 0;
      consume();
      held = false;
    };
    const start = () => {
      held = true;
      cancel(end);
      cancel(cap);
      end = 0;
      cap = later(release, 1500);
    };
    const stop = () => {
      if (!held) return;
      cancel(end);
      end = later(release, 60);
    };
    const down = ["pointerdown", "mousedown", "touchstart", "touchend", "keydown"];
    const up = ["pointerup", "mouseup", "pointercancel", "touchcancel", "keyup", "click"];
    for (const t of down) win.addEventListener(t, start, true);
    for (const t of up) win.addEventListener(t, stop, true);
    return {
      take() {
        if (held || !active()) return false;
        consume();
        return !active();
      },
      // settle ends an activation that is already there, such as the click
      // that opened the page, as the hub's own gesture.
      settle() {
        start();
        stop();
      },
      off() {
        for (const t of down) win.removeEventListener(t, start, true);
        for (const t of up) win.removeEventListener(t, stop, true);
        cancel(end);
        cancel(cap);
      },
    };
  }

  return { mark, split, sender, activation, noClick };
})();
