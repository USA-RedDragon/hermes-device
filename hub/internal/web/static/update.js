"use strict";
// HubUpdate notices a new hub release and loads it without the user
// seeing it. The page checks the version in /api/me when its event stream
// reconnects (a new release restarts the hub), when the tab comes back, and
// every few minutes. It reloads only while the tab is hidden, or after a
// minute with no input, scrolling or pointer movement, nothing typed, no
// menu, dialog or popover open, no text selected and no upload running.
// Before the reload the service worker fetches the new files, so the
// reload needs no network, and the view is saved for HubRestore. A draft
// save finishes first.
// ctx: api, version() (the version the page loaded with), busy() (true
// with unsent text, attachments, an upload, or an open dialog, menu or
// popover), flush() (a promise that ends when the draft is saved),
// save() (the view state to keep).
window.HubUpdate = (ctx) => {
  const idleFor = 60000;
  let last = Date.now();
  let pending = false;
  let checking = false;
  let reloading = false;

  const touch = () => { last = Date.now(); };
  for (const t of ["keydown", "input", "pointermove", "pointerdown", "wheel", "touchstart", "scroll"]) {
    document.addEventListener(t, touch, { capture: true, passive: true });
  }

  const selecting = () => {
    const s = window.getSelection && window.getSelection();
    return !!s && !s.isCollapsed && String(s).trim() !== "";
  };

  function quiet() {
    if (document.hidden) return true;
    return Date.now() - last >= idleFor && !ctx.busy() && !selecting();
  }

  // warm asks the service worker to fetch the new release's files; it
  // gives up after a few seconds, and the reload then uses the network.
  function warm() {
    const sw = "serviceWorker" in navigator ? navigator.serviceWorker.controller : null;
    if (!sw) return Promise.resolve();
    return new Promise((done) => {
      const ch = new MessageChannel();
      const t = setTimeout(done, 5000);
      ch.port1.onmessage = () => { clearTimeout(t); done(); };
      sw.postMessage({ type: "prefetch" }, [ch.port2]);
    });
  }

  async function reload() {
    if (reloading || !quiet()) return;
    reloading = true;
    try { await ctx.flush(); } catch (_) { /* the draft save reported its own failure */ }
    await warm();
    if (!quiet()) { reloading = false; return; }
    try { sessionStorage.setItem("hub.view", JSON.stringify({ at: Date.now(), ...ctx.save() })); } catch (_) { /* reload without it */ }
    location.reload();
  }

  async function check() {
    if (checking || reloading) return;
    if (pending) { reload(); return; }
    const had = ctx.version();
    if (!had) return;
    checking = true;
    try {
      const me = await ctx.api("GET", "/api/me");
      if (me && me.version && me.version !== had) pending = true;
    } catch (_) { /* try again later */ }
    checking = false;
    if (pending) reload();
  }

  document.addEventListener("visibilitychange", () => {
    if (document.hidden && pending) reload();
    else if (!document.hidden) check();
  });
  setInterval(() => { if (pending) reload(); }, 5000);
  setInterval(() => { if (!document.hidden) check(); }, 5 * 60000);
  return { check, pending: () => pending };
};

// HubRestore puts back the view HubUpdate saved before a reload. A page
// that restores hides the chat list and the conversation until they are
// back in place, or for at most a moment.
window.HubRestore = (() => {
  let saved = null;
  try {
    saved = JSON.parse(sessionStorage.getItem("hub.view") || "null");
    sessionStorage.removeItem("hub.view");
  } catch (_) { saved = null; }
  if (saved && (Date.now() - saved.at > 60000 || saved.hash !== location.hash)) saved = null;
  const root = document.documentElement;
  if (saved) {
    root.classList.add("restoring");
    setTimeout(() => root.classList.remove("restoring"), 2500);
  }
  return {
    state: () => saved,
    done: () => { saved = null; root.classList.remove("restoring"); },
  };
})();
