"use strict";
// HubPageChat runs the hub page in page chat mode (/pagechat): the chat
// that the Talaria Firefox extension frames in its toolbar popup and its
// sidebar. The chat itself is the hub's own (app.js, the composer, message
// rendering); this adds the small header with the page chat list, the
// empty state, the scope and selection chips, the meter row, and the
// MessagePort to the extension page, which knows the window's tabs.
// It returns null outside page chat mode.
// ctx: api, el, toast(text), banner(text, bad), view() (the open chat or
// null), sessions() (the last chat list), open(b, s), fresh() (a new page
// chat), hash(b, s) (a chat's hub route), ago(iso), submit().
window.HubPageChat = (ctx) => {
  if (!document.documentElement.classList.contains("pagechat")) return null;
  const { el } = ctx;
  const $ = (id) => document.getElementById(id);
  const I = window.HubIcons;
  const store = {
    get(k) { try { return JSON.parse(localStorage.getItem("talaria.pagechat." + k)); } catch (_) { return null; } },
    set(k, v) { try { localStorage.setItem("talaria.pagechat." + k, JSON.stringify(v)); } catch (_) { /* no storage */ } },
  };

  let port = null;
  let mode = "";
  let deviceID = "";
  let scope = store.get("scope") === "tab" ? "tab" : "window";
  let tabs = { count: 0, selection: false, text: "" };
  let listOpen = false;
  let seq = 0;
  const waiting = new Map();

  // The extension page sends a MessagePort after each load of this frame.
  addEventListener("message", (e) => {
    const d = e.data;
    if (port || window.parent === window || e.source !== window.parent || !/^moz-extension:\/\//.test(e.origin) ||
      !d || d.type !== "talaria-page:init" || !e.ports || !e.ports[0]) return;
    port = e.ports[0];
    mode = d.mode === "popup" ? "popup" : "sidebar";
    deviceID = typeof d.device_id === "string" ? d.device_id : "";
    port.onmessage = (m) => onPort(m.data);
    document.documentElement.classList.add("pc-" + mode);
    head();
    chips();
    port.postMessage({ type: "watch" });
  });

  function onPort(d) {
    if (!d || typeof d !== "object") return;
    if (d.type === "context" && waiting.has(d.id)) {
      waiting.get(d.id)(d);
      waiting.delete(d.id);
    } else if (d.type === "tabs") {
      tabs = { count: +d.count || 0, selection: !!d.selection, text: typeof d.selection_text === "string" ? d.selection_text : "" };
      chips();
      empty();
    } else if (d.type === "hover") {
      const b = document.querySelector('#phead [data-key="' + d.name + '"]');
      if (b) b.classList.toggle("hover", !!d.on);
    }
  }

  // context asks the extension for the tabs a message covers and lets the
  // chat read them. Without the extension (the view opened in a tab) a
  // message goes without them.
  function context(session) {
    if (!port) return Promise.resolve(null);
    const id = ++seq;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => { waiting.delete(id); reject(new Error("the extension did not answer")); }, 10000);
      waiting.set(id, (d) => {
        clearTimeout(timer);
        if (d.error) reject(new Error(d.error));
        else resolve({ scope, tabs: d.tabs || [], selection: d.selection || null });
      });
      port.postMessage({ type: "context", id, scope, session });
    });
  }

  function openURL(path) {
    if (port) port.postMessage({ type: "open", url: path });
    else window.open(path, "_blank", "noopener");
  }

  // The header: the mark, the chat's title (it opens the page chat list),
  // new page chat, move between the popup and the sidebar, open in the
  // hub, and in the sidebar close.
  const bar = el("header", "phead");
  bar.id = "phead";
  const ttl = el("button", "pttl");
  ttl.type = "button";
  ttl.title = "Page chats";
  ttl.setAttribute("aria-haspopup", "menu");
  ttl.setAttribute("aria-expanded", "false");
  const ttlText = el("span");
  ttl.append(ttlText, I.icon("chev"));
  ttl.onclick = () => showList(!listOpen);
  const list = el("div", "plist");
  list.id = "plist";
  list.setAttribute("role", "menu");
  list.hidden = true;

  function key(name, icon, label, onclick) {
    const b = I.button(icon, label);
    b.dataset.key = name;
    if (onclick) b.onclick = onclick;
    return b;
  }

  function head() {
    const keys = [];
    if (mode === "popup") keys.push(key("sidebar", "side", "Move to the sidebar"));
    if (mode === "sidebar") keys.push(key("popup", "popup", "Move back to the toolbar popup"));
    const hubBtn = key("hub", "open", "Open in the hub", () => {
      const v = ctx.view();
      openURL("/" + (v ? ctx.hash(v.backend, v.session) : ""));
    });
    const close = mode === "sidebar" ? [key("close", "close", "Close the sidebar")] : [];
    bar.replaceChildren(I.logo(), ttl, key("new", "plus", "New page chat", () => { showList(false); ctx.fresh(); }),
      ...keys, hubBtn, ...close);
    title();
    placeKeys();
  }

  // Firefox opens the sidebar and the popup only on a click in the
  // extension's own page, so the extension page puts its buttons over
  // these (talaria docs/browser.md section 6.2).
  function placeKeys() {
    if (!port) return;
    const keys = [];
    for (const b of bar.querySelectorAll("[data-key=sidebar], [data-key=popup], [data-key=close]")) {
      const r = b.getBoundingClientRect();
      keys.push({ name: b.dataset.key, label: b.getAttribute("aria-label"), x: r.left, y: r.top, w: r.width, h: r.height });
      b.tabIndex = -1;
    }
    port.postMessage({ type: "keys", keys });
  }
  new ResizeObserver(placeKeys).observe(bar);

  function title() {
    const t = $("title").textContent;
    ttlText.textContent = ctx.view() && t && t !== "New chat" ? t : "New page chat";
  }
  new MutationObserver(title).observe($("title"), { childList: true, characterData: true, subtree: true });

  function pageChats() {
    return (ctx.sessions() || []).filter((s) => s.page && !s.archived)
      .sort((a, b) => Date.parse(b.updated_at || 0) - Date.parse(a.updated_at || 0)).slice(0, 12);
  }

  function showList(open) {
    listOpen = open;
    ttl.setAttribute("aria-expanded", String(open));
    list.hidden = !open;
    if (!open) return;
    const v = ctx.view();
    const rows = pageChats().map((s) => {
      const b = el("button", v && v.backend === s.backend_id && v.session === s.id ? "on" : "");
      b.type = "button";
      b.setAttribute("role", "menuitem");
      b.append(el("span", "", s.title || "New page chat"), el("small", "", ctx.ago(s.updated_at)));
      b.onclick = () => { showList(false); ctx.open(s.backend_id, s.id); };
      return b;
    });
    list.replaceChildren(...(rows.length ? rows : [el("p", "none", "No page chats yet.")]));
  }
  document.addEventListener("click", (e) => { if (listOpen && !bar.contains(e.target) && !list.contains(e.target)) showList(false); });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && listOpen) { e.stopPropagation(); showList(false); ttl.focus(); }
  }, true);

  // The empty state: what to ask, until the chat has a message.
  const blank = el("div", "pempty");
  blank.id = "pempty";
  function empty() {
    const v = ctx.view();
    const log = $("log");
    const none = !v || !Array.from(log.children).some((n) => !n.classList.contains("status"));
    blank.hidden = !none;
    if (!none) return;
    const asks = ["Summarize this page", "Compare the open tabs"];
    if (tabs.selection) asks.push("Explain the selected text");
    blank.replaceChildren(el("p", "", "Ask about the tabs in this window."), ...asks.map((q) => {
      const b = el("button", "", q);
      b.type = "button";
      b.onclick = () => { $("input").value = q; ctx.submit(); };
      return b;
    }));
  }
  new MutationObserver(empty).observe($("log"), { childList: true });

  // The chips under the message box: what the agent can see (click to
  // switch), the selection, and the model; the send button follows.
  const row = el("div", "prow");
  const scopeBtn = el("button", "pchip");
  scopeBtn.type = "button";
  scopeBtn.title = "What Talaria can see. Click to switch.";
  const scopeText = el("span");
  scopeBtn.append(I.icon("tabs"), scopeText);
  scopeBtn.onclick = () => {
    scope = scope === "window" ? "tab" : "window";
    store.set("scope", scope);
    chips();
  };
  const selChip = el("span", "pchip", "+ selection");
  const modelChip = el("span", "pchip");

  function chips() {
    scopeText.textContent = scope === "tab" ? "This tab only" : "This window · " + tabs.count + (tabs.count === 1 ? " tab" : " tabs");
    scopeBtn.hidden = !port;
    selChip.hidden = !port || !tabs.selection;
    selChip.title = tabs.text || "The selected text goes with your message";
    const v = ctx.view();
    const model = v && v.info ? v.info.model || (v.info.usage && v.info.usage.model) : "";
    modelChip.textContent = model || "";
    modelChip.title = model ? "Model: " + model : "";
    modelChip.hidden = !model;
  }

  const meterRow = el("div", "pmeters");
  meterRow.id = "pmeters";

  function meters(parts) {
    meterRow.replaceChildren(...parts);
    chips();
  }

  function running(on) {
    $("input").placeholder = on ? "Steer the reply" : "Ask about this window";
  }

  // start lays the page chat parts into the hub page.
  function start() {
    const main = document.querySelector(".main");
    main.prepend(bar, list);
    $("log").before(blank);
    const form = $("composer");
    row.append(scopeBtn, selChip, modelChip, el("span", "grow"), $("send"));
    form.querySelector(".composer-row").after(row);
    form.append(meterRow);
    $("input").addEventListener("focus", () => { if (port) port.postMessage({ type: "watch" }); });
    running(false);
    head();
    chips();
    empty();
  }

  // signIn replaces the page when the hub session is gone: the hub's
  // sign-in does not run in a frame, so it opens in a tab.
  function signIn() {
    const box = el("div", "pempty");
    const b = el("button", "primary", "Sign in to the hub");
    b.type = "button";
    b.onclick = () => openURL("/auth/login?next=" + encodeURIComponent("/"));
    box.append(el("p", "", "Sign in to the hub to use the page chat. Then open it again."), b);
    document.body.replaceChildren(box);
  }

  return {
    start, context, meters, running, signIn, empty,
    shown() { running(false); title(); chips(); empty(); showList(false); },
    device: () => deviceID,
    remember(b, s) { store.set("last", { b, s }); },
    last: () => store.get("last"),
  };
};
