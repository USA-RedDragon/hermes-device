"use strict";
// HubViewReport tells the hub when the page shows a chat away from its
// newest message and the reader did nothing to put it there, at any time
// and not only after an open: no wheel, touch, key or press in the last
// 1.5 s, and none before the report goes out. It is for finding what
// moves the view in a browser the tests do not catch. At most one report
// every 30 s. A report carries a hash of the chat id (never its text),
// the log's last moves, the stack that noticed the move, how long the page
// and the chat have been open, whether the page loaded or its event
// stream came back in the last 60 s, and the page's visibility.
//
// loaded() tells the hub once that the page loaded and with which release,
// so a reload after a restart shows in the hub's log.
//
// o.view() is the open chat ({backend, session, loaded, openedAt, kinds})
// or null; o.stick has pinned(), hearing() (true while a scroll counts as
// the reader's) and trail(); o.log is the log element; o.csrf() is the
// token. o.fetch, o.beacon, o.now, o.every and o.settle are for tests.
window.HubViewReport = (o) => {
  const now = o.now || Date.now;
  const every = o.every || 30000;
  const settle = o.settle || 300;
  const send = o.fetch || ((url, init) => fetch(url, init));
  const beacon = o.beacon || ((url, blob) => (navigator.sendBeacon ? navigator.sendBeacon(url, blob) : false));
  const url = "/api/view-report";
  const born = now();
  let lastInput = -Infinity;
  let sentAt = -Infinity;
  let wasPinned = true;
  let excused = 0;
  let reconnectAt = -Infinity;
  let reconnects = 0;
  let pending = 0;

  const input = () => { lastInput = now(); };
  for (const type of ["wheel", "touchstart", "touchmove", "touchend", "keydown", "pointerdown", "mousedown", "click"]) {
    window.addEventListener(type, input, { capture: true, passive: true });
  }

  // hash is FNV-1a over the chat's backend and id: enough to tell chats
  // apart in the hub's log without naming one.
  function hash(text) {
    let h = 0x811c9dc5;
    for (let i = 0; i < text.length; i++) {
      h ^= text.charCodeAt(i);
      h = Math.imul(h, 0x01000193) >>> 0;
    }
    return h.toString(16).padStart(8, "0");
  }

  // post sends body with fetch and keepalive, and with a beacon when the
  // fetch fails or is not there. A beacon cannot set the CSRF header, so
  // the token goes in the body, and the hub takes it from there.
  function post(body) {
    const text = JSON.stringify(body);
    const fallback = () => {
      try { beacon(url, new Blob([JSON.stringify({ ...body, csrf: o.csrf() })], { type: "application/json" })); } catch (_) { /* only a report */ }
    };
    try {
      const r = send(url, { method: "POST", keepalive: true, credentials: "same-origin", headers: { "Content-Type": "application/json", "X-CSRF-Token": o.csrf() }, body: text });
      if (r && r.then) r.then((resp) => { if (resp && resp.status >= 500) fallback(); }, fallback);
    } catch (_) {
      fallback();
    }
  }

  function nav() {
    try {
      const e = performance.getEntriesByType("navigation")[0];
      return e ? e.type : "";
    } catch (_) {
      return "";
    }
  }

  // excuse says the page moved the view for the reader (a search result, a
  // place kept over a reload), for ms.
  function excuse(ms) { excused = now() + ms; }

  // reconnected says the event stream came back after a failure.
  function reconnected() { reconnectAt = now(); reconnects++; }

  // changed is for each time the log's state may have changed.
  function changed() {
    const pinned = o.stick.pinned();
    const was = wasPinned;
    wasPinned = pinned;
    if (pinned || !was || pending) return;
    const v = o.view();
    if (!v || !v.loaded) return;
    const t = now();
    if (t < excused || o.stick.hearing() || t - lastInput < 1500) return;
    const stack = String(new Error().stack || "").split("\n").slice(1, 9).join(" | ").slice(0, 800);
    pending = setTimeout(() => {
      pending = 0;
      const t2 = now();
      // Gone, back at the end, or the reader did something since.
      if (o.view() !== v || o.stick.pinned() || lastInput >= t) return;
      if (t2 - sentAt < every) return;
      sentAt = t2;
      const log = o.log;
      post({
        kind: "left", chat: hash(v.backend + "\n" + v.session), ms: t2 - v.openedAt,
        up: Math.round(t2 - born), nav: nav(), vis: document.visibilityState,
        loaded60: t2 - born < 60000, reconnect60: t2 - reconnectAt < 60000, reconnects,
        gap: Math.round(log.scrollHeight - log.scrollTop - log.clientHeight), top: Math.round(log.scrollTop),
        h: log.scrollHeight, c: log.clientHeight,
        trail: o.stick.trail(), events: (v.kinds || []).slice(-20), stack,
      });
    }, settle);
  }

  // loaded tells the hub this page loaded, with the release it runs.
  function loaded(version) {
    post({ kind: "loaded", version: String(version || ""), nav: nav(), vis: document.visibilityState });
  }

  return { changed, excuse, reconnected, loaded };
};
