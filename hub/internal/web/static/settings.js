"use strict";
// HubSettings builds the Settings dialog: a tab each for the person's
// computers, notifications, Telegram, Claude accounts and account. ctx
// gives it the app's api and el helpers, the /api/me answer, the computers
// with their online state, the notification panel and a way to log out.
window.HubSettings = (ctx) => {
  const { api, el } = ctx;
  const dlg = document.getElementById("settings");
  const tabsBox = document.getElementById("settabs");
  const gear = document.getElementById("gearbtn");
  const gearDot = document.getElementById("geardot");
  const devBox = document.getElementById("devices");
  const back = document.getElementById("setback");
  const title = document.getElementById("settings-title");
  const phone = matchMedia("(max-width: 700px)");
  const tabs = [
    ["computers", "Computers", (me) => me.devices],
    ["notifications", "Notifications", (me) => me.notify],
    ["telegram", "Telegram", (me) => !!me.telegram_steps],
    ["claude", "Claude accounts", (me) => me.claude],
    ["account", "Account", () => true],
  ];
  let tab = "";
  let list = [];
  let loaded = false;
  let open = {};
  let editing = "";
  let confirming = "";
  let busy = false;
  let note = "";
  // inPane is true on a phone while a section shows instead of the list.
  let inPane = false;

  const shownTabs = () => tabs.filter(([, , on]) => on(ctx.me()));
  const review = () => list.filter((d) => d.addr_changed_at).length;

  function when(iso) {
    const v = Date.parse(iso || "");
    return v ? new Date(v).toLocaleString() : "";
  }

  function button(cls, text, fn) {
    const b = el("button", cls, text);
    b.type = "button";
    b.disabled = busy;
    b.onclick = fn;
    return b;
  }

  function drawTabs() {
    const hadFocus = tabsBox.contains(document.activeElement);
    tabsBox.replaceChildren();
    for (const [k, label] of shownTabs()) {
      const b = el("button", "", "");
      b.type = "button";
      b.id = "settab-" + k;
      b.setAttribute("role", "tab");
      b.setAttribute("aria-controls", "set-" + k);
      b.setAttribute("aria-selected", String(k === tab));
      b.tabIndex = k === tab ? 0 : -1;
      b.append(el("span", "", label));
      if (k === "computers" && review()) {
        const w = el("span", "warn");
        w.title = review() + " to review";
        w.setAttribute("aria-label", review() + " to review");
        b.append(w);
      }
      b.onclick = () => { inPane = true; show(k); };
      tabsBox.append(b);
      if (hadFocus && k === tab) b.focus();
      if (k === tab) requestAnimationFrame(() => b.scrollIntoView({ block: "nearest", inline: "nearest" }));
    }
    for (const [k] of tabs) {
      const pane = document.getElementById("set-" + k);
      pane.hidden = k !== tab;
      pane.setAttribute("aria-labelledby", "settab-" + k);
    }
  }

  function drawGear() {
    const n = review();
    gearDot.hidden = !n;
    gear.setAttribute("aria-label", n ? "Settings, " + n + " computer" + (n === 1 ? "" : "s") + " to review" : "Settings");
  }

  // Tabs move with the arrow keys, Home and End, as a tab list does.
  tabsBox.addEventListener("keydown", (e) => {
    const keys = shownTabs().map(([k]) => k);
    const i = keys.indexOf(tab);
    const next = { ArrowRight: i + 1, ArrowDown: i + 1, ArrowLeft: i - 1, ArrowUp: i - 1, Home: 0, End: keys.length - 1 }[e.key];
    if (next === undefined || !keys.length) return;
    e.preventDefault();
    inPane = false;
    show(keys[(next + keys.length) % keys.length]);
    document.getElementById("settab-" + tab).focus();
  });

  // mode switches a phone between the list of sections and one section.
  function mode() {
    const mob = phone.matches;
    dlg.classList.toggle("m-list", mob && !inPane);
    dlg.classList.toggle("m-pane", mob && inPane);
    back.hidden = !(mob && inPane);
    const t = tabs.find(([k]) => k === tab);
    title.textContent = mob && inPane && t ? t[1] : "Settings";
  }

  function show(k) {
    tab = k;
    drawTabs();
    mode();
    if (k === "computers") loadDevices();
    if (k === "notifications") ctx.notices.load();
    if (k === "telegram") drawTelegram();
    if (k === "claude") drawClaude();
    if (k === "account") drawAccount();
  }

  async function loadDevices() {
    if (!ctx.me().devices) return;
    try {
      list = await api("GET", "/api/devices");
      loaded = true;
      note = "";
    } catch (e) {
      note = "Could not load your computers: " + e.message;
    }
    drawGear();
    if (dlg.open) {
      drawTabs();
      drawDevices();
    }
  }

  // closeText says how a computer's latest connection to Hermes ended.
  function closeText(c) {
    const who = { device: "The computer", backend: "Hermes", hub: "The hub" }[c.by] || "Something";
    return "Disconnected " + when(c.at) + ". " + who + " closed the connection" +
      (c.reason ? ": " + c.reason : "") + " (code " + c.code + ").";
  }

  function drawDevices() {
    const was = devBox.contains(document.activeElement) ? document.activeElement.getAttribute("aria-controls") : null;
    devBox.replaceChildren();
    if (note) devBox.append(el("p", "err", note));
    if (!loaded) return;
    if (!list.length) {
      devBox.append(el("p", "none", "No computers are connected."));
      return;
    }
    const online = new Map(ctx.computers().map((c) => [c.id, c.online]));
    const many = new Set(list.map((d) => d.backend_id)).size > 1;
    const rows = el("div", "set-rows");
    for (const d of list) rows.append(deviceRow(d, d.kind === "sandbox" ? d.online : online.get(d.id), many));
    devBox.append(rows);
    if (was) rows.querySelector('[aria-controls="' + CSS.escape(was) + '"]')?.focus();
  }

  function deviceRow(d, on, many) {
    const row = el("div", "set-row");
    const head = el("button", "set-rowhead", "");
    head.type = "button";
    head.setAttribute("aria-expanded", String(!!open[d.id]));
    head.setAttribute("aria-controls", "dev-" + d.id);
    const st = el("span", "st " + (on === true ? "on" : on === false ? "off" : "unknown"));
    st.setAttribute("aria-hidden", "true");
    const top = el("span", "set-top");
    top.append(st, el("span", "nm", d.name));
    if (d.addr_changed_at) top.append(el("span", "pill", "New address"));
    const meta = el("span", "meta");
    if (on === true) meta.append(el("span", "", "Online"));
    else if (on === false) meta.append(el("span", "", "Offline"));
    if (d.kind === "sandbox") meta.append(el("span", "", "Sandbox"));
    else if (!d.paired) meta.append(el("span", "", "Waiting for the computer to finish connecting"));
    else if (!d.last_seen_at) meta.append(el("span", "", "Not used yet"));
    else meta.append(el("span", "", "Last used " + when(d.last_seen_at) + (d.last_addr ? " from " + d.last_addr : "")));
    if (many) meta.append(el("span", "", "Works with " + d.backend_name));
    head.append(top, meta);
    head.onclick = () => {
      open[d.id] = !open[d.id];
      if (!open[d.id] && editing === d.id) editing = "";
      drawDevices();
    };
    row.append(head);
    if (open[d.id]) row.append(deviceDetail(d));
    return row;
  }

  function deviceDetail(d) {
    const box = el("div", "set-detail");
    box.id = "dev-" + d.id;
    if (d.kind === "sandbox") {
      box.append(el("p", "hint", "Where Talaria runs commands for chats that are not on one of your computers. It is set up by the owner."));
      return box;
    }
    if (d.addr_changed_at) {
      const a = el("div", "set-alert");
      a.append(el("span", "", "Connected from a new address on " + when(d.addr_changed_at) +
        (d.prev_addr ? ". Before that it used " + d.prev_addr + "." : ".") + " If that was not you, remove this computer."));
      const acts = el("div", "acts");
      acts.append(button("", "That was me", () => act(d, "dismiss-address")),
        button("danger-text", "Remove", () => { confirming = d.id; drawDevices(); }));
      a.append(acts);
      box.append(a);
    }
    const dl = el("dl", "kv");
    const add = (k, v) => { if (v) dl.append(el("dt", "", k), el("dd", "", v)); };
    add("Computer name", d.hostname);
    add("System", [d.os, d.arch].filter(Boolean).join(" "));
    add("Paired with", d.client_version ? "talaria " + d.client_version : "");
    add("Connected", when(d.created_at));
    add("Last used", when(d.last_seen_at));
    add("Address", d.last_addr);
    add("Works with", d.backend_name);
    box.append(dl);
    if (d.last_close) box.append(el("p", "hint", closeText(d.last_close)));
    box.append(editing === d.id ? renameForm(d) : confirming === d.id ? removeConfirm(d) : rowActs(d));
    return box;
  }

  function rowActs(d) {
    const acts = el("div", "acts");
    acts.append(button("", "Rename", () => { editing = d.id; confirming = ""; drawDevices(); document.getElementById("rename-" + d.id)?.focus(); }),
      button("danger-text", "Remove computer", () => { confirming = d.id; editing = ""; drawDevices(); }));
    return acts;
  }

  function removeConfirm(d) {
    const acts = el("div", "acts");
    acts.append(el("span", "hint", "Remove " + d.name + "? Talaria can no longer use it until you connect it again."),
      button("danger", "Remove", () => act(d, "revoke")),
      button("", "Cancel", () => { confirming = ""; drawDevices(); }));
    return acts;
  }

  function renameForm(d) {
    const f = el("form", "acts");
    const input = el("input");
    input.id = "rename-" + d.id;
    input.value = d.name;
    input.maxLength = 64;
    input.required = true;
    input.setAttribute("aria-label", "New name for " + d.name);
    f.append(input, button("primary", "Save", null), button("", "Cancel", () => { editing = ""; drawDevices(); }));
    f.querySelector("button.primary").type = "submit";
    f.onsubmit = async (e) => {
      e.preventDefault();
      busy = true;
      try {
        await api("POST", "/api/devices/" + encodeURIComponent(d.id) + "/rename", { name: input.value });
        editing = "";
      } catch (err) {
        note = "Could not rename " + d.name + ": " + err.message;
      }
      busy = false;
      await loadDevices();
    };
    return f;
  }

  async function act(d, what) {
    busy = true;
    try {
      await api("POST", "/api/devices/" + encodeURIComponent(d.id) + "/" + what);
      if (what === "revoke") delete open[d.id];
    } catch (e) {
      note = "Could not change " + d.name + ": " + e.message;
    }
    busy = false;
    confirming = "";
    await loadDevices();
  }

  function pane(k, heading, hint) {
    const p = document.getElementById("set-" + k);
    p.replaceChildren(el("h2", "", heading));
    if (hint) p.append(el("p", "hint", hint));
    return p;
  }

  async function drawTelegram() {
    const me = ctx.me();
    const p = pane("telegram", "Telegram", "The hub's Telegram bot shows your chats, with the same assistants and permissions as this page.");
    const dl = el("dl", "kv");
    const linked = el("dd", "", me.notify ? "Checking" : "Send the bot a message to link your Telegram account.");
    dl.append(el("dt", "", "Account"), linked);
    const steps = { collapsed: "Collapsed: a quote that opens on a tap", expanded: "Expanded", off: "Off" }[me.telegram_steps] || me.telegram_steps;
    dl.append(el("dt", "", "Steps in replies"), el("dd", "", steps));
    p.append(dl, el("p", "hint", "The owner sets how replies show their steps (telegram.steps). Notifications through Telegram are in Notifications."));
    if (!me.notify) return;
    try {
      const s = await api("GET", "/api/notify");
      linked.textContent = s.telegram ? "Linked" : "Not linked. Send the bot a message, then open the link it sends you.";
    } catch (e) {
      linked.textContent = "Could not check: " + e.message;
    }
  }

  function drawClaude() {
    const p = pane("claude", "Claude accounts", ctx.me().usage
      ? "The Claude accounts your assistants use, their usage and their sign-ins are on the Usage page."
      : "Ask the owner about the Claude accounts your assistants use.");
    if (!ctx.me().usage) return;
    const a = el("a", "button primary", "Open Usage");
    a.href = "#/usage";
    a.onclick = () => close();
    p.append(a);
  }

  async function drawAccount() {
    const me = ctx.me();
    const p = pane("account", "Account", "");
    const dl = el("dl", "kv");
    const name = me.display && me.display !== me.user ? me.display + " (" + me.user + ")" : me.user || me.display || "";
    dl.append(el("dt", "", "Signed in as"), el("dd", "", name));
    const backend = el("dd", "", me.default_backend || "The first one you may use");
    dl.append(el("dt", "", "Default assistant"), backend);
    dl.append(el("dt", "", "Hub version"), el("dd", "", me.version || "Unknown"));
    p.append(dl);
    const out = el("div", "acts");
    const ask = () => {
      out.replaceChildren(el("span", "", "Log out of Talaria?"),
        button("", "Cancel", () => { out.replaceChildren(logoutButton()); out.firstChild.focus(); }),
        button("danger", "Log out", () => ctx.logout()));
      out.children[1].focus();
    };
    const logoutButton = () => button("", "Log out", ask);
    out.append(logoutButton());
    p.append(out);
    if (!me.default_backend) return;
    try {
      const b = (await api("GET", "/api/backends")).find((x) => x.id === me.default_backend);
      if (b && b.name) backend.textContent = b.name;
    } catch (_) { /* the id stays */ }
  }

  function trap(e) {
    if (e.key !== "Tab") return;
    const all = [...dlg.querySelectorAll("button, a[href], input, select, textarea, [tabindex]")]
      .filter((x) => !x.disabled && x.tabIndex >= 0 && x.getClientRects().length);
    if (!all.length) return;
    const first = all[0], last = all[all.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  }

  function openDialog(k) {
    const keys = shownTabs().map(([x]) => x);
    tab = keys.includes(k) ? k : keys.includes(tab) ? tab : keys[0];
    open = {};
    editing = confirming = "";
    if (review()) list.filter((d) => d.addr_changed_at).forEach((d) => { open[d.id] = true; });
    inPane = false;
    ctx.closeSide();
    dlg.showModal();
    show(tab);
    document.getElementById("settab-" + tab)?.focus();
  }

  function close() {
    if (!dlg.open) return;
    dlg.close();
    gear.focus();
  }

  function start() {
    gear.onclick = () => openDialog(review() ? "computers" : "");
    document.getElementById("setclose").onclick = close;
    back.onclick = () => {
      inPane = false;
      mode();
      document.getElementById("settab-" + tab)?.focus();
    };
    phone.addEventListener("change", mode);
    dlg.addEventListener("keydown", trap);
    dlg.addEventListener("click", (e) => { if (e.target === dlg) close(); });
    dlg.addEventListener("cancel", (e) => { e.preventDefault(); close(); });
    if (ctx.me().devices) {
      loadDevices();
      setInterval(() => { if (!document.hidden && !dlg.open) loadDevices(); }, 60000);
    }
    drawGear();
  }

  return { start, open: openDialog, close };
};
