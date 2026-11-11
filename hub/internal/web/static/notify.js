"use strict";
// HubNotify installs the service worker, tells the hub which chat the page
// shows while it is visible (so the hub does not notify about it), and
// builds the Notifications part of Settings: which events notify, quiet
// hours, Web Push on this device and a test. ctx gives it the app's api
// and el helpers, the open chat and a way to open a hash.
window.HubNotify = (ctx) => {
  const { api, el } = ctx;
  const root = document.getElementById("notify-prefs");
  const kinds = [
    ["approvals", "Approvals", "Hermes asks before it runs something"],
    ["questions", "Questions", "Hermes asks you to choose or answer"],
    ["failures", "Failures", "A chat or scheduled job failed or needs you to sign in"],
    ["finished", "Long tasks done", "A reply that took more than 2 minutes is ready"],
    ["jobs", "Scheduled jobs", "A scheduled job finished"],
  ];
  let state = null;
  let note = "";
  let bad = false;
  let busy = false;
  let reg = null;
  let sub = null;
  let mine = false;

  const pushable = () => "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  const ios = () => /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  const standalone = () => matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;

  function b64(s) {
    const pad = "=".repeat((4 - (s.length % 4)) % 4);
    const raw = atob((s + pad).replace(/-/g, "+").replace(/_/g, "/"));
    return Uint8Array.from(raw, (c) => c.charCodeAt(0));
  }

  function label() {
    const ua = navigator.userAgent;
    const os = /Android/.test(ua) ? "Android" : ios() ? "iPhone or iPad" : /Mac/.test(ua) ? "Mac" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
    const br = /Firefox\//.test(ua) ? "Firefox" : /Edg\//.test(ua) ? "Edge" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser";
    return os ? br + " on " + os : br;
  }

  async function register() {
    if (!("serviceWorker" in navigator)) return;
    try {
      reg = await navigator.serviceWorker.register("/sw.js", { scope: "/" });
      sub = reg.pushManager ? await reg.pushManager.getSubscription() : null;
    } catch (_) { reg = null; }
    navigator.serviceWorker.addEventListener("message", (e) => {
      const d = e.data || {};
      if (d.type === "hub.open" && typeof d.url === "string" && d.url.startsWith("/")) {
        const i = d.url.indexOf("#");
        if (i >= 0) location.hash = d.url.slice(i); else location.hash = "";
      }
    });
  }

  // Presence: while the page is visible and shows a chat, say so every
  // 45 seconds; say when it stops.
  let shown = null;
  async function presence() {
    const cur = document.hidden ? null : ctx.current();
    const key = cur ? cur.backend + "/" + cur.session : "";
    const was = shown ? shown.backend + "/" + shown.session : "";
    if (was && was !== key) {
      const last = shown;
      shown = null;
      try { await api("POST", "/api/presence", { backend: last.backend, session: last.session, visible: false }); } catch (_) { /* best effort */ }
    }
    if (!cur) return;
    shown = cur;
    try { await api("POST", "/api/presence", { backend: cur.backend, session: cur.session, visible: true }); } catch (_) { /* best effort */ }
  }

  function start(enabled) {
    register().then(() => (enabled ? own() : null)).catch(() => {});
    if (!enabled) return;
    presence();
    setInterval(presence, 45000);
    document.addEventListener("visibilitychange", presence);
    window.addEventListener("hashchange", () => setTimeout(presence, 0));
    window.addEventListener("pagehide", () => {
      if (!shown) return;
      const body = JSON.stringify({ backend: shown.backend, session: shown.session, visible: false });
      fetch("/api/presence", { method: "POST", keepalive: true, credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-CSRF-Token": ctx.csrf() }, body }).catch(() => {});
    });
  }

  // own checks that this browser's subscription is the signed-in
  // person's, and drops one that another person's ended login left.
  async function own() {
    mine = false;
    if (!sub) return;
    mine = (await api("POST", "/api/notify/push/check", { endpoint: sub.endpoint })).mine;
    if (!mine) {
      await sub.unsubscribe();
      sub = null;
    }
  }

  async function load() {
    if (!root) return;
    try {
      state = await api("GET", "/api/notify");
      if (reg && reg.pushManager) sub = await reg.pushManager.getSubscription();
      await own();
    } catch (e) {
      note = "Could not load notification settings: " + e.message;
      bad = true;
    }
    draw();
  }

  async function save(patch) {
    const next = Object.assign({}, state.settings, patch);
    if (!next.time_zone) next.time_zone = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
    await act(async () => { state = await api("POST", "/api/notify/settings", next); }, "Saved.");
  }

  async function act(fn, ok) {
    busy = true;
    draw();
    try {
      await fn();
      if (ok !== null) note = ok || "";
      bad = false;
    } catch (e) {
      note = e.message;
      bad = true;
    }
    busy = false;
    draw();
  }

  async function enablePush() {
    await act(async () => {
      if (!reg) throw new Error("This browser cannot show notifications from the hub.");
      const perm = await Notification.requestPermission();
      if (perm !== "granted") throw new Error("Notifications are blocked for this site in the browser's settings.");
      sub = await reg.pushManager.getSubscription();
      if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64(state.push_key) });
      const j = sub.toJSON();
      state = await api("POST", "/api/notify/push", { endpoint: j.endpoint, keys: j.keys, label: label() });
      mine = true;
    }, "Notifications are on for this device.");
  }

  async function disablePush() {
    await act(async () => {
      const s = sub || (reg && await reg.pushManager.getSubscription());
      if (s) {
        state = await api("POST", "/api/notify/push/remove", { endpoint: s.endpoint });
        await s.unsubscribe();
      }
      sub = null;
      mine = false;
    }, "Notifications are off for this device.");
  }

  // logout ends this device's push subscription, so the next person who
  // signs in here does not get the notices of the last one.
  async function logout() {
    try {
      const s = sub || (reg && reg.pushManager && await reg.pushManager.getSubscription());
      if (s) {
        await api("POST", "/api/notify/push/remove", { endpoint: s.endpoint });
        await s.unsubscribe();
      }
    } catch (_) { /* best effort */ }
  }

  function check(name, text, hint, on, fn) {
    const row = el("label", "notify-row");
    const box = el("input");
    box.type = "checkbox";
    box.checked = !!on;
    box.disabled = busy;
    box.onchange = () => fn(box.checked);
    const words = el("span", "");
    words.append(el("span", "", text));
    if (hint) words.append(el("small", "hint", hint));
    row.append(box, words);
    row.dataset.name = name;
    return row;
  }

  function draw() {
    if (!root) return;
    root.replaceChildren();
    root.append(el("h2", "", "Notifications"));
    if (!state) {
      if (note) root.append(el("p", bad ? "hint err" : "hint", note));
      return;
    }
    const s = state.settings;
    root.append(el("p", "hint", "Hermes tells you on your phone when it needs you or is done, while the chat is not open on a screen."));
    const shown = state.claude_admin ? [...kinds, ["claude_signin", "Claude sign-ins", "A Claude account that Hermes uses lost its login"]] : kinds;
    for (const [k, text, hint] of shown) root.append(check(k, text, hint, s[k], (v) => save({ [k]: v })));

    root.append(el("h3", "", "Where"));
    const tg = check("telegram", "Telegram", state.telegram ? "Your linked Telegram chat, with buttons to answer" :
      "Link Telegram first: send the hub's bot a message", s.telegram, (v) => save({ telegram: v }));
    root.append(tg);
    root.append(check("push", "Browser and app notifications", "On the devices below", s.push, (v) => save({ push: v })));

    const dev = el("div", "notify-push");
    if (!state.push_key) {
      dev.append(el("p", "hint", "Browser notifications are not set up on this hub."));
    } else if (!pushable()) {
      dev.append(el("p", "hint", ios() && !standalone()
        ? "On iPhone and iPad, notifications work only from the Home Screen app: tap Share, then Add to Home Screen, open the hub from there and come back here."
        : "This browser cannot show notifications from the hub."));
    } else {
      const here = !!sub && mine;
      const btn = el("button", here ? "" : "primary", here ? "Turn off on this device" : "Turn on for this device");
      btn.type = "button";
      btn.disabled = busy;
      btn.onclick = () => (here ? disablePush() : enablePush());
      dev.append(btn);
      if (ios()) dev.append(el("p", "hint", "On iPhone and iPad this works only in the Home Screen app."));
    }
    if (state.subscriptions.length) {
      const list = el("ul", "notify-devices");
      for (const d of state.subscriptions) {
        const li = el("li", "");
        li.append(el("span", "", d.label + " (since " + new Date(d.created).toLocaleDateString() + ")"));
        const rm = el("button", "ghost", "Remove");
        rm.type = "button";
        rm.disabled = busy;
        rm.onclick = () => act(async () => { state = await api("POST", "/api/notify/push/remove", { id: d.id }); }, "Removed.");
        li.append(rm);
        list.append(li);
      }
      dev.append(list);
    }
    root.append(dev);

    root.append(el("h3", "", "Quiet hours"));
    const q = el("div", "notify-quiet");
    const from = el("input");
    const to = el("input");
    from.type = to.type = "time";
    from.value = s.quiet_start || "";
    to.value = s.quiet_end || "";
    from.disabled = to.disabled = busy;
    from.setAttribute("aria-label", "Quiet from");
    to.setAttribute("aria-label", "Quiet until");
    const set = el("button", "", "Save");
    set.type = "button";
    set.disabled = busy;
    set.onclick = () => save({ quiet_start: from.value, quiet_end: to.value,
      time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone || "" });
    const clear = el("button", "ghost", "None");
    clear.type = "button";
    clear.disabled = busy;
    clear.onclick = () => save({ quiet_start: "", quiet_end: "" });
    q.append(el("span", "", "From"), from, el("span", "", "until"), to, set, clear);
    root.append(q);
    root.append(el("p", "hint", s.quiet_start
      ? "Nothing is sent from " + s.quiet_start + " until " + s.quiet_end + " (" + (s.time_zone || "UTC") + "). A question still waiting after that gets its reminder."
      : "No quiet hours."));

    const test = el("button", "", "Send a test");
    test.type = "button";
    test.disabled = busy;
    test.onclick = () => act(async () => {
      const r = await api("POST", "/api/notify/test");
      note = "Sent to " + r.telegram + " Telegram chat(s) and " + r.push + " device(s).";
    }, null);
    root.append(test);
    if (note) root.append(el("p", bad ? "hint err" : "hint", note));
  }

  return { start, load, logout };
};
