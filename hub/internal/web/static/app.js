"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  };
  // pageMode is the page chat of the Firefox extension (/pagechat, pagechat.js).
  const pageMode = document.documentElement.classList.contains("pagechat");
  let csrf = "";
  let stream = null;
  let view = null;
  let me = {};
  // connectValue is the model menu line that opens Connect a model.
  const connectValue = "\u0000connect";
  let current = null;
  const words = { approval: "Talaria asks to run this", allow: "Allow", deny: "Deny",
    args: "Input", result: "Output", running: "Running", done: "Done", failed: "Failed",
    working: "Working", queued: "Queued: Talaria reads this when the current turn ends", waiting: "Waiting to run", newchat: "New chat", loading: "Loading\u2026",
    lost: "The agent restarted before it finished replying to this message. Messages sent after it wait until you dismiss it.",
    held: "The agent is restarting. Your message will send when it is back.", notsent: "Not sent.", again: "Send again",
    steered: "Sent to the running turn", now: "Stop and send now",
    unread: "The turn stopped before Talaria read this message.",
    tobottom: "Jump to latest", newone: "1 new message", newmany: "%d new messages" };
  const t = (k) => words[k];

  async function api(method, path, body, meta) {
    const opts = { method, headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" };
    if (body instanceof FormData) {
      opts.body = body;
    } else if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const r = await fetch(path, opts);
    if (meta) meta.headers = r.headers;
    if (r.status === 401) {
      if (pageMode) page.signIn();
      else location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.hash);
      throw new Error("login required");
    }
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch (_) { /* not JSON */ }
      const err = new Error(msg);
      err.status = r.status;
      throw err;
    }
    const json = (r.headers.get("Content-Type") || "").startsWith("application/json");
    return r.status === 200 || r.status === 201 || (r.status === 202 && json) ? r.json() : null;
  }

  // banner shows one line above the chat. A banner has an owner (key) that
  // alone may clear it again by itself with clearBanner, so a connection
  // that came back does not wipe a message about something else.
  let bannerKey = "";
  function banner(text, bad, key, quiet) {
    const b = $("banner");
    bannerKey = text ? key || "" : "";
    b.hidden = !text;
    b.replaceChildren();
    b.classList.toggle("bad", !!bad);
    b.classList.toggle("quiet", !!quiet);
    if (!text) return;
    const close = el("button", "close", "\u00d7");
    close.type = "button";
    close.title = "Dismiss";
    close.setAttribute("aria-label", "Dismiss");
    close.onclick = () => banner("");
    b.append(el("span", "", text), close);
  }

  function clearBanner(key) {
    if (bannerKey === key) banner("");
  }

  function toast(text) {
    const x = $("toast");
    x.textContent = text;
    x.hidden = false;
    clearTimeout(toast.h);
    toast.h = setTimeout(() => { x.hidden = true; }, 2200);
  }

  function ago(iso) {
    const v = Date.parse(iso);
    // A time before 2001 is a missing one (zero or the epoch), never an age.
    if (!v || v < 978307200000) return "";
    const s = Math.max(0, (Date.now() - v) / 1000);
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }

  const chatHash = (b, s) => "#/s/" + encodeURIComponent(b) + "/" + encodeURIComponent(s);

  // focusInput puts the caret in the message box. On a touch screen it
  // does nothing: focus raises the on-screen keyboard, and opening or
  // switching a chat must not do that. The user taps the box when they
  // want to type.
  function focusInput() {
    if (matchMedia("(pointer: coarse)").matches) return;
    $("input").focus({ preventScroll: true });
  }

  // With the on-screen keyboard open, a browser that does not honor
  // interactive-widget=resizes-content (iOS) pans the page and hides the
  // header. Bind the app to the visual viewport there and undo the pan.
  if (window.visualViewport) {
    const vv = window.visualViewport;
    const fit = () => {
      const shrunk = window.innerHeight - vv.height > 1;
      document.documentElement.style.setProperty("--vvh", shrunk ? vv.height + "px" : "");
      if (shrunk && (window.scrollY || vv.offsetTop)) window.scrollTo(0, 0);
    };
    vv.addEventListener("resize", fit);
    vv.addEventListener("scroll", fit);
  }

  function openSide(open) {
    $("side").classList.toggle("open", open);
    $("scrim").hidden = !open;
  }

  // Sidebar: every chat on every backend the user may use. Empty chats are
  // hidden unless open, archived chats unless asked for. Backend headings
  // show only when there is more than one. While a chat's menu is open the
  // list is not redrawn, so a refresh cannot throw away a half-typed name.
  let listing = false;
  const AWAY_NOTE = "Reconnecting to the agent. You can keep reading and typing.";
  let notice = "";
  // unhid is a chat just unarchived. Hermes lists archived and hidden
  // chats together, so one that stays in that list was hidden, not archived.
  let unhid = "";
  let lastList = null;
  let menuOpen = false;
  let worksLoaded = false;
  let showArchived = false;
  const listBackoff = window.HubBackoff();
  let listRetry = 0;
  // unreachableIDs reads the backends a list answer could not reach.
  const unreachableIDs = (h) => new Set((h || "").split(",").filter(Boolean).map(decodeURIComponent));
  // keepUnreachable is the new list, where a backend the hub could not
  // reach and sent no rows for keeps the rows the page had: a failed list
  // is never an empty one.
  function keepUnreachable(prev, got, down) {
    if (!down.size || !prev) return got;
    const sent = new Set(got.map((s) => s.backend_id));
    return got.concat(prev.sessions.filter((s) => down.has(s.backend_id) && !sent.has(s.backend_id)));
  }
  async function refreshList() {
    if (listing) return;
    listing = true;
    const asked = Date.now();
    clearTimeout(listRetry);
    listRetry = 0;
    try {
      const meta = {};
      const [backends, got] = await Promise.all([api("GET", "/api/backends"), api("GET", "/api/sessions", undefined, meta)]);
      const down = unreachableIDs(meta.headers && meta.headers.get("X-Talaria-Unreachable"));
      // Refreshing backends sent the hub's copy of their chats while the
      // fresh list is on its way, as during an agent restart: shown as
      // is, asked for again soon, and no banner unless that lasts.
      const refreshing = unreachableIDs(meta.headers && meta.headers.get("X-Talaria-Refreshing"));
      const sessions = keepUnreachable(lastList, got, down);
      const alarm = unreachableIDs(meta.headers && meta.headers.get("X-Talaria-Down"));
      lastList = { backends, sessions };
      if (down.size) {
        // One status, in the top bar. While the agent may be restarting it
        // is a quiet note; an error only once it is down.
        if (alarm.size) banner(sessions.length ? "Cannot reach the agent. Showing the chats from before; retrying." : "Cannot reach the agent. Retrying.", true, "away");
        else if (bannerKey !== "away" || !$("banner").classList.contains("bad")) banner(AWAY_NOTE, false, "away", true);
        listRetry = setTimeout(refreshList, listBackoff.next());
      } else {
        listBackoff.reset();
        clearBanner("away");
        clearBanner("list");
        if (refreshing.size) listRetry = setTimeout(refreshList, 2000);
      }
      // A chat the hub's fresh list does not have, opened before that list
      // was asked for, is gone from Hermes: say so, with a way out.
      if (!page && view && !down.size && !refreshing.size && view.openedAt < asked && !sessions.some(isCurrent) &&
        sessions.some((s) => s.backend_id === view.backend)) showGone();
      // A chat opened before the list came shows its title now.
      const open = view && !view.loaded && sessions.find(isCurrent);
      if (open && open.title) setTitle(open.title);
      if (unhid) {
        if (sessions.some((s) => s.id === unhid && s.archived)) banner("Talaria keeps this chat hidden, so it stays with the archived chats.");
        unhid = "";
      }
      if (!menuOpen || !document.querySelector("#sessions .menu")) drawList();
      if (me.computers) loadPlaces(sessions);
      if (me.partners && !worksLoaded) { worksLoaded = true; worksWith.load(); }
    } catch (e) {
      if (e.message !== "login required") {
        banner("Could not load chats: " + e.message, true, "list");
        listRetry = setTimeout(refreshList, listBackoff.next());
        // The chats shown stay as they were; with none yet, say why.
        if (!lastList) $("sessions").replaceChildren(el("p", "none", "Cannot reach the agent. Retrying\u2026"));
      }
    } finally {
      listing = false;
    }
  }

  // Where chats run. places holds the place of each chat by backend and
  // session; computers are the user's paired computers.
  const places = new Map();
  let computers = [];
  const placeKey = (b, s) => b + "\n" + s;
  const placeOf = (s) => places.get(placeKey(s.backend_id, s.id));
  const base = (dir) => (dir || "").replace(/\/+$/, "").split("/").pop() || dir || "";

  // placeText is how a chat's place reads in the list and the header.
  function placeText(p, long) {
    if (!p) return "";
    const off = p.online ? "" : " (offline)";
    if (p.kind === "computer") {
      const where = (p.mine ? p.name : "another computer") + (p.dir ? (long ? ", in " : " \u00B7 ") + (long ? p.dir : base(p.dir)) : "");
      return (long ? "On " : "") + where + off;
    }
    if (p.kind === "sandbox") return (long ? "In the sandbox" : "Sandbox") + off;
    if (p.kind === "none") return long ? "Not on a computer" : "No computer";
    return "";
  }

  async function loadComputers() {
    try {
      computers = await api("GET", "/api/computers");
    } catch (_) {
      computers = [];
    }
    chats.showChevron(computers.length > 0);
  }

  let placing = false;
  async function loadPlaces(sessions) {
    if (placing) return;
    const shown = sessions.filter((s) => s.running || s.message_count > 0 || isCurrent(s) || s.archived);
    if (!shown.length) return;
    placing = true;
    try {
      const got = await api("POST", "/api/places", { sessions: shown.map((s) => ({ backend_id: s.backend_id, session_id: s.id })) });
      for (const x of got) places.set(placeKey(x.backend_id, x.session_id), x.place);
      if (!menuOpen || !document.querySelector("#sessions .menu")) drawList();
    } catch (_) {
      /* the list works without places */
    } finally {
      placing = false;
    }
  }

  const isCurrent = (s) => current && current.b === s.backend_id && current.s === s.id;

  // pinnedChats are the pinned chats in the order the user set; chats
  // pinned before pins had an order, and ties, go by recent activity.
  function pinnedChats() {
    if (!lastList) return [];
    return lastList.sessions.filter((s) => s.pinned && !s.archived).sort((x, y) =>
      ((x.pin_pos || 0) - (y.pin_pos || 0)) || (Date.parse(y.updated_at) - Date.parse(x.updated_at)));
  }

  function drawList() {
    if (!lastList || pinOrder.defer()) return;
    const { backends, sessions } = lastList;
    menuOpen = false;
    const nav = $("sessions");
    const held = document.activeElement;
    const heldRow = held && held.closest && nav.contains(held) ? held.closest(".item") : null;
    const heldKey = heldRow && heldRow.dataset.key;
    const heldMore = !!held && held.classList.contains("more");
    nav.replaceChildren();
    if (!backends.length) {
      nav.append(el("p", "none", "No assistant is set up for you yet."));
      $("newchat").disabled = true;
      return;
    }
    $("newchat").disabled = false;
    const q = $("search").value.trim().toLowerCase();
    const match = (s) => !q || (s.title || t("newchat")).toLowerCase().includes(q);
    const shown = (s) => s.running || s.message_count > 0 || isCurrent(s);
    const order = (x, y) => (y.running - x.running) || (Date.parse(y.updated_at) - Date.parse(x.updated_at));
    const many = backends.length > 1;
    let found = 0;
    const pinned = pinnedChats().filter(match);
    if (pinned.length) {
      const group = el("div", "group pinned");
      const h = el("div", "group-head");
      h.append(el("span", "grow", "Pinned"));
      h.title = q ? "Clear the search to reorder pins" : "Drag to reorder, or drag a chat here to pin it. On a touch screen, press and hold a chat first.";
      group.append(h);
      for (const s of pinned) group.append(chatItem(s, many));
      if (!q) pinOrder.enable(group);
      found += pinned.length;
      nav.append(group);
    }
    for (const b of backends) {
      const group = el("div", "group recent");
      if (many || pinned.length) {
        const h = el("div", "group-head");
        h.append(el("span", "grow", many ? b.name : "Recent"));
        if (many) {
          const nb = window.HubIcons.button("plus", "New chat on " + b.name);
          nb.onclick = () => newChat(b.id);
          h.append(nb);
        }
        group.append(h);
      }
      const mine = sessions.filter((s) => s.backend_id === b.id && !s.archived && shown(s)).sort(order);
      // With the chats shown (the hub's copy), the top bar says it all.
      if (b.error && !mine.length) group.append(el("p", "err", "Unreachable: " + b.error));
      const hits = mine.filter((s) => match(s) && !s.pinned);
      found += hits.length;
      if (!mine.length && !b.error) group.append(el("p", "none", "No chats yet."));
      if (!hits.length && mine.length && !b.error && !many) continue;
      for (const s of hits) group.append(chatItem(s));
      if (!q) pinOrder.enable(group);
      nav.append(group);
    }
    if (q && !found) nav.append(el("p", "none", "No chat titles match."));
    const open = sessions.find(isCurrent);
    if (open) chats.seen(open);
    if (sideTop !== null) { nav.scrollTop = sideTop; sideTop = null; }
    search.draw(nav);
    if (heldKey) refocus(heldKey, heldMore);
    const archived = sessions.filter((s) => s.archived).sort(order);
    if (archived.length) {
      const tg = el("button", "link archived-toggle", (showArchived ? "Hide" : "Show") + " archived chats (" + archived.length + ")");
      tg.type = "button";
      tg.onclick = () => { showArchived = !showArchived; drawList(); };
      nav.append(tg);
      if (showArchived) {
        const group = el("div", "group archived");
        for (const s of archived.filter(match)) group.append(chatItem(s));
        nav.append(group);
      }
    }
  }

  // refocus puts keyboard focus back on a row after the list was redrawn:
  // on its Options button when it was there, else on the chat.
  function refocus(key, onMore) {
    const row = Array.from(document.querySelectorAll("#sessions .item")).find((r) => r.dataset.key === key);
    const go = row && row.querySelector(".go");
    const more = row && row.querySelector(".more");
    if (!go) return;
    go.focus();
    if (onMore && more) more.focus();
  }

  function chatItem(s, many) {
    const row = el("div", "item" + (s.unread && !s.running ? " unread" : ""));
    row.dataset.key = s.backend_id + "\n" + s.id;
    if (isCurrent(s)) row.classList.add("active");
    const a = el("a", "go");
    a.href = chatHash(s.backend_id, s.id);
    if (!s.archived) a.draggable = false;
    a.append(chats.mark(s));
    const badge = s.activity > 0 ? el("span", "badge", String(s.activity)) : null;
    if (badge) badge.title = s.activity + " running beside this chat: subagents and background commands";
    const p = placeOf(s);
    let where = computers.length || (p && p.kind === "computer") ? placeText(p, false) : "";
    if (s.pinned && many && lastList) {
      const b = lastList.backends.find((x) => x.id === s.backend_id);
      if (b) where = b.name + (where ? " \u00B7 " + where : "");
    }
    const col = el("span", "col");
    col.append(el("span", "name", s.title || t("newchat")));
    if (where) col.append(el("span", "where" + (p && !p.online ? " off" : ""), where));
    a.append(col);
    if (badge) a.append(badge);
    const when = el("span", "when", ago(s.updated_at));
    if (when.textContent) when.title = new Date(s.updated_at).toLocaleString();
    a.append(when, ...chats.tail(s), ...worksWith.mark(s));
    const state = chats.label(s);
    a.setAttribute("aria-label", (s.title || t("newchat")) + (state ? ", " + state : ""));
    a.onclick = () => openSide(false);
    const more = window.HubIcons.button("more", "Chat options", "more");
    more.setAttribute("aria-label", "Options for " + (s.title || t("newchat")));
    more.setAttribute("aria-expanded", "false");
    more.setAttribute("aria-haspopup", "menu");
    more.onclick = () => chatMenu(row, s, more);
    row.append(a, more);
    return row;
  }

  function button(cls, text, onclick) {
    const b = el("button", cls, text);
    b.type = "button";
    b.onclick = onclick;
    return b;
  }

  // chatMenu opens the options of one chat right under it.
  function chatMenu(row, s, more) {
    const open = row.nextSibling && row.nextSibling.classList && row.nextSibling.classList.contains("menu");
    for (const m of document.querySelectorAll("#sessions .menu")) m.remove();
    for (const b of document.querySelectorAll("#sessions .more")) b.setAttribute("aria-expanded", "false");
    menuOpen = !open;
    if (open) { drawList(); return; }
    more.setAttribute("aria-expanded", "true");
    const menu = el("div", "menu");
    menu.setAttribute("role", "menu");
    const close = () => drawList();
    const extra = chats.menu(s, close);
    const sbase = "/api/sessions/" + encodeURIComponent(s.backend_id) + "/" + encodeURIComponent(s.id);
    const act = async (btns, path, body, failText, after) => {
      for (const b of btns) b.disabled = true;
      try {
        await api("POST", sbase + path, body);
        if (after) after();
        close();
        refreshList();
      } catch (e) {
        for (const b of btns) b.disabled = false;
        banner(failText + e.message, true);
      }
    };
    const rename = button("", "Rename", () => {
      const form = el("form", "rename");
      const input = el("input");
      input.value = s.title || "";
      input.maxLength = 100;
      input.setAttribute("aria-label", "New name");
      const save = button("primary", "Save");
      save.type = "submit";
      const cancel = button("", "Cancel", close);
      form.onsubmit = (ev) => {
        ev.preventDefault();
        const title = input.value.trim();
        if (!title) { input.focus(); return; }
        act([save, cancel], "/rename", { title }, "Could not rename: ", () => {
          s.title = title;
          if (isCurrent(s)) setTitle(title);
        });
      };
      input.onkeydown = (ev) => { if (ev.key === "Escape") { ev.stopPropagation(); close(); } };
      form.append(input, save, cancel);
      menu.replaceChildren(form);
      input.focus();
      input.select();
    });
    const archive = button("", s.archived ? "Unarchive" : "Archive", () =>
      act([archive], "/archive", { archived: !s.archived }, s.archived ? "Could not unarchive: " : "Could not archive: ",
        s.archived ? () => { unhid = s.id; } : null));
    const del = button("danger-text", "Delete", () => {
      const box = el("div", "confirm");
      box.append(el("p", "", "Delete this chat for good? This cannot be undone."));
      const yes = button("danger", "Delete");
      const no = button("", "Cancel", close);
      yes.onclick = () => act([yes, no], "/delete", undefined, "Could not delete: ", () => {
        if (isCurrent(s)) { notice = "Chat deleted."; location.hash = ""; }
      });
      box.append(yes, no);
      menu.replaceChildren(box);
      no.focus();
    });
    rename.setAttribute("role", "menuitem");
    archive.setAttribute("role", "menuitem");
    menu.append(...extra.top, ...(s.archived ? [] : pinOrder.items(s, close)), rename, ...worksWith.menuItems(s, close), extra.export, archive);
    const p = placeOf(s);
    if (computers.some((c) => c.backend_id === s.backend_id)) {
      menu.append(button("", "Move to computer...", () => { close(); picker.open({ move: s, place: p }); }));
    }
    if (p && p.kind === "computer" && p.mine && !p.inherited) {
      const rel = button("", "Stop using this computer", () => {
        const box = el("div", "confirm");
        box.append(el("p", "", "Stop using " + p.name + " for this chat? Talaria then cannot run commands or change files " +
          "for this chat anywhere, also not in the sandbox, until you move it to a computer again."));
        const yes = button("danger", "Stop using it");
        const no = button("", "Cancel", close);
        yes.onclick = () => act([yes, no], "/release", undefined, "Could not change this chat: ", () => {
          places.delete(placeKey(s.backend_id, s.id));
          if (isCurrent(s)) loadPlace();
        });
        box.append(yes, no);
        menu.replaceChildren(box);
        no.focus();
      });
      menu.append(rel);
    }
    menu.append(del);
    menu.addEventListener("keydown", (e) => {
      const items = Array.from(menu.querySelectorAll(":scope > button:not(:disabled)"));
      const i = items.indexOf(document.activeElement);
      if ((e.key === "ArrowDown" || e.key === "ArrowUp") && items.length && i >= 0) {
        e.preventDefault();
        items[(i + (e.key === "ArrowDown" ? 1 : items.length - 1)) % items.length].focus();
      } else if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        close();
        const again = document.querySelector('#sessions a.go[href="' + CSS.escape(chatHash(s.backend_id, s.id)) + '"]');
        if (again) again.parentNode.querySelector(".more").focus();
      }
    });
    row.after(menu);
    const first = menu.querySelector("button");
    if (first) first.focus();
  }

  async function newChat(backendID) {
    const btn = $("newchat");
    btn.disabled = true;
    try {
      const id = backendID || me.default_backend || (await api("GET", "/api/backends"))[0]?.id;
      if (!id) { banner("No assistant is set up for you yet.", true); return; }
      const s = await api("POST", "/api/sessions", { backend_id: id });
      openSide(false);
      location.hash = chatHash(id, s.id);
    } catch (e) {
      if (e.status === 409 && !page) {
        // Hermes has no model: the chat area says so, with the way out.
        location.hash = "";
        showWelcome();
        checkModels(true);
        return;
      }
      banner("Could not start a chat: " + e.message, true);
    } finally {
      btn.disabled = false;
    }
  }

  // checkModels shows the first-run card in the chat area while the
  // backend's Hermes has no model a chat can use, and the usual welcome
  // otherwise. A hub without the models routes keeps the welcome.
  let modelSeq = 0;
  async function checkModels(force) {
    const seq = ++modelSeq;
    const b = me.default_backend || (lastList && lastList.backends && lastList.backends[0] && lastList.backends[0].id) ||
      (await api("GET", "/api/backends").catch(() => []))[0]?.id;
    if (!b || seq !== modelSeq) return;
    let st;
    try {
      st = await api("GET", "/api/models/" + encodeURIComponent(b) + "/status");
    } catch (_) {
      if (!force) return;
      st = null;
    }
    if (seq !== modelSeq || view || page || gone || $("welcome").hidden) return;
    const box = $("welcome");
    box.classList.remove("first-run");
    $("modelbar")?.remove();
    if (!st || st.ready) {
      box.replaceChildren(el("p", "", "Pick a chat, or start a new one."));
      return;
    }
    box.classList.add("first-run");
    const bar = modelsUI.firstRun(box, Object.assign({ backend: b }, st), () => checkModels(true));
    bar.id = "modelbar";
    box.after(bar);
  }

  // modelsChanged follows a connected model or a new default: the model
  // menu asks Hermes for fresh lists, and the chat area checks again.
  async function modelsChanged() {
    if (!view) {
      checkModels(true);
      return;
    }
    const chat = view;
    try {
      const m = await api("GET", chat.base + "/models?refresh=1");
      if (chat !== view) return;
      settings.models = m;
      settings.fill();
    } catch (_) { settings.models = null; }
  }

  // showPlace shows where the open chat runs, and a note when its
  // computer is offline or it is on none.
  function showPlace(p) {
    const chat = view;
    if (!chat) return;
    chat.place = p && p.kind ? p : null;
    const line = $("place");
    const note = $("placenote");
    const input = $("input");
    line.hidden = !chat.place || chat.place.kind === "server";
    line.textContent = placeText(chat.place, true);
    line.classList.toggle("off", !!chat.place && !chat.place.online);
    line.title = computers.some((c) => c.backend_id === chat.backend) ? "Change where this chat runs" : "";
    const pl = chat.place;
    $("filesbtn").hidden = !me.files || !pl || !((pl.kind === "computer" && pl.mine) || pl.kind === "sandbox");
    note.replaceChildren();
    note.hidden = true;
    input.placeholder = page ? (chat.running ? "Steer the reply" : "Ask about this window") : "Message Talaria";
    if (chat.place && chat.place.kind === "computer" && !chat.place.online) {
      const name = chat.place.mine ? chat.place.name : "The computer";
      note.append(el("span", "", name + " is offline. Talaria cannot run commands or change files there until it is back, " +
        "so messages you send now will fail. Nothing waits for it."));
      input.placeholder = name + " is offline";
      note.hidden = false;
    } else if (chat.place && chat.place.kind === "sandbox" && !chat.place.online) {
      note.append(el("span", "", "The sandbox is not running right now. Talaria cannot run commands for this chat until it is back."));
      input.placeholder = "The sandbox is not running";
      note.hidden = false;
    } else if (chat.place && chat.place.kind === "none") {
      note.append(el("span", "", "This chat is not on a computer, so Talaria cannot run commands or change files for it."));
      input.placeholder = "Not on a computer";
      note.hidden = false;
    }
    if (!note.hidden && computers.some((c) => c.backend_id === chat.backend)) {
      note.append(button("", "Move to computer...", () => picker.open({ move: { backend_id: chat.backend, id: chat.session }, place: chat.place })));
    }
    drawInfo(chat.info);
  }

  async function loadPlace() {
    const chat = view;
    if (!chat || !me.computers) return;
    try {
      const p = await api("GET", chat.base + "/place");
      if (chat !== view) return;
      places.set(placeKey(chat.backend, chat.session), p);
      showPlace(p);
    } catch (_) {
      if (chat === view && chat.place === undefined) showPlace(null);
    }
  }

  function closeStream() {
    if (stream) stream.close();
    stream = null;
  }

  // Chat view state for one session.
  // The log stays at its end while the reader is at the end, and on the
  // same message while the reader is above it (HubStick). Only the reader's
  // own scrolling moves between the two; a reply that grows, a composer
  // that grows, a panel, a banner or the window's size do not.
  let unseen = 0;
  const ui = window.HubUIState($("log"));
  // stickProbe finds the first message in view, through the boxes that
  // hold older messages, and how far below the top of the log it starts.
  function stickProbe(l) {
    const top = l.getBoundingClientRect().top;
    let box = l;
    for (;;) {
      const n = Array.from(box.children).find((c) => !c.classList.contains("acts") && c.getBoundingClientRect().bottom > top);
      if (!n) return null;
      if (n.classList.contains("older") || n.classList.contains("part")) { box = n; continue; }
      return { n, off: n.getBoundingClientRect().top - top };
    }
  }
  // rowPlace names where the reader is by the nearest message row the
  // server numbered, which a snapshot draws again under the same number.
  function rowPlace(log) {
    const at = stickProbe(log);
    if (!at) return null;
    const rows = Array.from(log.querySelectorAll("[data-row]"));
    const before = rows.filter((r) => r === at.n || (r.compareDocumentPosition(at.n) & Node.DOCUMENT_POSITION_FOLLOWING));
    const r = before.length ? before[before.length - 1] : rows[0];
    if (!r) return null;
    return { row: r.dataset.row, off: r.getBoundingClientRect().top - log.getBoundingClientRect().top };
  }
  function rowTop(log, row) {
    const r = log.querySelector('[data-row="' + CSS.escape(row) + '"]');
    return r ? r.getBoundingClientRect().top - log.getBoundingClientRect().top : null;
  }
  const stick = (() => {
    const log = $("log");
    const jump = $("tobottom");
    const place = (n, l) => (n.isConnected ? n.getBoundingClientRect().top - l.getBoundingClientRect().top : null);
    const s = window.HubStick(log, {
      probe: stickProbe,
      place,
      changed: () => {
        if (s.pinned()) unseen = 0;
        // A chat that is still loading has no latest to jump to.
        jump.hidden = s.pinned() || log.hidden || !!(view && !view.loaded);
        const label = unseen ? (unseen === 1 ? t("newone") : t("newmany").replace("%d", unseen)) : t("tobottom");
        jump.querySelector(".lbl").textContent = label;
        jump.title = label;
      },
    });
    let queued = false;
    const soon = () => {
      if (queued) return;
      queued = true;
      requestAnimationFrame(() => { queued = false; s.sync(); });
    };
    const ro = new ResizeObserver(() => s.sync());
    ro.observe(log);
    for (const n of log.children) ro.observe(n);
    new MutationObserver((list) => {
      for (const m of list) {
        for (const n of m.addedNodes) if (n.nodeType === 1) ui.apply(n);
        if (m.target === log) {
          for (const n of m.removedNodes) if (n.nodeType === 1) ro.unobserve(n);
          for (const n of m.addedNodes) if (n.nodeType === 1) ro.observe(n);
        }
      }
      soon();
    }).observe(log, { childList: true, subtree: true, characterData: true });
    log.addEventListener("scroll", () => s.scrolled(), { passive: true });
    let swiped = false;
    log.addEventListener("wheel", (e) => { s.heard(400); if (e.deltaY < 0) s.release(); }, { passive: true });
    log.addEventListener("touchstart", () => { swiped = false; }, { passive: true });
    // A touch only says the reader may scroll; whether the reader left the
    // end is for the scroll events to tell (a tap or a long press that
    // moves a pixel is not a scroll).
    log.addEventListener("touchmove", () => { swiped = true; s.heard(8000); }, { passive: true });
    const lifted = () => s.heard(swiped ? 3000 : 1);
    log.addEventListener("touchend", lifted, { passive: true });
    log.addEventListener("touchcancel", lifted, { passive: true });
    let down = false;
    log.addEventListener("pointerdown", () => { down = true; s.heard(8000); }, { passive: true });
    for (const up of ["pointerup", "pointercancel", "mouseup"]) {
      window.addEventListener(up, () => { if (down) { down = false; s.heard(300); } }, { passive: true });
    }
    document.addEventListener("keydown", (e) => {
      const tag = e.target && e.target.tagName;
      if (tag === "TEXTAREA" || tag === "INPUT" || tag === "SELECT") return;
      if (["PageUp", "PageDown", "Home", "End", "ArrowUp", "ArrowDown", " "].includes(e.key)) {
        s.heard(800);
        if (["PageUp", "Home", "ArrowUp"].includes(e.key)) s.release();
      }
    }, { capture: true, passive: true });
    log.addEventListener("click", (e) => ui.record(e), true);
    log.addEventListener("load", soon, true);
    log.addEventListener("click", (e) => {
      if (!ui.busy() && s.pinned() && e.target.closest && e.target.closest("summary, [aria-expanded], .notice-more")) s.release();
      soon();
    }, true);
    log.addEventListener("toggle", soon, true);
    window.addEventListener("resize", soon);
    if (window.visualViewport) window.visualViewport.addEventListener("resize", soon);
    jump.addEventListener("click", () => { s.pin(); });
    // For tests that drive the page: true once the log has taken in its size.
    window.HubLogSettled = s.settled;
    return s;
  })();

  // A snapshot renders its newest rows first: at least firstRows rows and
  // firstChars characters, in whole turns. The older rows follow in the
  // background, olderRows at a time.
  const firstRows = 30;
  const firstChars = 30000;
  const olderRows = 60;
  const olderChars = 60000;

  // olderCut is where the rows to render next start: back from end until
  // they have rows rows and chars characters, then back to the user
  // message that starts the turn, so a turn and its tool cards render
  // together.
  function olderCut(msgs, end, rows, chars) {
    let i = end;
    let n = 0;
    let size = 0;
    while (i > 0 && (n < rows || size < chars)) {
      i--;
      n++;
      const m = msgs[i] || {};
      size += (typeof m.text === "string" ? m.text.length : 0) + (typeof m.content === "string" ? m.content.length : 0);
    }
    while (i > 0 && (msgs[i] || {}).role !== "user") i--;
    return i;
  }

  // partHeight guesses the height of rows before they are laid out, for
  // a part of older history the browser skips while it is out of view.
  function partHeight(rows) {
    let chars = 0;
    for (const m of rows) chars += (typeof m.text === "string" ? m.text.length : 0) + (typeof m.content === "string" ? m.content.length : 0);
    return Math.round(rows.length * 40 + chars * 0.3);
  }

  // later runs fn when the page is idle, soon in any case.
  const later = (fn) => (window.requestIdleCallback ? requestIdleCallback(fn, { timeout: 100 }) : setTimeout(fn, 16));

  class Chat {
    constructor(backend, session) {
      this.backend = backend;
      this.session = session;
      this.base = "/api/sessions/" + encodeURIComponent(backend) + "/" + encodeURIComponent(session);
      this.log = $("log");
      // loaded turns true with the first snapshot; until then the log
      // shows a loading state, not an empty chat.
      this.loaded = false;
      this.reset();
    }

    reset() {
      this.log.replaceChildren();
      this.older = [];
      this.olderBox = null;
      this.olderGen = (this.olderGen || 0) + 1;
      this.sink = null;
      this.turn = null;
      this.tools = new Map();
      this.wraps = new Map();
      if (this.cards) this.cards.dispose();
      this.cards = window.HubApprovals.create({ el, api, base: this.base, words: t, banner, add: (box) => this.add(box) });
      this.pending = [];
      this.failed = [];
      this.at = 0;
      this.status = null;
      this.info = {};
      this.compactions = undefined;
      for (const u of this.urls || []) if (!this.keep || !this.keep.has(u)) URL.revokeObjectURL(u);
      this.urls = [];
    }

    // stick tells whether a change should scroll the log to its end. Not
    // while a snapshot fills the log: scrolling reads the log's height,
    // which lays it out again, so a scroll per row made a long chat's
    // load quadratic. The snapshot scrolls once at the end.
    stick() { return stick.pinned() && !this.filling; }

    bottom() { stick.pin(); }

    add(node) {
      if (this.sink) return this.sink.appendChild(node);
      const follow = this.stick();
      if (this.status && this.status.parentNode === this.log) this.log.insertBefore(node, this.status);
      else this.log.append(node);
      if (follow) this.bottom();
      else if (!this.filling && view === this) {
        if (node.classList.contains("msg")) unseen++;
        stick.refresh();
      }
      return node;
    }

    follow(fn) {
      const follow = this.stick();
      fn();
      if (follow) this.bottom();
    }

    // when puts the time ms under a message, after the buttons that sit
    // there; a message shows its time once, and a time that is not known
    // shows nothing.
    when(node, ms) {
      if (!window.HubTimes.valid(ms)) return;
      if (node.whenLine && node.whenLine.dataset.at) return;
      if (!node.whenLine) {
        node.whenLine = el("div", "when" + (node.classList.contains("user") ? " mine" : ""));
        let ref = node;
        while (ref.nextElementSibling && ["queued", "lost", "acts"].some((c) => ref.nextElementSibling.classList.contains(c))) ref = ref.nextElementSibling;
        ref.after(node.whenLine);
      }
      window.HubTimes.stamp(node.whenLine, ms);
    }

    setRunning(on) {
      this.running = !!on;
      $("subtitle").textContent = on ? t("working") : "";
      if (view === this) {
        composer.setRunning(on);
        msgActs.decorate();
        if (page) page.running(on);
      }
    }

    ensureTurn() {
      if (this.turn) return this.turn;
      this.turn = { reasoning: null, text: null, raw: "" };
      return this.turn;
    }

    reasoning(text) {
      const tn = this.ensureTurn();
      if (!tn.reasoning) {
        const d = el("details", "reasoning");
        d.open = true;
        d.append(el("summary", "", "Thinking"), el("div", "body"));
        tn.reasoning = this.add(d);
        tn.text = null;
      }
      this.follow(() => { tn.reasoning.lastChild.textContent += text; });
    }

    // assistantText adds to the reply. A streamed delta renders at most
    // once per frame; anything else renders at once.
    assistantText(text, streamed) {
      if (typeof text !== "string") text = "";
      const tn = this.ensureTurn();
      if (!tn.text) {
        tn.text = this.add(el("div", "msg assistant md"));
        tn.raw = "";
        if (tn.reasoning) ui.closeUnlessKept(tn.reasoning);
      }
      tn.raw += text;
      const node = tn.text;
      node.raw = tn.raw;
      if (!streamed) {
        node.queued = false;
        this.follow(() => this.showText(node, true));
        return;
      }
      if (node.queued) return;
      node.queued = true;
      requestAnimationFrame(() => {
        if (!node.queued) return;
        node.queued = false;
        this.follow(() => this.showText(node, false));
      });
    }

    // showText renders a reply without its page blocks and MEDIA
    // directives. Once the reply is final it shows the pages and the files
    // they name; while it streams it also hides a directive or page that is
    // still being typed.
    showText(node, final) {
      const pg = window.HubPageBlocks.pageBlocks(node.raw || "", !final);
      const m = mediaDirectives(pg.text);
      render(node, final ? m.text : hidePending(m.text), final ? (p, label, image) => markdownMedia(this.base, p, label, image) : null);
      // A page whose closing fence has streamed in gets its card at once;
      // only a page still being typed shows the wait note.
      if (pg.pages.length) {
        const row = el("div", "media");
        for (const html of pg.pages) row.append(pageCard(this.base, { html }));
        node.append(row);
      }
      if (!final && pg.writing) node.append(el("div", "note page-wait", "Writing a page"));
      const paths = m.paths.filter((p) => !(this.published && this.published.has(p)));
      if (final && paths.length) node.append(mediaRow(this.base, paths));
      if (final) this.when(node, this.at);
    }

    // endText shows the files of the reply segment that ends here.
    endText() {
      const tn = this.turn;
      if (!tn || !tn.text) return;
      tn.text.queued = false;
      this.follow(() => this.showText(tn.text, true));
    }

    // originCard shows what HubOrigin.classify said of a user message that
    // the user did not type: nothing for a hidden one, else its card (see
    // notice.js). It returns true when that is all the message shows, and
    // false when the user's bubble with v.text follows. A queued card shows
    // as waiting until its turn starts.
    originCard(v, text, waiting) {
      if (v.show === "hidden") return true;
      const n = v.notice;
      if (!n) return false;
      if (v.show === "user") {
        this.add(window.HubNotice.card(n, text, { el, toast, at: waiting ? 0 : this.at }));
        return false;
      }
      if (!this.agentWaiting) this.agentWaiting = new Map();
      const key = n.id || "text:" + text;
      const shown = this.agentWaiting.get(key);
      if (shown) {
        this.agentWaiting.delete(key);
        shown.classList.remove("pending");
        if (shown.waitLabel) shown.waitLabel.remove();
        window.HubNotice.stamp(shown, this.at, { el });
        return true;
      }
      const box = window.HubNotice.card(n, text, { el, toast, at: waiting ? 0 : this.at });
      box.dataset.ukey = "n:" + ui.hash(text);
      if (n.id) box.title = n.id;
      if (n.kind === "agent") worksWith.decorate(box, n.id, this.backend, this.session);
      this.add(box);
      if (waiting) {
        box.classList.add("pending");
        box.waitLabel = el("div", "queued", t("waiting"));
        box.after(box.waitLabel);
        this.agentWaiting.set(key, box);
      }
      return true;
    }

    // userText adds a user message. atts are the files the page sent with
    // it; without them they come from the lines Hermes keeps in the text.
    userText(text, pending, atts) {
      const shown = atts ? { text, atts } : splitAttachments(text);
      const from = window.HubPageSend.split(shown.text);
      shown.text = from.text;
      const m = el("div", "msg user" + (pending ? " pending" : ""));
      if (from.page) m.append(el("div", "frompage", "From page: " + from.page));
      if (shown.atts.length) m.append(attachmentRow(shown.atts, this.base));
      if (shown.text) window.HubLinkify.append(m, shown.text);
      m.rawText = shown.text || "";
      if (atts) for (const a of atts) if (a.url) this.urls.push(a.url);
      this.add(m);
      if (pending) this.pending.push({ bubble: m, label: null, text, id: null, at: Date.now() });
      else this.when(m, this.at);
      return m;
    }

    // label replaces the line under a pending message.
    label(p, node) {
      if (p.label) p.label.remove();
      p.label = node;
      p.bubble.after(node);
    }

    // held marks a message the hub keeps until Hermes is back.
    held(p, id) {
      p.id = id;
      p.held = true;
      this.label(p, el("div", "queued", t("held")));
    }

    // heldEvent handles input.held: a message this page sent, or one sent
    // from somewhere else while Hermes is away.
    heldEvent(d) {
      let p = this.pending.find((x) => x.id === d.id);
      if (!p) {
        const typed = splitAttachments(d.text || "").text;
        p = this.pending.find((x) => !x.id && !x.admitted && sameText(x.text, typed));
      }
      if (!p) {
        this.userText(d.text || "", true);
        p = this.pending[this.pending.length - 1];
      }
      this.held(p, d.id);
    }

    sentEvent(d) {
      let p = this.pending.find((x) => x.id === d.id);
      if (!p) {
        const i = this.failed.findIndex((x) => x.id === d.id);
        if (i < 0) return;
        p = this.failed.splice(i, 1)[0];
        p.bubble.classList.remove("failed");
        p.bubble.classList.add("pending");
        this.pending.push(p);
      }
      p.held = false;
      p.sent = true;
      this.label(p, el("div", "queued", t("queued")));
    }

    notSentEvent(d) {
      const i = this.pending.findIndex((x) => x.id === d.id);
      const p = i >= 0 ? this.pending.splice(i, 1)[0] : null;
      const text = p ? p.text : (d.text || "");
      const box = el("div", "lost");
      box.append(el("div", "", t("notsent") + (d.message ? " " + d.message : "")));
      if (!p && text) box.append(el("div", "lost-text", text));
      if (text || (p && p.files && p.files.length)) {
        const b = el("button", "", t("again"));
        b.type = "button";
        b.onclick = () => resend(this, text, p, box);
        box.append(b);
      }
      if (!p) {
        this.add(box);
        return;
      }
      p.id = d.id;
      p.bubble.classList.remove("pending");
      p.bubble.classList.add("failed");
      this.label(p, box);
      this.failed.push(p);
    }

    // queued marks a sent message the backend has accepted but not started.
    // With an id (from a snapshot) it also offers Cancel.
    queued(bubble, id, text) {
      const p = this.pending.find((x) => x.bubble === bubble);
      if (!p || (p.label && !id)) return;
      if (id) p.admitted = true;
      const label = el("div", "queued", id ? t("waiting") : t("queued"));
      if (id) {
        const c = el("button", "link", "Cancel");
        c.type = "button";
        c.onclick = () => this.inputAction(id, "cancel", c);
        label.append(" ", c);
      }
      if (this.running) label.append(" ", this.nowButton(p));
      this.label(p, label);
    }

    // steered marks a message Hermes hands to the running turn: the agent
    // reads it after its current step.
    steered(p) {
      p.steered = true;
      p.bubble.classList.remove("pending");
      const label = el("div", "queued", t("steered"));
      label.append(" ", this.nowButton(p));
      this.label(p, label);
    }

    // nowButton stops the running turn and runs p as the next turn.
    nowButton(p) {
      const b = el("button", "link", t("now"));
      b.type = "button";
      b.title = "Stop the running turn, then run this message next";
      b.onclick = () => this.stopAndSend(p, b);
      return b;
    }

    async stopAndSend(p, btn) {
      if (!p.id) return;
      btn.disabled = true;
      p.now = true;
      try {
        const r = await api("POST", this.base + "/inputs/" + encodeURIComponent(p.id) + "/now");
        if (r && r.id) p.id = r.id;
        p.steered = false;
        if (this.pending.includes(p)) {
          p.bubble.classList.add("pending");
          this.label(p, el("div", "queued", t("queued")));
        }
        toast("Stopping the turn; Talaria reads your message next");
      } catch (e) {
        p.now = false;
        banner("Could not stop and send: " + e.message, true);
        if (this.pending.includes(p)) this.unread(p);
      }
    }

    // transferEnded handles the end of a message Hermes handed to the
    // running turn: read, or dropped when the turn stopped first.
    transferEnded(d) {
      const i = this.pending.findIndex((x) => (x.id && (x.id === d.submission_id || x.id === d.admission_id)) || (x.adm && x.adm === d.admission_id));
      if (i < 0) return;
      const p = this.pending[i];
      if (d.outcome === "steered") {
        this.pending.splice(i, 1);
        p.bubble.classList.remove("pending");
        if (p.label) p.label.remove();
        return;
      }
      // Stop and send now sends it again; a cancelled queued message is not a steer.
      if (p.now || !p.steered) return;
      this.unread(p);
    }

    // unread marks a steer that no turn read, with Send again.
    unread(p) {
      const i = this.pending.indexOf(p);
      if (i >= 0) this.pending.splice(i, 1);
      const box = el("div", "lost");
      box.append(el("div", "", t("unread")));
      const b = el("button", "", t("again"));
      b.type = "button";
      b.onclick = () => resend(this, p.text, p, box);
      box.append(b);
      p.bubble.classList.add("failed");
      this.label(p, box);
      this.failed.push(p);
    }

    lost(id, text) {
      const box = el("div", "lost");
      box.append(el("div", "", t("lost")));
      if (text) box.append(el("div", "lost-text", text));
      const b = el("button", "", "Dismiss");
      b.type = "button";
      b.onclick = () => this.inputAction(id, "dismiss", b);
      box.append(b);
      this.add(box);
    }

    async inputAction(id, action, btn) {
      btn.disabled = true;
      try {
        await api("POST", this.base + "/inputs/" + encodeURIComponent(id) + "/" + action);
      } catch (e) {
        btn.disabled = false;
        banner((action === "cancel" ? "Could not cancel: " : "Could not dismiss: ") + e.message, true);
      }
    }

    // started shows the input a turn started with: this page's own message
    // when it sent it, else the message sent from another app.
    started(text, note, id, admission, start) {
      start = start || {};
      // A producer's input (note, such as cron) is named the way Hermes names it.
      const v = window.HubOrigin.classify({ text, display_kind: start.display_kind, display_text: start.display_text,
        submission_id: note ? JSON.stringify([note]) : id });
      if (this.originCard(v, text)) return;
      let i = id || admission ? this.pending.findIndex((x) => (x.id && (x.id === id || x.id === admission)) || (x.adm && x.adm === admission)) : 0;
      if (i < 0 && text) {
        const typed = splitAttachments(v.text != null ? v.text : text).text;
        // A stopped steer sent again starts under a new ID, maybe before the page learns it.
        i = this.pending.findIndex((x) => (!x.id || x.now) && sameText(x.text, typed));
      }
      const p = i >= 0 ? this.pending.splice(i, 1)[0] : null;
      if (p) {
        p.bubble.classList.remove("pending");
        if (p.label) p.label.remove();
        this.when(p.bubble, this.at);
      } else if (v.text) {
        this.userText(v.text);
      }
    }

    computerName() {
      const p = placeOf({ backend_id: this.backend, id: this.session });
      return p && p.kind === "computer" && p.mine ? p.name || "" : "";
    }

    toolCard(id, name, args) {
      let card = this.tools.get(id);
      if (card) return card;
      if (!this.known) this.known = window.HubToolCard.memory();
      const call = { name: name || "tool", args, res: null, out: "", done: false, failed: false, startedAt: this.at, endedAt: 0,
        duration: undefined, computer: this.computerName(), mem: this.known };
      const view = window.HubToolCard.create({ el, toast, highlight: highlightBash, diff: diffView, page: (p) => this.publishedPage(p) }, call);
      card = view.node;
      card.view = view;
      card.call = call;
      card.toolName = call.name;
      card.dataset.ukey = "t:" + id;
      card.classList.add("running");
      if (activity.opens(name, args)) {
        const open = el("button", "link tool-open", "Show in Activity");
        open.type = "button";
        open.onclick = () => activity.fromTool(name, args, card.result);
        view.body.append(open);
      }
      if (name === "terminal" && args && typeof args.command === "string" && this.canBackground && !args.background) {
        view.live.append(this.backgroundActs(id));
      }
      this.tools.set(id, card);
      this.endText();
      // A card ends the thinking before it and the reply text: what comes
      // next opens a new block after the card, so the log keeps the order
      // the steps happened in.
      const tn = this.ensureTurn();
      tn.text = null;
      if (tn.reasoning) ui.closeUnlessKept(tn.reasoning);
      tn.reasoning = null;
      this.add(card);
      return card;
    }

    // publishedPage is the card of a page the agent published. The reply
    // that names the same file shows no second card.
    publishedPage(p) {
      (this.published || (this.published = new Set())).add(p.path);
      return pageCard(this.base, p);
    }

    // waitingNotes marks the memory cards that still run as waiting for the
    // user's approval while Hermes asks, and clears it when the question is
    // answered. Only a question that is about notes or memory marks them.
    waitingNotes(p, on) {
      if (on && !/memory|note/i.test(String(p.description || "") + " " + String(p.command || "") + " " + String(p.tool || p.tool_name || ""))) return;
      for (const card of this.tools.values()) {
        if (card.finished || (card.toolName !== "memory" && card.toolName !== "project_memory") || !!card.call.pending === on) continue;
        card.call.pending = on;
        card.view.update(card.call);
      }
    }

    tagBatch(card, id) {
      card.batch = id;
      card.call.batch = id;
    }

    // toolCards draws the card of a live tool call. A tool_call of Hermes
    // carries several calls: each gets its own card, as when saved rows
    // load, and they fold with the cards around them.
    toolCards(id, name, args) {
      const w = this.wraps.get(id);
      if (w) return w.cards;
      const list = window.HubToolCard.calls(id, name, args);
      if (!list) return [this.toolCard(id, name, args)];
      const cards = list.map((x) => {
        const c = this.toolCard(x.key, x.name, x.args);
        if (x.batch) this.tagBatch(c, x.batch);
        return c;
      });
      this.wraps.set(id, { cards });
      return cards;
    }

    // toolsDone finishes the card or cards of a live call. Hermes ends a
    // tool_call with one event; when its result is a list with a result
    // for each call, each card gets its own, else each shows the result.
    toolsDone(p) {
      const cards = this.toolCards(p.tool_id, p.name, p.args);
      if (!this.wraps.has(p.tool_id)) {
        this.toolDone(cards[0], p);
        return;
      }
      const parts = window.HubToolCard.split(p.result, cards.length);
      cards.forEach((card, i) => {
        const own = parts && parts.length === cards.length ? parts[i] : undefined;
        const one = own === undefined ? p : Object.assign({}, p, { result: own, result_text: undefined });
        this.toolDone(card, one);
      });
    }

    // backgroundActs is the Run in background button of a running terminal
    // card: Hermes moves the command to its background commands and the
    // turn goes on.
    backgroundActs(id) {
      const acts = el("div", "acts");
      const b = el("button", "", "Run in background");
      b.type = "button";
      b.title = "Keep the command running in the background and let Talaria go on";
      b.onclick = async () => {
        b.disabled = true;
        try {
          await api("POST", this.base + "/tools/" + encodeURIComponent(id) + "/background");
          b.textContent = "Moving to the background";
        } catch (e) {
          b.disabled = false;
          banner("Could not move the command to the background: " + e.message, true);
        }
      };
      acts.append(b);
      return acts;
    }

    // toolDone shows a finished tool card. at is when it finished: the time
    // of the event or row that says so, 0 for a card that no event closed.
    toolDone(card, p, at = this.at) {
      if (!card.classList.contains("running") && card.finished) return;
      card.finished = true;
      card.view.live.replaceChildren();
      if (p.result !== undefined) card.result = p.result;
      card.classList.remove("running");
      const call = card.call;
      const { res, failed, out } = window.HubToolCard.outcome(card.toolName, p);
      card.failed = failed;
      if (failed) card.classList.add("failed");
      call.done = true;
      call.res = res;
      call.failed = failed;
      call.error = typeof p.error === "string" ? p.error : "";
      call.out = out || "";
      call.diff = typeof p.inline_diff === "string" ? p.inline_diff : "";
      call.endedAt = at || 0;
      if (typeof p.duration_s === "number") {
        card.duration = p.duration_s;
        call.duration = p.duration_s;
      }
      this.known.learn(call.name, call.args, res);
      card.view.update(call);
      if (card.toolName === "agent_send") worksWith.refusal(card, call.out + " " + call.error, this.backend, this.session);
      if (card.toolName === "browser_screenshot" && !failed && res && typeof res.path === "string" && mediaPath(res.path)) {
        card.view.body.append(mediaRow(this.base, [res.path], "shot"));
      }
    }

    // groupTools folds each run of finished tool cards into one line that
    // opens to show them. keep is a node after which cards stay as they are.
    groupTools(keep, parent = this.log) {
      window.HubToolGroup.fold(el, parent, keep, (c) => ui.expanded(c));
    }

    showInfo(p) {
      if (!p) return;
      if (p.model && this.info.model && p.model !== this.info.model) {
        // The old model's window is wrong for the new one; Hermes reports the new one on the next reply.
        delete this.info.context_length;
        if (this.info.usage) this.info.usage = { ...this.info.usage, context_max: undefined, context_used: undefined };
      }
      for (const k of ["model", "provider", "cwd", "reasoning_effort", "context_length"]) {
        if (p[k] !== undefined && p[k] !== null) this.info[k] = p[k];
      }
      if (p.usage && typeof p.usage === "object") {
        this.info.usage = p.usage;
        this.countCompactions(p.usage.compressions);
      }
      drawInfo(this.info);
    }

    // countCompactions notes a compaction that the usage counter shows and
    // no "compacted" status reported.
    countCompactions(n) {
      if (typeof n !== "number") return;
      if (typeof this.compactions === "number" && n > this.compactions) this.compacted();
      this.compactions = Math.max(n, this.compactions || 0);
    }

    compacted() {
      if (typeof this.compactions === "number") this.compactions++;
      this.add(el("div", "note", "Older messages were summarized to make room."));
    }

    // pickEvent shows what Hermes decided about a model pick that waited
    // for the end of a turn: a question to answer, or a pick it dropped.
    pickEvent(p) {
      const ask = window.HubModelPick.ask(p);
      if (ask) {
        $("info").open = true;
        settings.askConfirm(ask.message, ask.token, ask.provider, ask.model);
        return;
      }
      const text = window.HubModelPick.refusal(p);
      if (text) {
        settings.note(text, true);
        settings.fill();
      }
    }

    deleted() {
      if (view !== this) return;
      notice = "This chat was deleted.";
      closeStream();
      location.hash = "";
    }

    approval(p) { this.cards.offer(p); }

    settle(id, text) { this.cards.outcome(id, text); }

    setStatus(text) {
      if (!text) {
        if (this.status) this.status.remove();
        this.status = null;
        return;
      }
      if (!this.status) {
        this.status = el("div", "status");
        this.log.append(this.status);
      }
      this.status.textContent = text;
    }

    // adopt gives x, the pending message that the snapshot shows, what the
    // page still holds of the one it is sending: its key, files and state.
    adopt(x, c) {
      if (!c || !x) return;
      x.cid = c.cid;
      x.files = c.files;
      x.atts = c.atts;
      x.inflight = c.inflight;
    }

    snapshot(s) {
      if (!$("banner").classList.contains("bad")) clearBanner("away");
      const kept = window.HubReconcile.anchor(this.log, stick.pinned(), rowPlace(this.log));
      const carried = window.HubReconcile.carry(this.pending, s);
      this.keep = new Set(carried.filter((c) => c.act === "again").flatMap((c) => (c.p.atts || []).map((a) => a.url)));
      this.reset();
      this.keep = null;
      this.loaded = true;
      this.canBackground = !!s.tool_background;
      setTitle(s.title);
      this.setRunning(s.running);
      activity.running(s.running);
      this.showInfo(s.info || {});
      // Stored tool rows carry no arguments; the assistant message that made the call has them.
      this.calls = new Map();
      this.filling = true;
      mark("rows");
      const msgs = s.messages || [];
      const cut = olderCut(msgs, msgs.length, firstRows, firstChars);
      this.older = msgs.slice(0, cut);
      // A split snapshot sends its older rows next (snapshot.older).
      this.olderWait = Number.isInteger(s.older) && s.older > 0;
      this.afterOlder = undefined;
      try {
        for (let i = cut; i < msgs.length; i++) this.row(msgs[i]);
      } finally {
        this.filling = false;
        this.at = 0;
      }
      mark("rows-end");
      this.turn = null;
      const users = this.log.querySelectorAll(".msg.user");
      this.groupTools(s.running && users.length ? users[users.length - 1] : null);
      mark("grouped");
      this.agentWaiting = new Map();
      const mine = new Map();
      for (const c of carried) if (c.act === "adopt") mine.set(c.at, c.p);
      (s.pending || []).forEach((p, i) => {
        if (p.status === "lost") this.lost(p.id, p.text);
        else if (p.status === "steering") {
          // Hermes also hands a background result or another agent's message to the running turn; the
          // sender named in the input ID decides if it is the user's bubble or a card.
          const v = window.HubOrigin.classify(p);
          if (this.originCard(v, p.text, true)) return;
          this.userText(v.text, true);
          const x = this.pending[this.pending.length - 1];
          x.id = window.HubReconcile.idOf(p);
          x.adm = p.id;
          this.adopt(x, mine.get(i));
          this.steered(x);
        } else if (!this.originCard(window.HubOrigin.classify(p), p.text, true)) {
          const bubble = this.userText(window.HubOrigin.classify(p).text, true);
          this.queued(bubble, p.id);
          const x = this.pending.find((y) => y.bubble === bubble);
          if (x) {
            x.id = window.HubReconcile.idOf(p);
            x.adm = p.id;
            this.adopt(x, mine.get(i));
          }
        }
      });
      for (const c of carried) {
        if (c.act !== "again") continue;
        const bubble = this.userText(c.p.text, true, c.p.atts);
        const x = this.pending.find((y) => y.bubble === bubble);
        this.adopt(x, c.p);
        x.id = c.p.id;
      }
      for (const p of s.prompts || []) this.approval(p);
      const j = jump;
      if (this.olderWait && (kept !== null || window.HubRestore.state() || (j && j.b === this.backend && j.s === this.session))) {
        // The place to show may be in the rows still coming: show the end
        // and go to the place when they are here.
        this.afterOlder = kept;
        this.bottom();
      } else this.place(kept);
      if (this.onLoad) this.onLoad();
      this.onLoad = null;
    }

    // place puts the reader where the snapshot found them (kept), at the
    // message a search opened, or where a reload left them; else at the end.
    place(kept) {
      if (kept !== null || window.HubRestore.state()) this.showOlder(() => true);
      if (window.HubReconcile.restore(this.log, kept, (p) => rowTop(this.log, p.row))) stick.look();
      else this.bottom();
      this.jumpTo();
      this.fillOlder();
      if (view === this) restoreView(this);
    }

    // olderRows takes the rows a split snapshot sent after its newest ones.
    olderRows(p) {
      if (!this.olderWait) return;
      this.olderWait = false;
      const idle = !this.older.length;
      this.older = (p.messages || []).concat(this.older);
      if (this.afterOlder !== undefined) {
        const kept = this.afterOlder;
        this.afterOlder = undefined;
        this.place(kept);
      } else if (idle) this.fillOlder();
    }

    // showOlder renders older history that the snapshot left out above
    // what the log shows, newest first, about olderRows rows in whole
    // turns at a time, until more says stop or the history is done. The
    // rows render apart from the live turn. The parts go in one box at the
    // top of the log, each in a box of its own that the browser skips while
    // it is out of view (content-visibility): the log is a flex column, and
    // Firefox lays out every message in it again on any change, and Chrome
    // all its children when one goes in at the top. A reader who is not at
    // the end keeps the same messages in view.
    showOlder(more) {
      if (!this.older.length) return;
      const frag = document.createDocumentFragment();
      const turn = this.turn;
      this.filling = true;
      try {
        do {
          const cut = olderCut(this.older, this.older.length, olderRows, olderChars);
          const part = this.older.slice(cut);
          this.older.length = cut;
          const chunk = el("div", "part");
          chunk.style.containIntrinsicSize = "auto " + partHeight(part) + "px";
          this.sink = chunk;
          this.turn = null;
          for (const m of part) this.row(m);
          this.groupTools(null, chunk);
          frag.prepend(chunk);
        } while (this.older.length && more && more(frag));
      } finally {
        this.sink = null;
        this.filling = false;
        this.turn = turn;
        this.at = 0;
      }
      const log = this.log;
      if (!this.olderBox || this.olderBox.parentNode !== log) {
        this.olderBox = el("div", "older");
        log.insertBefore(this.olderBox, log.firstChild);
      }
      this.olderBox.insertBefore(frag, this.olderBox.firstChild);
      stick.sync();
      if (view === this) msgActs.decorate();
    }

    // fillOlder renders the rest of the history in the background, a few
    // turns at a time, so the newest messages show first.
    fillOlder() {
      const gen = this.olderGen;
      const step = () => {
        if (view !== this || gen !== this.olderGen || !this.older.length) return;
        this.showOlder();
        if (this.older.length) later(step);
        else mark("filled");
      };
      later(step);
    }

    // jumpTo scrolls to the message a search result opened the chat at.
    jumpTo() {
      const j = jump;
      if (!j || j.b !== this.backend || j.s !== this.session) return;
      jump = null;
      const sel = Number.isInteger(j.row) ? '[data-row="' + j.row + '"]' : "";
      if (sel && !this.log.querySelector(sel)) this.showOlder((frag) => !frag.querySelector(sel));
      const n = sel ? this.log.querySelector(sel) : null;
      if (!n) {
        this.add(el("div", "note", "The message you searched for is further back than this chat shows."));
        return;
      }
      n.scrollIntoView({ block: "center" });
      stick.look();
      n.classList.add("flash");
      setTimeout(() => n.classList.remove("flash"), 2500);
    }

    row(m) {
      if (m.display_kind === "hidden") return;
      const text = m.text != null ? m.text : (typeof m.content === "string" ? m.content : "");
      const rid = Number.isInteger(m.row_id) ? m.row_id : m._row_id;
      this.at = window.HubTimes.valid(m.timestamp * 1000) ? Math.round(m.timestamp * 1000) : 0;
      if (m.role === "user") {
        this.turn = null;
        const v = window.HubOrigin.classify(m);
        if (this.originCard(v, text)) return;
        const b = this.userText(v.text);
        if (b && Number.isInteger(rid)) b.dataset.row = rid;
        return;
      }
      if (m.role === "assistant") {
        const sum = m.archived ? window.HubNotice.parse(text) : null;
        if (sum && sum.kind === "compaction") {
          this.add(window.HubNotice.card(sum, text, { el, toast, at: this.at }));
          return;
        }
        for (const c of m.tool_calls || []) {
          const fn = c.function || {};
          let args = fn.arguments;
          if (typeof args === "string") {
            try { args = JSON.parse(args); } catch (_) { args = { arguments: args }; }
          }
          const call = { name: fn.name, args };
          const inner = window.HubToolCard.wrapped(fn.name, args);
          if (inner) {
            call.inner = inner;
            call.batchSize = inner.length;
          }
          if (c.id) this.calls.set(c.id, call);
          if (c.call_id) this.calls.set(c.call_id, call);
        }
        const r = m.reasoning || m.reasoning_content;
        if (r) { this.reasoning(r); ui.closeUnlessKept(this.turn.reasoning); }
        if (text) {
          this.assistantText(text);
          if (Number.isInteger(rid) && !this.turn.text.dataset.row) this.turn.text.dataset.row = rid;
          // A reply stored in pieces cut inside a fenced block goes on in
          // the same bubble, so its page block stays whole.
          if (!window.HubPageBlocks.open(this.turn.raw)) this.turn.text = null;
        }
        return;
      }
      if (m.role === "tool") {
        let call = (this.calls && this.calls.get(m.tool_call_id)) || {};
        if (call.inner) {
          const one = window.HubToolCard.take(call.inner, m.name || m.tool_name);
          if (one) call = Object.assign(one, { unwrapped: true, batch: call.batchSize > 1 ? m.tool_call_id || "" : "" });
        }
        const at = this.at;
        this.at = 0;
        const card = this.toolCard(m.tool_call_id && !call.unwrapped ? m.tool_call_id : "row" + m.row_id, m.name || m.tool_name || call.name, m.args || call.args);
        if (call.batch) this.tagBatch(card, call.batch);
        this.at = at;
        this.toolDone(card, { result: m.content != null ? m.content : text });
        return;
      }
      if (text) this.add(el("div", "note", text));
    }

    event(kind, p, at) {
      p = p || {};
      this.at = window.HubTimes.valid(at) ? at : 0;
      this.dispatch(kind, p);
      this.at = 0;
      activity.event(kind, p);
    }

    dispatch(kind, p) {
      switch (kind) {
        case "snapshot": this.snapshot(p); break;
        case "snapshot.older": this.olderRows(p); break;
        case "backend.status":
          if (p.connected) clearBanner("away");
          else if (bannerKey !== "away" || !$("banner").classList.contains("bad")) banner(AWAY_NOTE, false, "away", true);
          break;
        case "message.start":
          // A message handed to the running turn can arrive while a reply
          // streams. The reply goes on after it, so a reply that is inside
          // an open fence stays one text: cut in two, its page block would
          // lose its opening fence and show as raw text.
          if (!(this.turn && this.turn.text && window.HubPageBlocks.open(this.turn.raw))) this.turn = null;
          this.setRunning(true);
          this.started(p.text, p.note, p.submission_id, p.admission_id, p);
          break;
        case "reasoning.delta": this.reasoning(p.text || ""); break;
        case "reasoning.available":
          if (!this.ensureTurn().reasoning) { this.reasoning(p.text || ""); ui.closeUnlessKept(this.turn.reasoning); }
          break;
        case "thinking.delta": this.setStatus(p.text || ""); break;
        case "status.update":
          if (p.kind === "compacted") { this.setStatus(""); this.compacted(); }
          else if (p.kind === "compacting") this.setStatus("Summarizing older messages to make room...");
          else if (p.text) this.setStatus(p.text);
          break;
        case "tool.generating": this.setStatus("Preparing " + (p.name || "a tool") + "..."); break;
        case "tool.start": this.setStatus(""); this.toolCards(p.tool_id, p.name, p.args); break;
        case "tool.progress": {
          const card = this.toolCards(p.tool_id, p.name)[0];
          const live = card.view.live;
          let bar = live.querySelector("progress");
          if (!bar && p.total) { bar = el("progress"); live.append(bar); }
          if (bar && p.total) { bar.max = p.total; bar.value = p.progress; }
          let msg = live.querySelector(".progress");
          if (p.message) {
            if (!msg) { msg = el("div", "status progress"); live.append(msg); }
            msg.textContent = p.message;
          }
          break;
        }
        case "tool.complete": this.toolsDone(p); break;
        case "message.delta": this.setStatus(""); if (typeof p.text === "string" && p.text) this.assistantText(p.text, true); break;
        case "message.interim":
          if (p.text && !p.already_streamed) {
            this.assistantText(p.text);
            // A piece that ends inside a fenced block: the next one goes on in the same bubble.
            if (!window.HubPageBlocks.open(this.turn.raw)) this.turn.text = null;
          }
          break;
        case "message.complete": {
          // The end of a message handed to the running turn, not of a turn.
          if (p.outcome && !p.status && p.admission_id) { this.transferEnded(p); break; }
          this.setStatus("");
          const tn = this.ensureTurn();
          if (!tn.text && !tn.raw && typeof p.text === "string" && p.text) this.assistantText(p.text);
          else this.endText();
          if (tn.reasoning) ui.closeUnlessKept(tn.reasoning);
          if (p.status && p.status !== "complete" && p.status !== "completed") {
            this.add(el("div", "note" + (p.status === "error" || p.status === "failed" ? " bad" : ""), "Turn " + p.status + (p.error ? ": " + p.error : "")));
          }
          for (const card of this.tools.values()) if (card.classList.contains("running")) this.toolDone(card, {}, 0);
          this.groupTools(null);
          if (p.usage) this.showInfo({ usage: p.usage });
          claude.afterReply(this.backend, this.info.provider, this.session, this.info.model || (this.info.usage || {}).model || "");
          this.turn = null;
          this.setRunning(false);
          refreshList().then(() => {
            const s = lastList && lastList.sessions.find(isCurrent);
            if (s && view === this) chats.seen(s);
          });
          break;
        }
        case "session.info": this.showInfo(p); break;
        case "session.usage": this.showInfo({ usage: p.usage }); break;
        case "session.updated":
          if (typeof p.title === "string") setTitle(p.title);
          if (p.operation === "model" && p.model) this.showInfo({ model: p.model, provider: p.provider });
          this.pickEvent(p);
          if (p.operation === "delete") this.deleted();
          break;
        case "session.deleted": this.deleted(); break;
        case "input.held": this.heldEvent(p); break;
        case "input.sent": this.sentEvent(p); break;
        case "input.not_sent": this.notSentEvent(p); break;
        case "approval.request": case "clarify.request": this.approval(p); if (kind === "approval.request") this.waitingNotes(p, true); break;
        case "approval.settled": case "clarify.settled": this.cards.settled(p.prompt_id); this.waitingNotes(p, false); break;
        case "approval.outcome": this.cards.outcome(p.prompt_id, outcomeText(p)); break;
        case "approval.cancelled": this.cards.cancelled(p.request_ids); this.waitingNotes(p, false); break;
        case "prompt.lost": this.cards.lost(p.prompt_ids); this.waitingNotes(p, false); break;
        case "error": this.add(el("div", "note bad", p.message || "Error")); break;
        case "claude.signin": this.add(claude.chatNotice(this.backend, p)); break;
        default: break;
      }
    }
  }

  // outcomeText says what became of an approval and where it got its answer,
  // as the hub's approval.outcome event tells it. An outcome the hub worked out
  // from the tool call has no choice, so it names no scope.
  function outcomeText(p) {
    const verbs = { once: "Allowed once", session: "Allowed for this chat", always: "Allowed always" };
    const head = !p.allowed ? "Denied" : verbs[p.choice] || "Allowed";
    const where = { hub: "in the hub", telegram: "in Telegram" }[p.via] ||
      (p.via === "device" ? "on " + (p.device || "a paired device") : "in another app");
    return head + " (" + where + ")";
  }

  const render = (node, raw, local) => window.hubMarkdown.render(node, raw, local);

  // MEDIA:<path> directives in a reply name files on the Hermes server or
  // the chat's computer for the user. This follows hub.MediaDirectives in the hub, which decides
  // what the media route serves; the page only shows what it finds.
  const mediaExts = new Set(("png jpg jpeg gif webp bmp tiff svg mp4 mov avi mkv webm 3gp mp3 m2a wav ogg opus m4a " +
    "flac pdf docx doc odt rtf txt md epub xlsx xls ods csv tsv json xml yaml yml kmz kml geojson gpx pptx ppt odp key " +
    "zip tar gz tgz bz2 xz 7z rar apk ipa html htm").split(" "));
  const baseName = (p) => p.replace(/\/+$/, "").split("/").pop() || p;
  const imageExts = new Set(["png", "jpg", "jpeg", "gif", "webp"]);
  const extOf = (p) => { const b = baseName(p); const i = b.lastIndexOf("."); return i >= 0 ? b.slice(i + 1).toLowerCase() : ""; };
  const imagePath = (p) => imageExts.has(extOf(p));
  const utf8 = new TextEncoder();

  function mediaPath(p) {
    if (typeof p !== "string" || p.length < 2 || p[0] !== "/" || utf8.encode(p).length > 4096) return false;
    if (/[\x00-\x1f\x7f]/.test(p)) return false;
    if (p.slice(1).split("/").some((s) => s === "" || s === "." || s === "..")) return false;
    return mediaExts.has(extOf(p));
  }

  // maskMedia blanks fenced code, inline code and quote lines, so examples
  // there are not directives. Inline code that is a whole directive, or the
  // quoted path of one, stays.
  function maskMedia(text) {
    const lines = text.split("\n");
    let fence = "";
    for (let i = 0; i < lines.length; i++) {
      const f = lines[i].match(/^[ \t]{0,3}(```|~~~)/);
      if (fence) {
        if (f && f[1] === fence) fence = "";
        lines[i] = " ".repeat(lines[i].length);
      } else if (f) {
        fence = f[1];
        lines[i] = " ".repeat(lines[i].length);
      } else if (/^[ \t]*>/.test(lines[i])) {
        lines[i] = " ".repeat(lines[i].length);
      }
    }
    const masked = lines.join("\n");
    return masked.replace(/`[^`\n]+`/g, (code, at) => {
      if (/MEDIA:[ \t]*$/i.test(masked.slice(Math.max(0, at - 20), at))) return code;
      if (/^MEDIA:/i.test(code.slice(1, -1).trim())) return code;
      return " ".repeat(code.length);
    });
  }

  function mediaDirectives(text) {
    if (!/MEDIA:/i.test(text)) return { paths: [], text };
    const scan = maskMedia(text);
    const tag = /[`"'*_]{0,3}MEDIA:[ \t]*(`[^`\n]+`|"[^"\n]+"|'[^'\n]+'|\/[^ \t\n\f\r`"'<>]+)/gi;
    const spans = [];
    const paths = [];
    let m;
    while ((m = tag.exec(scan))) {
      const pStart = m.index + m[0].length - m[1].length;
      let raw = text.slice(pStart, pStart + m[1].length);
      let p;
      if ("`\"'".includes(raw[0])) {
        p = raw.slice(1, -1).trim();
      } else {
        const glued = raw.search(/MEDIA:/i);
        if (glued > 0) {
          raw = raw.slice(0, glued);
          tag.lastIndex = pStart + glued;
        }
        raw = raw.replace(/[.,;:!?)\]}*_]+$/, "");
        p = raw;
      }
      if (!mediaPath(p)) continue;
      let end = pStart + raw.length;
      for (let n = 0; n < 3 && end < text.length && "`\"'*_".includes(text[end]); n++) end++;
      if (text[end] === "." && (end + 1 === text.length || text[end + 1] === " " || text[end + 1] === "\n")) end++;
      spans.push([m.index, end]);
      if (!paths.includes(p)) paths.push(p);
    }
    if (!spans.length) return { paths: [], text };
    let out = "";
    let last = 0;
    for (const [a, b] of spans) {
      out += text.slice(last, a);
      last = b;
    }
    out += text.slice(last);
    return { paths, text: out.replace(/\n{3,}/g, "\n\n").trim() };
  }

  // hidePending cuts a directive still being typed at the end of a
  // streamed reply, so its path never flashes on screen.
  function hidePending(text) {
    const cut = text.search(/(?:[`"'*_]{0,3}MEDIA:[ \t]*(?:[`"'~/][^\n]*)?|(?<![A-Za-z0-9])[`"'*_]{0,3}M(?:E(?:D(?:I(?:A)?)?)?)?)$/i);
    return cut < 0 ? text : text.slice(0, cut);
  }

  // markdownMedia is the node for ![alt](/abs/path) or [label](/abs/path)
  // in a reply, which follows hub.MarkdownMedia: an image inline, a link
  // that downloads the file, a card for an HTML page. It is null for a
  // path the hub would not serve, which shows as its label.
  function markdownMedia(base, p, label, image) {
    if (!mediaPath(p)) return null;
    if (imagePath(p)) {
      const a = mediaImage(base, p);
      if (label) a.querySelector("img").alt = label;
      return a;
    }
    if (pagePath(p)) return pageCard(base, { path: p });
    if (image) return mediaFile(base, p);
    const a = el("a", "media-link", label || baseName(p));
    a.href = mediaURL(base, p) + "&download=1";
    a.download = baseName(p);
    a.rel = "noopener";
    a.title = p;
    return a;
  }

  const mediaURL = (base, p) => base + "/media?path=" + encodeURIComponent(p);

  // openImage shows an image full size over the page. A click anywhere or
  // Escape closes it; the link opens the original in a new tab.
  function openImage(src, name) {
    const prev = document.activeElement;
    const box = el("div", "lightbox");
    box.setAttribute("role", "dialog");
    box.setAttribute("aria-modal", "true");
    box.setAttribute("aria-label", name || "Image");
    box.tabIndex = -1;
    const img = el("img");
    img.src = src;
    img.alt = name || "";
    const bar = el("div", "lightbox-bar");
    const open = el("a", "", "Open original");
    open.href = src;
    open.target = "_blank";
    open.rel = "noopener";
    const shut = el("button", "", "Close");
    shut.type = "button";
    bar.append(el("span", "name", name || ""), open, shut);
    box.append(img, bar);
    // The viewer holds a history entry so Back closes it and nothing else.
    let entry = false;
    try { history.pushState({ lightbox: true }, "", location.href); entry = true; } catch (_) { /* no history entry */ }
    let done = false;
    const close = (fromBack) => {
      if (done) return;
      done = true;
      box.remove();
      document.removeEventListener("keydown", onKey, true);
      window.removeEventListener("popstate", onBack);
      if (entry && fromBack !== true && history.state && history.state.lightbox) history.back();
      // The thumbnail's chip may be gone by now: fall back to the message box.
      const back = prev && prev.isConnected && prev !== document.body ? prev : document.getElementById("input");
      if (back && back.focus) back.focus();
    };
    const onKey = (e) => { if (e.key === "Escape") { e.stopPropagation(); close(); } };
    const onBack = () => close(true);
    box.addEventListener("click", (e) => { if (e.target !== open) close(); });
    document.addEventListener("keydown", onKey, true);
    window.addEventListener("popstate", onBack);
    document.body.append(box);
    box.focus();
  }

  // zoomable makes a thumbnail open in the in-page viewer; a modified or
  // middle click keeps the browser's own behavior.
  function zoomable(node, src, name) {
    node.style.cursor = "zoom-in";
    node.addEventListener("click", (e) => {
      if (e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
      e.preventDefault();
      openImage(typeof src === "function" ? src() : src, name);
    });
  }

  // mediaImage is a thumbnail of an image the agent named that opens
  // full size. It turns into the file's name when the hub cannot show it.
  function mediaImage(base, p, cls, fallback) {
    const a = el("a", cls || "media-img");
    a.href = mediaURL(base, p);
    a.target = "_blank";
    a.rel = "noopener";
    a.title = baseName(p);
    const img = el("img");
    img.alt = baseName(p);
    img.loading = "lazy";
    img.src = a.href;
    img.onerror = () => {
      if (fallback && imagePath(fallback) && mediaPath(fallback) && fallback !== p) {
        const other = fallback;
        fallback = "";
        a.href = img.src = mediaURL(base, other);
      } else a.replaceWith(mediaFile(base, p, true));
    };
    a.append(img);
    zoomable(a, () => a.href, baseName(p));
    return a;
  }

  // mediaFile is a chip that downloads a file the agent named, from the
  // Hermes server or the computer that runs the chat.
  function mediaFile(base, p, gone) {
    const chip = el(gone ? "span" : "a", "att media-file" + (gone ? " gone" : ""));
    chip.title = gone ? "This file is gone, or the computer that has it is offline." : p;
    if (!gone) {
      chip.href = mediaURL(base, p) + "&download=1";
      chip.download = baseName(p);
      chip.rel = "noopener";
    }
    chip.append(el("span", "file", extOf(p) || "file"), el("span", "name", baseName(p)));
    if (gone) chip.append(el("span", "size", "gone"));
    return chip;
  }

  // mediaRow shows the files a reply names: images inline, others as chips.
  function mediaRow(base, paths, cls) {
    const row = el("div", "media" + (cls ? " " + cls : ""));
    for (const p of paths) row.append(imagePath(p) ? mediaImage(base, p) : pagePath(p) ? pageCard(base, { path: p }) : mediaFile(base, p));
    return row;
  }

  // Pages are interactive HTML the agent writes in a reply as a fenced
  // block with the info string "page", or as an HTML file it names with
  // MEDIA. This follows hub.PageBlocks in the hub, which decides what the
  // page route serves; a page block's id is the start of its SHA-256.
  const pagePath = (p) => mediaPath(p) && (extOf(p) === "html" || extOf(p) === "htm");

  async function pageId(html) {
    const d = await crypto.subtle.digest("SHA-256", utf8.encode(html));
    return Array.from(new Uint8Array(d).slice(0, 16), (b) => b.toString(16).padStart(2, "0")).join("");
  }

  const entities = { amp: "&", lt: "<", gt: ">", quot: "\"", apos: "'", nbsp: " " };
  function pageTitle(html) {
    const m = html.slice(0, 65536).match(/<title[^>]*>([\s\S]*?)<\/title>/i);
    if (!m) return "";
    const t = m[1].replace(/&(#x[0-9a-f]+|#[0-9]+|[a-z]+);/gi, (all, e) => {
      if (e[0] === "#") {
        const n = e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10);
        return n > 0 && n <= 0x10ffff && (n < 0xd800 || n > 0xdfff) ? String.fromCodePoint(n) : all;
      }
      return entities[e.toLowerCase()] ?? all;
    }).replace(/\s+/g, " ").trim();
    return t.length > 120 ? t.slice(0, 119) + "…" : t;
  }

  const pageHash = (b, s, ref) => "#/page/" + encodeURIComponent(b) + "/" + encodeURIComponent(s) + "/" + encodeURIComponent(ref);
  // Titles and HTML of the pages the chat's cards show, by ref (a page id
  // or the path of an HTML file).
  const pageInfo = new Map();

  // pageCard shows a page of a reply with its title and an Open button.
  // page is { html } for a page block or { path } for an HTML file.
  function pageCard(base, page) {
    const card = el("div", "page-card");
    const name = el("span", "name", page.html !== undefined ? pageTitle(page.html) || "Page" : page.title || baseName(page.path));
    const kind = el("span", "kind", "Page");
    const open = el("button", "primary", "Open");
    open.type = "button";
    open.disabled = true;
    const text = el("span", "text");
    text.append(name, kind);
    card.append(text, open);
    const ready = (ref) => {
      pageInfo.set(ref, { title: name.textContent, html: page.html, path: page.path });
      open.disabled = false;
      open.onclick = () => {
        if (!view) return;
        pagesView.openedHere = true;
        location.hash = pageHash(view.backend, view.session, ref);
      };
    };
    if (page.path) {
      ready(page.path);
      api("GET", base + "/page-info?path=" + encodeURIComponent(page.path)).then((r) => {
        if (r && r.title) { name.textContent = r.title; pageInfo.get(page.path).title = r.title; }
      }, () => {
        card.classList.add("gone");
        kind.textContent = "Gone";
        card.title = "This file is gone, or the computer that has it is offline.";
        open.disabled = true;
      });
    } else if (window.crypto && crypto.subtle) {
      pageId(page.html).then(ready, () => { kind.textContent = "Cannot open"; });
    } else {
      kind.textContent = "Open the hub over HTTPS to see pages";
    }
    return card;
  }

  // pagesView shows one page of the open chat over the chat, in a frame
  // sandboxed into an opaque origin of its own (allow-same-origin is not
  // set), so it cannot reach the hub or other pages. The page talks back
  // only over the MessagePort sent on its first load; a later load means the
  // frame left the page, so the port closes. Leaving the page drops the frame,
  // so opening the page again starts it afresh.
  const pageSandbox = "allow-scripts allow-forms allow-modals allow-popups";
  const maxPageText = 200000;
  const pagesView = {
    cur: null,
    openedHere: false,

    open(chat, ref) {
      const c = this.cur;
      if (c && c.chat === chat && c.ref === ref) {
        c.box.hidden = false;
        return;
      }
      this.destroy();
      const info = pageInfo.get(ref) || {};
      const file = ref.startsWith("/");
      const box = el("div", "pageview");
      box.setAttribute("role", "dialog");
      box.setAttribute("aria-modal", "true");
      const bar = el("div", "pageview-bar");
      const back = el("button", "", "Back");
      back.type = "button";
      const title = el("span", "name", info.title || (file ? baseName(ref) : "Page"));
      box.setAttribute("aria-label", title.textContent);
      const status = el("span", "status");
      status.setAttribute("aria-live", "polite");
      const save = el("a", "", "Download");
      save.rel = "noopener";
      save.download = (file ? baseName(ref) : (info.title || "page").replace(/[^\w .-]+/g, "").trim().slice(0, 60) || "page") + (file ? "" : ".html");
      if (file) save.href = mediaURL(chat.base, ref) + "&download=1";
      else if (info.html !== undefined) save.href = URL.createObjectURL(new Blob([info.html], { type: "application/octet-stream" }));
      else save.hidden = true;
      const reload = el("button", "", "Reload");
      reload.type = "button";
      bar.append(back, title, status, reload, save);
      const frame = el("iframe");
      frame.setAttribute("sandbox", pageSandbox);
      frame.referrerPolicy = "no-referrer";
      frame.title = title.textContent;
      const src = chat.base + "/page?" + (file ? "path=" : "id=") + encodeURIComponent(ref);
      const note = el("div", "pageview-note");
      note.hidden = true;
      note.setAttribute("role", "status");
      box.append(bar, note, frame);
      const cur = { chat, ref, box, frame, port: null, blob: save.href.startsWith("blob:") ? save.href : "", status, note, timer: 0 };
      const sender = () => window.HubPageSend.sender({
        title: () => (pageInfo.get(ref) || {}).title || title.textContent,
        deliver: (text) => {
          if (view !== chat) throw new Error("The chat is not open");
          return deliver(chat, text, [], undefined, true);
        },
        state: (st) => this.sendState(cur, st),
        reply: (id, r) => { if (id && cur.port) cur.port.postMessage({ type: "result", id, ok: r.ok, error: r.error || "" }); },
      });
      cur.sender = sender();
      cur.act = window.HubPageSend.activation({
        win: window,
        active: () => !!navigator.userActivation && navigator.userActivation.isActive,
        // showPicker on a text input whose list is not in the document ends
        // the activation in Chrome and Firefox and shows nothing. Date and
        // color inputs would open their picker; do not use them.
        consume: () => {
          const i = document.createElement("input");
          i.setAttribute("list", "hub-no-such-list");
          try { i.showPicker(); } catch (_) { /* not active, or not supported: take() refuses */ }
        },
        later: (f, ms) => setTimeout(f, ms),
        cancel: (id) => clearTimeout(id),
      });
      cur.act.settle();
      this.cur = cur;
      let loads = 0;
      frame.addEventListener("load", () => {
        loads++;
        if (cur.port) { cur.port.close(); cur.port = null; }
        if (loads > 1) {
          this.say(cur, "The page left; its buttons no longer reach the chat. Reload to start it again.", 0);
          return;
        }
        const ch = new MessageChannel();
        ch.port1.onmessage = (e) => this.message(cur, e.data);
        cur.port = ch.port1;
        frame.contentWindow.postMessage({ type: "hermes-page:init" }, "*", [ch.port2]);
      });
      back.onclick = () => this.back();
      reload.onclick = () => { loads = 0; this.say(cur, ""); cur.sender = sender(); frame.src = src; };
      box.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); this.back(); } });
      frame.src = src;
      document.body.append(box);
      back.focus();
    },

    // message handles what the page sends over its port.
    // A hidden page is ignored, so it cannot add text to the message box or
    // the clipboard while the user works in the chat.
    message(cur, d) {
      if (cur !== this.cur || !cur.port || cur.box.hidden || !d || typeof d !== "object") return;
      const text = typeof d.text === "string" && d.text.length <= maxPageText ? d.text : null;
      // A send or draft is taken only right after the user's own click or key
      // press in the frame: fresh(cur) reads the hub's own activation.
      if (d.type === "send" && text !== null) this.fresh(cur).then((ok) => cur.sender.send(typeof d.id === "number" ? d.id : 0, text, ok));
      else if (d.type === "draft" && text !== null) this.draft(cur, d.id, text);
      else if (d.type === "copy" && text !== null) this.copy(cur, text);
      else if (d.type === "copied") this.say(cur, "Copied", 2000);
      else if (d.type === "send" || d.type === "draft" || d.type === "copy") this.say(cur, "The page sent too much text.", 4000);
    },

    toComposer(cur, text) {
      if (!text.trim() || view !== cur.chat) return;
      const input = $("input");
      const had = input.value.replace(/\s+$/, "");
      input.value = had ? had + "\n\n" + text : text;
      this.back();
      grow();
      focusInput();
      banner("The page put text in the message box. Edit it, then send it.");
    },

    // fresh answers true once per click or key press of the user in the
    // page's frame (see HubPageSend.activation).
    fresh(cur) {
      return cur.act.take();
    },

    // draft puts a page's text in the message box, with the same rule as a
    // send; a refused draft shows the error the way a refused send does.
    async draft(cur, id, text) {
      const reply = (r) => { if (id && cur.port) cur.port.postMessage({ type: "result", id, ok: r.ok, error: r.error || "" }); };
      if (!(await this.fresh(cur))) {
        this.sendState(cur, { failed: new Error(window.HubPageSend.noClick), text });
        reply({ ok: false, error: window.HubPageSend.noClick });
        return;
      }
      if (cur !== this.cur) return;
      this.toComposer(cur, text);
      reply({ ok: true });
    },

    // sendState shows the result of a page's send in a line under the bar:
    // the page stays open and usable. A failure offers Retry and Copy.
    sendState(cur, st) {
      const n = cur.note;
      clearTimeout(cur.noteTimer);
      n.replaceChildren();
      n.classList.toggle("bad", !!st.failed);
      n.hidden = false;
      if (st.sending) {
        n.append("Sending to the chat\u2026");
      } else if (st.sent) {
        n.append("Sent to the chat at " + st.sent.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" }));
      } else {
        n.append("Could not send: " + st.failed.message);
        const copy = el("button", "", "Copy");
        copy.type = "button";
        copy.onclick = () => this.copy(cur, st.text);
        if (st.retry) {
          const retry = el("button", "", "Retry");
          retry.type = "button";
          retry.onclick = () => st.retry();
          n.append(retry);
        }
        n.append(copy);
      }
    },

    async copy(cur, text) {
      try {
        await navigator.clipboard.writeText(text);
        this.say(cur, "Copied", 2000);
      } catch (_) {
        copyBox(text);
      }
    },

    say(cur, text, ms) {
      clearTimeout(cur.timer);
      cur.status.textContent = text;
      if (ms) cur.timer = setTimeout(() => { cur.status.textContent = ""; }, ms);
    },

    back() {
      const c = this.cur;
      if (!c) return;
      if (this.openedHere && /^#\/page\//.test(location.hash)) {
        this.openedHere = false;
        history.back();
      } else {
        location.replace(chatHash(c.chat.backend, c.chat.session));
      }
    },

    // hide leaves the page for good: the frame goes out of the document. A
    // frame that is only hidden keeps its own process alive, and on Android
    // Chrome its stale touch area can cover the chat and swallow every tap.
    // Back, the Close button and any other exit end here through route.
    hide() {
      this.destroy();
    },

    destroy() {
      const c = this.cur;
      this.cur = null;
      if (!c) return;
      clearTimeout(c.timer);
      if (c.port) c.port.close();
      c.act.off();
      if (c.blob) URL.revokeObjectURL(c.blob);
      c.box.remove();
    },
  };

  // showText shows a long text, such as a background command's output, in
  // a dialog.
  function showText(title, text) {
    if (document.querySelector("dialog.copybox")) return;
    const d = el("dialog", "picker copybox");
    d.append(el("h2", "", title || "Output"));
    const ta = el("textarea");
    ta.readOnly = true;
    ta.value = text;
    ta.setAttribute("aria-label", "Output");
    const body = el("div", "pbody");
    body.append(ta);
    const foot = el("div", "pfoot");
    const shut = el("button", "primary", "Close");
    shut.type = "button";
    shut.onclick = () => d.close();
    foot.append(shut);
    d.append(body, foot);
    d.addEventListener("close", () => d.remove());
    document.body.append(d);
    d.showModal();
    ta.scrollTop = ta.scrollHeight;
  }

  // copyBox shows text selected for the user to copy, when the browser
  // does not let the hub write the clipboard.
  function copyBox(text) {
    if (document.querySelector("dialog.copybox")) return;
    const d = el("dialog", "picker copybox");
    d.append(el("h2", "", "Copy this text"), el("p", "intro", "The browser did not let the page copy it. Select it and copy it yourself."));
    const ta = el("textarea");
    ta.readOnly = true;
    ta.value = text;
    const body = el("div", "pbody");
    body.append(ta);
    const foot = el("div", "pfoot");
    const sel = el("button", "", "Select all");
    sel.type = "button";
    sel.onclick = () => { ta.focus(); ta.select(); };
    const shut = el("button", "primary", "Close");
    shut.type = "button";
    shut.onclick = () => d.close();
    foot.append(sel, shut);
    d.append(body, foot);
    d.addEventListener("close", () => d.remove());
    document.body.append(d);
    d.showModal();
    ta.focus();
    ta.select();
  }

  const splitAttachments = (text) => window.HubAttach.split(text);

  // attachmentRow shows a message's attachments: a thumbnail when the page
  // has the image or the hub can fetch it from Hermes, else the file's name.
  function attachmentRow(atts, base) {
    const row = el("div", "atts");
    for (const a of atts) {
      const fetched = !a.url && base && a.path && imagePath(a.path) && mediaPath(a.path);
      if (fetched) {
        row.append(mediaImage(base, a.path, "att image", a.fallback));
        continue;
      }
      const box = el("div", "att" + (a.url ? " image" : ""));
      if (a.title) box.title = a.title;
      if (a.url) {
        const img = el("img");
        img.alt = a.name;
        img.src = a.url;
        box.append(img);
        zoomable(box, a.url, a.name);
      } else if (a.pdf || /\.pdf$/i.test(a.name)) {
        box.append(pdfIcon(), el("span", "name", a.name));
        if (a.pages) box.append(el("span", "size", pages(a.pages)));
      } else {
        box.append(el("span", "file", a.image ? "image" : "file"), el("span", "name", a.name));
      }
      row.append(box);
    }
    return row;
  }

  const pages = (n) => n + (n === 1 ? " page" : " pages");

  function pdfIcon() {
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("class", "pdf");
    svg.setAttribute("aria-label", "PDF");
    svg.setAttribute("role", "img");
    const path = document.createElementNS(ns, "path");
    path.setAttribute("d", "M6 2h8l6 6v14H6z M14 2v6h6");
    path.setAttribute("fill", "none");
    path.setAttribute("stroke", "currentColor");
    path.setAttribute("stroke-width", "1.6");
    path.setAttribute("stroke-linejoin", "round");
    const label = document.createElementNS(ns, "text");
    label.setAttribute("x", "13");
    label.setAttribute("y", "18");
    label.setAttribute("text-anchor", "middle");
    label.setAttribute("font-size", "6.5");
    label.setAttribute("font-weight", "700");
    label.setAttribute("fill", "currentColor");
    label.textContent = "PDF";
    svg.append(path, label);
    return svg;
  }

  const isPDF = (f) => f.type === "application/pdf" || /\.pdf$/i.test(f.name || "");

  // pdfPages guesses a PDF's page count from the /Count of its page tree,
  // or 0 when the tree is in a compressed object stream. The hub counts
  // the pages itself when the message is sent.
  async function pdfPages(f) {
    try {
      const text = new TextDecoder("latin1").decode(await f.arrayBuffer());
      let n = 0;
      let seen = 0;
      for (const m of text.matchAll(/\/Type\s*\/Pages\b/g)) {
        if (++seen > 1000) break;
        const before = text.slice(Math.max(0, m.index - 2000), m.index);
        const after = text.slice(m.index, m.index + 2000);
        const start = before.lastIndexOf("<<");
        const end = after.indexOf(">>");
        if (start < 0 || end < 0) continue;
        const c = (before.slice(start) + after.slice(0, end)).match(/\/Count\s+(\d+)/);
        if (c) n = Math.max(n, +c[1]);
      }
      return n;
    } catch (_) {
      return 0;
    }
  }

  const pageRange = /^\s*[1-9]\d{0,6}\s*(?:-\s*(?:[1-9]\d{0,6})?\s*)?$/;

  const imageTypes = ["image/png", "image/jpeg", "image/gif", "image/webp"];
  const limits = { count: 10, images: 10, image: 25 << 20, file: 32 << 20, total: 64 << 20 };

  // attachments are the files waiting in the composer.
  const attachments = {
    list: [],
    add(files) {
      for (const f of files) {
        const image = imageTypes.includes(f.type);
        const total = this.list.reduce((n, a) => n + a.file.size, 0) + f.size;
        let why = "";
        if (this.list.length >= limits.count) why = "Attach at most " + limits.count + " files to one message.";
        else if (!f.size) why = f.name + " is empty.";
        else if (image && f.size > limits.image) why = f.name + " is larger than 25 MB, the most Talaria takes for an image.";
        else if (!image && f.size > limits.file) why = f.name + " is larger than 32 MB, the most the hub takes for a file.";
        else if (total > limits.total) why = "The attachments add up to more than 64 MB.";
        if (why) { banner(why, true); break; }
        const a = { file: f, image, url: image ? URL.createObjectURL(f) : "", pdf: !image && isPDF(f), pages: 0, range: "" };
        this.list.push(a);
        if (a.pdf) pdfPages(f).then((n) => { a.pages = n; if (this.list.includes(a)) this.draw(); });
      }
      this.draw();
    },
    remove(a) {
      this.list = this.list.filter((x) => x !== a);
      if (a.url) URL.revokeObjectURL(a.url);
      this.draw();
    },
    clear() {
      for (const a of this.list) if (a.url) URL.revokeObjectURL(a.url);
      this.list = [];
      this.draw();
    },
    take() {
      const l = this.list;
      this.list = [];
      this.draw();
      return l;
    },
    restore(l) {
      this.list = l.concat(this.list);
      this.draw();
    },
    // rangeBox lets the user pick the pages of a PDF that the agent sees,
    // behind a disclosure. Empty means the hub picks.
    rangeBox(a) {
      const d = el("details", "range");
      d.open = !!a.range;
      const s = el("summary", "", a.range ? "pages " + a.range : "pages");
      s.title = "Choose which pages the agent sees";
      const input = el("input");
      input.type = "text";
      input.inputMode = "numeric";
      input.placeholder = "all";
      input.value = a.range;
      input.setAttribute("aria-label", "Pages of " + (a.file.name || "the PDF") + " to show, such as 3-10");
      input.onchange = () => {
        const v = input.value.replace(/\s+/g, "");
        if (v && !pageRange.test(v)) {
          banner("Pick pages as a number or a range such as 3-10.", true);
          input.value = a.range;
          return;
        }
        a.range = v;
        s.textContent = v ? "pages " + v : "pages";
      };
      d.append(s, input);
      return d;
    },
    draw() {
      const box = $("chips");
      box.replaceChildren();
      box.hidden = !this.list.length;
      for (const a of this.list) {
        const chip = el("div", "chip");
        // An image opens full size in the viewer; the thumbnail and the
        // name are one button, the x beside it only removes.
        const head = a.url ? el("button", "open") : chip;
        if (a.url) {
          head.type = "button";
          head.title = "View " + (a.file.name || "attachment");
          head.onclick = () => openImage(a.url, a.file.name || "attachment");
          const img = el("img");
          img.alt = "";
          img.src = a.url;
          head.append(img);
        } else if (a.pdf) {
          chip.append(pdfIcon());
        } else {
          chip.append(el("span", "file", "file"));
        }
        head.append(el("span", "name", a.file.name || "attachment"));
        if (a.url) chip.append(head);
        chip.append(el("span", "size", a.pdf && a.pages ? pages(a.pages) : size(a.file.size)));
        if (a.pdf) chip.append(this.rangeBox(a));
        const x = el("button", "", "\u00d7");
        x.type = "button";
        x.title = "Remove " + (a.file.name || "attachment");
        x.setAttribute("aria-label", x.title);
        x.onclick = () => this.remove(a);
        chip.append(x);
        box.append(chip);
      }
      composer.update();
    },
  };

  function setTitle(title) {
    $("title").textContent = title || t("newchat");
    document.title = (title || t("newchat")) + " - Talaria";
  }

  function highlightBash(code) {
    const hl = window.hljs;
    if (!hl || !hl.getLanguage("bash") || code.length > 100000) return null;
    try {
      return hl.highlight(code, { language: "bash", ignoreIllegals: true }).value;
    } catch (_) {
      return null;
    }
  }

  function parseResult(r) {
    if (r && typeof r === "object") return r;
    if (typeof r !== "string" || r[0] !== "{") return null;
    try { return JSON.parse(r); } catch (_) { return null; }
  }

  // diffView shows Hermes' inline edit diff. Hermes renders it for a
  // terminal, with ANSI colors and a header line; the colors are dropped
  // and each line is styled by its diff prefix instead.
  function diffView(text) {
    const pre = el("pre", "diff");
    const lines = String(text).split("\n");
    lines.forEach((raw, i) => {
      const colored = raw.includes("\x1b[");
      const line = raw.replace(/\x1b\[[0-9;]*[A-Za-z]/g, "");
      if (i === 0 && !colored) return;
      let cls = "";
      if (line.startsWith("@@")) cls = "hunk";
      else if (line.startsWith("+++") || line.startsWith("---")) cls = "file";
      else if (line.startsWith("+")) cls = "add";
      else if (line.startsWith("-")) cls = "del";
      else if (colored && i > 0 && !line.startsWith(" ")) cls = "file";
      pre.append(el("span", cls, line));
    });
    return pre;
  }

  function size(n) {
    const base = n % 1024 === 0 ? 1024 : 1000;
    if (n >= base * base) return +(n / (base * base)).toFixed(1) + "M";
    if (n >= base) return Math.round(n / base) + "K";
    return String(n);
  }

  function tokens(n) {
    if (n >= 1e6) return +(n / 1e6).toFixed(1) + "M";
    if (n >= 1000) return Math.round(n / 1000) + "k";
    return String(n);
  }

  // metric is one meter of the line under the title: a bar on wide
  // screens, a small ring on phones. It turns the warning color at 80%.
  function metric(cls, label, pct, text, longText, title) {
    const sp = el("span", "metric " + cls + (pct >= 80 ? " hi" : ""));
    sp.append(el("span", "lbl", label));
    const meter = el("span", "meter");
    meter.setAttribute("aria-hidden", "true");
    meter.style.setProperty("--p", String(pct));
    const fill = el("span");
    fill.style.width = pct + "%";
    meter.append(fill);
    sp.append(meter, text);
    if (longText) sp.append(el("span", "long", longText));
    if (title) sp.title = title;
    return sp;
  }

  // contextPart shows how much of the model's context window the chat
  // fills.
  function contextPart(info) {
    const u = info.usage || {};
    const max = u.context_max || info.context_length;
    if (!max) return null;
    const used = u.context_used;
    if (typeof used !== "number") {
      const sp = el("span", "metric ctx");
      sp.append(tokens(max), el("span", "long", " context"));
      sp.title = "The model's context window. It shows how full it is after the next reply.";
      return sp;
    }
    const pct = Math.min(100, Math.round((used / max) * 100));
    return metric("ctx", "ctx", pct, (u.context_estimated ? "~" : "") + tokens(used) + "/" + tokens(max), " context",
      pct + "% of the model's context window is in use" + (u.context_estimated ? " (estimated)" : "") +
      ". When it fills up, Talaria summarizes older messages to make room.");
  }

  // limitParts are the meters of the usage windows that the chat's Claude
  // account or ChatGPT plan reports.
  function limitParts(info, at) {
    const c = at || view;
    if (!c || !info.provider) return { name: null, parts: [] };
    const a = claude.account(c.backend, info.provider, c.session, info.model || (info.usage || {}).model || "");
    if (!a) return { name: null, parts: [] };
    const out = [];
    if (a.signed_in) {
      for (const [kind, short, long] of [["session", "5h", "5-hour limit"], ["weekly_all", "week", "Weekly limit"]]) {
        const w = ((a.usage && a.usage.windows) || []).find((x) => x.kind === kind);
        if (!w || typeof w.used_percent !== "number") continue;
        const pct = Math.min(100, Math.round(w.used_percent));
        out.push(metric("lim", short, pct, pct + "%", "", long + ": " + pct + "% used" + (w.resets_text ? ", resets " + w.resets_text : "")));
      }
    }
    return { name: a, parts: out };
  }

  // drawInfo fills the line under the title: model or account, how much
  // of the context the chat uses, this turn's tokens (phones) and the
  // account's limits. It opens the model and thinking settings.
  function drawInfo(info) {
    const u = info.usage || {};
    const model = info.model || u.model;
    const line = $("infoline");
    line.replaceChildren();
    const lim = limitParts(info);
    const apiKey = view && info.provider === claude.apiProvider;
    const who = el("b", "who1");
    if (lim.name) {
      if (lim.name.provider !== claude.chatgptProvider && !/^Claude\b/.test(lim.name.label)) who.append(el("span", "long", "Claude "));
      who.append(lim.name.label);
      who.title = (lim.name.email ? lim.name.label + ": " + lim.name.email : lim.name.label) + (model ? ", " + model : "");
    } else if (apiKey) {
      who.append(claude.apiName);
      who.title = "Anthropic API with an API key" + (model ? ", " + model : "");
    } else {
      who.append(model || (view && !view.loaded ? "\u2026" : "Chat settings"));
      who.title = model ? "Model: " + model : "";
    }
    line.append(who);
    if ((lim.name || apiKey) && model) {
      const m = el("span", "long model", model);
      m.title = "Model";
      line.append(m);
    }
    if (lim.name && !lim.name.signed_in) line.append(el("span", "claude-off", "not signed in"));
    const ctx = contextPart(info);
    if (ctx) line.append(ctx);
    const subs = el("span", "subs");
    subs.id = "subsline";
    line.append(subs);
    drawSubs();
    const mt = el("span", "mtok");
    mt.id = "mtok";
    line.append(mt, el("span", "break"));
    line.append(...lim.parts);
    line.append(el("span", "grow"));
    line.append(el("span", "who2", me.display || me.user || ""));
    $("qpbtn").hidden = !(view && me.agent);
    mirrorTokens();
    const p = view ? view.place : null;
    let cwd = info.cwd;
    if (p) cwd = p.kind === "server" ? info.cwd : "";
    else if (p === undefined && me.computers) cwd = "";
    const cl = $("cwdline");
    cl.hidden = !cwd;
    cl.textContent = cwd ? base(cwd) : "";
    cl.title = cwd ? "Working folder: " + cwd : "";
    if (view && !$("info").hidden) claude.note($("claudenote"), view.backend, info);
    settings.sync(info);
    if (page) {
      page.meters(view ? pageMeters(info) : []);
      if (view) page.rememberInfo(view.backend, info);
    }
  }

  // The page chat before its first message has no chat to ask, so its
  // model list, default model and context window come from the backend
  // (GET /api/backends/<id>/models). idle holds them, the model and thinking
  // level the user picked for the chat that the first message creates, and
  // the state of the load.
  const idle = { backend: "", data: null, at: 0, loading: false, failed: false, pick: null, effort: "" };
  const idleFresh = 5 * 60000;

  // idleBackend is the backend a new page chat runs on: that of the
  // computer that runs the extension, else the user's default.
  function idleBackend() {
    const c = computers.find((x) => x.id === page.device());
    return (c && c.backend_id) || me.default_backend || "";
  }

  async function idleLoad() {
    if (idle.loading) return;
    let backend = idleBackend();
    if (idle.data && idle.backend === backend && Date.now() - idle.at < idleFresh) return;
    idle.loading = true;
    try {
      if (!backend) backend = ((await api("GET", "/api/backends"))[0] || {}).id || "";
      if (!backend) return;
      const data = await api("GET", "/api/backends/" + encodeURIComponent(backend) + "/models");
      if (idle.backend !== backend) { idle.pick = null; idle.effort = ""; }
      Object.assign(idle, { backend, data, at: Date.now(), failed: false });
    } catch (_) {
      idle.failed = true;
      idle.at = Date.now() - idleFresh + 30000;
    } finally {
      idle.loading = false;
    }
    if (!view && page) pageIdle();
  }

  // idleOffers says whether the backend offers a model.
  function idleOffers(provider, model) {
    const ps = (idle.data && idle.data.providers) || [];
    return ps.some((p) => (!provider || p.id === provider) && p.models.some((m) => m.id === model));
  }

  // idleModel is the model the next page chat runs on: the user's pick,
  // else the model of the last page chat if the backend still offers it,
  // else the backend's default.
  function idleModel() {
    const d = idle.data;
    if (!d) return null;
    if (idle.pick && idleOffers(idle.pick.provider, idle.pick.model)) return idle.pick;
    const last = page.lastInfo();
    if (last && last.backend === idle.backend && idleOffers(last.provider, last.model)) return { provider: last.provider, model: last.model };
    return { provider: d.provider, model: d.model };
  }

  // idleContext is the context window of a model before a chat reports it:
  // the one the backend resolved for its default model, 1M for a model
  // whose ID says [1m], 200k for another Claude model, else what the last
  // page chat on that model showed.
  function idleContext(m) {
    const d = idle.data;
    if (m.model === d.model && m.provider === d.provider && d.context_length) return d.context_length;
    if (/\[1m\]$/.test(m.model)) return 1000000;
    if (/^claude-/.test(m.model)) return 200000;
    const last = page.lastInfo();
    return last && last.model === m.model ? last.context_length || 0 : 0;
  }

  // idleInfo is what a chat's info would be: the model, its window, no
  // tokens used.
  function idleInfo() {
    const m = idleModel();
    if (!m) return null;
    const max = idleContext(m);
    return { provider: m.provider, model: m.model, context_length: max, reasoning_effort: idle.effort,
      usage: { model: m.model, context_max: max, context_used: 0 } };
  }

  // pageIdle fills the page chat's meters and model chip before the chat
  // starts: the model, its context window, the account's limits and no
  // tokens. A chat has none yet.
  function pageIdle() {
    if (view) return;
    idleLoad();
    let info = idleInfo();
    if (!info) {
      const last = idle.failed ? page.lastInfo() : null;
      if (!last) { $("info").hidden = true; page.meters([]); return; }
      info = { provider: last.provider, model: last.model, context_length: last.context_length,
        usage: { model: last.model, context_max: last.context_length, context_used: 0 } };
      $("info").hidden = true;
      page.meters(pageMeters(info, { backend: last.backend, session: "" }), info);
      return;
    }
    $("info").hidden = false;
    settings.syncIdle(info);
    page.meters(pageMeters(info, { backend: idle.backend, session: "" }), info);
  }

  // drawSubs shows the chat's running subagents in the line under the
  // title, by model: "subagents: Haiku ×6".
  function drawSubs() {
    const sp = $("subsline");
    if (!sp) return;
    let models = [];
    try { models = activity.liveModels(); } catch (_) { /* before Activity starts */ }
    const counts = new Map();
    for (const m of models) {
      const name = shortModel(m);
      counts.set(name, (counts.get(name) || 0) + 1);
    }
    const parts = [...counts].map(([name, n]) => name + " \u00D7" + n);
    sp.hidden = !parts.length;
    sp.textContent = parts.length ? "subagents: " + parts.join(", ") : "";
    sp.title = parts.length ? "Subagents running now, by model" : "";
  }

  // shortModel names a model in a few letters: Haiku for claude-haiku-5-5.
  function shortModel(id) {
    const m = /^claude-([a-z]+)-/.exec(id || "");
    if (m) return m[1][0].toUpperCase() + m[1].slice(1);
    return id || "model";
  }

  // pageMeters are the page chat's meter row: the context window, the
  // Claude account's windows, and the tokens of the chat.
  function pageMeters(info, at) {
    const out = [];
    const u = info.usage || {};
    const max = u.context_max || info.context_length;
    if (max && typeof u.context_used === "number") {
      const pct = Math.min(100, Math.round((u.context_used / max) * 100));
      out.push(metric("ctx", "", pct, (u.context_estimated ? "~" : "") + tokens(u.context_used) + " / " + tokens(max) + " tokens", "",
        pct + "% of the model's context window is in use" + (u.context_estimated ? " (estimated)" : "") +
        ". When it fills up, Talaria summarizes older messages to make room."));
    } else if (max) {
      out.push(contextPart(info));
    }
    out.push(...limitParts(info, at).parts);
    const tin = u.prompt || u.input || 0;
    const tout = u.output || 0;
    const io = el("span", "metric io", "In " + tokens(tin) + " \u00B7 Out " + tokens(tout));
    io.title = "Tokens this chat has sent to the model (in) and received (out)";
    out.push(io);
    return out;
  }

  // mirrorTokens copies this turn's tokens from the top bar into the line
  // under the title, where phones show them.
  function mirrorTokens() {
    const mt = $("mtok");
    if (!mt) return;
    const tok = $("tok");
    const turn = $("tokturn");
    const txt = tok.hidden ? "" : turn.textContent.replace(/ (this|last) turn$| in this chat$/, "");
    mt.textContent = txt;
    mt.hidden = !txt;
    if (txt) mt.title = "This turn: " + txt;
  }
  new MutationObserver(mirrorTokens).observe($("tok"), { childList: true, subtree: true, characterData: true, attributes: true });

  const effortNames = { "": "Default", none: "Off", minimal: "Minimal", low: "Low", medium: "Medium",
    high: "High", xhigh: "Extra high", max: "Max", ultra: "Ultra" };

  // settings is the model and thinking level panel that opens from the
  // line under the title. It loads the model list when first opened.
  const settings = {
    models: null,
    subagents: null,
    info: {},
    note(text, bad) {
      const n = $("settings-note");
      n.replaceChildren(text || "");
      n.classList.toggle("bad", !!bad);
    },
    reset() {
      this.busy = false;
      this.models = null;
      this.subagents = null;
      this.subSeq++;
      $("subagents-row").hidden = true;
      $("subagents-line").hidden = true;
      this.info = {};
      $("info").open = false;
      this.note("");
    },
    sync(info) {
      this.info = info;
      if ($("info").open) this.fill();
    },
    // syncIdle is sync for a page chat that has not started: the backend's
    // models stand in for the chat's own list.
    syncIdle(info) {
      this.models = idle.data;
      $("subagents-row").hidden = true;
      $("subagents-line").hidden = true;
      this.sync(info);
    },
    async load() {
      const chat = view;
      if (!chat) {
        if (idle.data) this.syncIdle(idleInfo());
        else this.note(idle.failed ? "Could not load the model list." : "Loading models...", idle.failed);
        return;
      }
      this.fill();
      if (!this.subagents) this.loadSubagents();
      if (this.models) return;
      this.note("Loading models...");
      try {
        const m = await api("GET", chat.base + "/models");
        if (chat !== view) return;
        this.models = m;
        this.note("");
        this.fill();
      } catch (e) {
        if (chat === view) this.note("Could not load the model list: " + e.message, true);
      }
    },
    current() {
      const providers = (this.models && this.models.providers) || [];
      const model = this.info.model || (this.models && this.models.model) || "";
      const provider = this.info.provider || (this.models && this.models.provider) || "";
      for (const p of providers) {
        for (const m of p.models) if (m.id === model && (!provider || p.id === provider)) return { p, m };
      }
      return { p: null, m: null, model, provider };
    },
    fill() {
      const sel = $("model");
      const eff = $("effort");
      const cur = this.current();
      if (document.activeElement !== sel) {
        sel.replaceChildren();
        const providers = (this.models && this.models.providers) || [];
        if (!cur.m && cur.model) {
          const o = el("option", "", cur.model);
          o.value = (cur.provider || "") + "\n" + cur.model;
          sel.append(o);
        }
        // Providers with the same group share one heading, as the Claude
        // API credit providers do.
        const group = (p) => p.group || p.name;
        const many = new Set(providers.map(group)).size > 1;
        const heads = new Map();
        for (const p of providers) {
          let parent = sel;
          if (many) {
            parent = heads.get(group(p));
            if (!parent) {
              parent = el("optgroup");
              parent.label = group(p);
              heads.set(group(p), parent);
              sel.append(parent);
            }
          }
          for (const m of p.models) {
            const o = el("option", "", m.label || m.id);
            o.value = p.id + "\n" + m.id;
            if (cur.m === m) o.selected = true;
            parent.append(o);
          }
        }
        const menuBackend = view ? view.backend : idle.backend;
        if ((me.models_admin_backends || []).includes(menuBackend) && providers.length) {
          // Admins get one more line at the menu's foot.
          sel.append(document.createElement("hr"));
          const o = el("option", "connect", "Connect another model");
          o.value = connectValue;
          sel.append(o);
        }
        sel.disabled = !providers.length || this.busy;
      }
      this.route(cur.p ? cur.p.id : cur.provider, cur.m ? cur.m.id : cur.model);
      const reasoning = !cur.m || cur.m.reasoning;
      $("effort-row").hidden = !reasoning;
      if (document.activeElement !== eff) {
        eff.replaceChildren();
        const value = this.info.reasoning_effort || "";
        const efforts = ["", ...((this.models && this.models.efforts) || Object.keys(effortNames).filter((k) => k))];
        if (!efforts.includes(value)) efforts.push(value);
        for (const v of efforts) {
          if (v === "none" && cur.m && !cur.m.can_disable_reasoning && value !== "none") continue;
          const o = el("option", "", effortNames[v] || v);
          o.value = v;
          if (v === value) o.selected = true;
          eff.append(o);
        }
      }
    },
    // loadSubagents reads the chat's Subagents choice. A backend without
    // the subagent-routes plugin answers 404, and the picker stays hidden.
    subSeq: 0,
    async loadSubagents() {
      const chat = view;
      const seq = ++this.subSeq;
      try {
        const s = await api("GET", chat.base + "/subagents");
        if (chat !== view || seq !== this.subSeq) return;
        this.subagents = s;
      } catch (_) {
        if (chat !== view || seq !== this.subSeq) return;
        this.subagents = null;
      }
      this.fillSubagents();
    },
    fillSubagents() {
      const s = this.subagents;
      const sel = $("subagents");
      $("subagents-row").hidden = !s;
      if (!s) { $("subagents-line").hidden = true; return; }
      if (document.activeElement !== sel) {
        sel.replaceChildren();
        const heads = new Map();
        for (const c of s.choices || []) {
          let parent = sel;
          if (c.group) {
            parent = heads.get(c.group);
            if (!parent) {
              parent = el("optgroup");
              parent.label = c.group;
              heads.set(c.group, parent);
              sel.append(parent);
            }
          }
          const o = el("option", "", c.label);
          o.value = c.id;
          if (c.id === s.choice) o.selected = true;
          parent.append(o);
        }
      }
      this.subLine(sel.value || s.choice);
    },
    // subLine says under the pickers what the Subagents choice means.
    subLine(id) {
      const line = $("subagents-line");
      const c = ((this.subagents && this.subagents.choices) || []).find((x) => x.id === id);
      line.hidden = !c;
      line.replaceChildren();
      if (!c) return;
      const at = c.em ? c.line.indexOf(c.em) : -1;
      if (at < 0) line.append(c.line);
      else line.append(c.line.slice(0, at), el("b", "", c.em), c.line.slice(at + c.em.length));
    },
    async pickSubagents() {
      const chat = view;
      const choice = $("subagents").value;
      if (!chat || !this.subagents) return;
      this.subLine(choice);
      const seq = ++this.subSeq;
      try {
        const s = await api("PUT", chat.base + "/subagents", { choice });
        if (chat !== view || seq !== this.subSeq) return;
        this.subagents = s;
        this.note("Subagents set. It applies from the next subagent.");
      } catch (e) {
        if (chat !== view || seq !== this.subSeq) return;
        this.note("Could not change subagents: " + e.message, true);
      }
      this.fillSubagents();
    },
    // route fills the line under the model picker: which account a chat on
    // the Claude provider runs on, or what an API credit provider bills.
    // Only the newest call writes the line: a provider lookup that started
    // before a model change can end after it.
    routeSeq: 0,
    async route(provider, model) {
      const chat = view;
      const line = $("model-route");
      const seq = ++this.routeSeq;
      if (!chat || !me.claude) { line.hidden = true; return; }
      const r = await claude.route(chat.backend, chat.session, provider, model);
      if (chat !== view || seq !== this.routeSeq) return;
      line.hidden = !r;
      line.classList.toggle("bad", !!(r && r.bad));
      line.replaceChildren(...(r ? r.parts.filter((x) => x !== "") : []));
    },
    // askConfirm shows the question Hermes put to a model pick, with the
    // buttons that answer it, in the settings note.
    askConfirm(message, token, provider, model) {
      const box = el("span", "confirm");
      box.append(message || "This model needs a confirmation.", " ");
      const yes = button("primary", "Use it anyway", () => this.pickModel(token, [provider, model]));
      const no = button("", "Cancel", () => { this.note(""); this.fill(); });
      box.append(yes, " ", no);
      this.note("");
      $("settings-note").append(box);
    },
    async pickModel(confirm, picked) {
      const chat = view;
      if (!picked && $("model").value === connectValue) {
        this.fill();
        modelsUI.open(chat ? chat.backend : "");
        return;
      }
      const [provider, model] = picked || $("model").value.split("\n");
      if (!chat && page && model && idle.data) {
        idle.pick = { provider, model };
        this.note("The next chat uses this model.");
        pageIdle();
        return;
      }
      if (!chat || !model || this.busy) return;
      this.busy = true;
      $("model").disabled = true;
      this.note("Switching model...");
      try {
        const r = await api("POST", chat.base + "/model", { provider, model, confirm: confirm || "" });
        if (chat !== view) return;
        if (r.status === "confirm") {
          this.askConfirm(r.message, r.confirm, provider, model);
          return;
        }
        if (r.status === "deferred") this.note("The new model starts when the current reply is done.");
        else if (r.status === "unchanged") this.note("That model is already in use.");
        else {
          this.note("Model changed.");
          chat.showInfo({ model, provider });
        }
      } catch (e) {
        if (chat !== view) return;
        this.note("Could not switch the model: " + e.message, true);
      } finally {
        if (chat === view) {
          this.busy = false;
          $("model").disabled = false;
          this.fill();
        }
      }
    },
    async pickEffort() {
      const chat = view;
      const effort = $("effort").value;
      if (!chat && page && idle.data) {
        idle.effort = effort;
        this.note("The next chat uses this thinking level.");
        pageIdle();
        return;
      }
      if (!chat) return;
      try {
        await api("POST", chat.base + "/reasoning", { effort });
        if (chat !== view) return;
        chat.info.reasoning_effort = effort;
        this.note("Thinking set to " + (effortNames[effort] || effort) + ". It applies from the next reply.");
      } catch (e) {
        if (chat !== view) return;
        this.note("Could not change thinking: " + e.message, true);
        this.fill();
      }
    },
  };

  // gone is true while the welcome shows that the chat in the address no
  // longer exists.
  let gone = false;
  function showGone() {
    showWelcome();
    gone = true;
    $("welcome").classList.remove("first-run");
    $("modelbar")?.remove();
    const back = el("a", "", "Back to your chats");
    back.href = "#";
    $("welcome").replaceChildren(el("p", "", "This chat no longer exists. "), back);
  }

  function showWelcome() {
    gone = false;
    pagesView.destroy();
    memory.hide();
    usage.hide();
    scheduled.hide();
    files.close();
    $("filesbtn").hidden = true;
    quickPanel(false);
    $("qpbtn").hidden = true;
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    $("welcome").hidden = false;
    $("log").hidden = true;
    $("composer").hidden = true;
    $("exportbtn").hidden = true;
    composer.open(null);
    if (!page) checkModels(false);
    $("info").hidden = true;
    $("place").hidden = true;
    $("placenote").hidden = true;
    $("claudenote").hidden = true;
    settings.reset();
    $("title").textContent = "Talaria";
    $("subtitle").textContent = "";
    document.title = "Talaria";
    banner(notice);
    notice = "";
    if (page) {
      $("welcome").hidden = true;
      $("composer").hidden = false;
      page.shown();
      pageIdle();
    }
  }

  // mark records a step of opening a chat as a performance mark
  // ("talaria:open", "talaria:painted"), to time it in the browser's tools.
  function mark(step) {
    try {
      if (step === "open") performance.clearMarks();
      performance.mark("talaria:" + step);
    } catch (_) { /* no User Timing */ }
  }

  // loadingRows is the log of a chat whose snapshot has not come yet: a
  // few grey bars where its newest messages will be.
  function loadingRows() {
    const box = el("div", "loading");
    box.setAttribute("role", "status");
    box.setAttribute("aria-label", t("loading"));
    for (const w of ["mine", "", "", "mine", ""]) box.append(el("div", "bar" + (w ? " " + w : "")));
    return box;
  }

  function showChat(backend, session) {
    mark("open");
    pagesView.destroy();
    memory.hide();
    usage.hide();
    scheduled.hide();
    files.close();
    $("filesbtn").hidden = true;
    quickPanel(false);
    $("qpbtn").hidden = true;
    closeStream();
    attachments.clear();
    current = { b: backend, s: session };
    $("welcome").hidden = true;
    $("modelbar")?.remove();
    $("log").hidden = false;
    $("composer").hidden = false;
    // The chosen chat's title and a loading state until its snapshot
    // comes, never the empty New chat state.
    const listed = lastList && lastList.sessions.find((x) => x.backend_id === backend && x.id === session);
    setTitle(listed ? listed.title : "");
    if (!listed) {
      // The list or the chat brings the title, at once when the hub has a
      // copy; say it is loading only when neither came for a while.
      $("title").textContent = "";
      document.title = "Talaria";
      const opened = current;
      setTimeout(() => { if (current === opened && view && !view.loaded && !$("title").textContent) $("title").textContent = t("loading"); }, 2000);
    }
    $("subtitle").textContent = "";
    banner("");
    settings.reset();
    unseen = 0;
    stick.reset();
    view = new Chat(backend, session);
    view.openedAt = Date.now();
    view.log.append(loadingRows());
    msgActs.reset();
    composer.open(view);
    composer.setRunning(false);
    $("exportbtn").hidden = false;
    activity.show(view);
    $("place").hidden = true;
    $("placenote").hidden = true;
    $("claudenote").hidden = true;
    $("input").placeholder = "Message Talaria";
    drawInfo({});
    $("info").hidden = false;
    const known = places.get(placeKey(backend, session));
    if (known) showPlace(known);
    loadPlace();
    const chat = view;
    let first = true;
    // fp names the transcript this page shows, while nothing has changed it
    // since the snapshot: a reconnect to a replica that cannot resume from
    // the event id then gets "snapshot.same" instead of the whole chat.
    let fp = "";
    const follow = window.HubStream(view.base + "/events", {
      query: () => (fp ? "fp=" + encodeURIComponent(fp) : ""),
      message: (m) => {
        if (first) mark("event");
        let e;
        try { e = JSON.parse(m.data); } catch (_) { return; }
        if (e.kind === "hub.reconnect") { follow.moving(); return; }
        // The hub's copy of a chat it is still fetching: a page that shows
        // the chat already would swap it for an older one.
        if (e.provisional && chat.loaded) return;
        if (e.kind === "snapshot.same") return;
        if (e.kind === "snapshot") fp = e.fp || "";
        else if (e.kind !== "snapshot.older" && e.kind !== "backend.status") fp = "";
        if (first) mark("parsed");
        chat.event(e.kind, e.data, e.at);
        if (first) {
          first = false;
          mark("handled");
          requestAnimationFrame(() => setTimeout(() => mark("painted"), 0));
        }
      },
      lost: () => banner("Connection lost. Reconnecting.", false, "stream"),
      back: () => clearBanner("stream"),
      healthy: () => updates.check(),
      failed: async () => {
        try { await api("GET", "/api/me"); } catch (_) { return; }
        banner("Could not open this chat. Reconnecting.", true, "stream");
      },
    });
    stream = follow;
    focusInput();
    if (page) {
      page.remember(backend, session);
      page.shown();
    }
  }

  // showMemory opens the Memory page in place of the chat.
  function showMemory(rest) {
    pagesView.destroy();
    usage.hide();
    scheduled.hide();
    files.close();
    $("filesbtn").hidden = true;
    quickPanel(false);
    $("qpbtn").hidden = true;
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    composer.open(null);
    for (const id of ["welcome", "log", "composer", "exportbtn", "info", "place", "placenote", "claudenote"]) $(id).hidden = true;
    settings.reset();
    $("title").textContent = "Memory";
    $("subtitle").textContent = "";
    document.title = "Memory - Talaria";
    banner("");
    memory.show(rest);
  }

  // showUsage opens the Usage page in place of the chat.
  function showUsage(rest) {
    pagesView.destroy();
    memory.hide();
    scheduled.hide();
    files.close();
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    composer.open(null);
    for (const id of ["welcome", "log", "composer", "exportbtn", "info", "place", "placenote", "claudenote", "filesbtn"]) $(id).hidden = true;
    settings.reset();
    $("title").textContent = "Usage";
    $("subtitle").textContent = "";
    document.title = "Usage - Talaria";
    banner("");
    usage.show(rest);
  }

  // showScheduled opens the Scheduled page in place of the chat.
  function showScheduled() {
    pagesView.destroy();
    memory.hide();
    usage.hide();
    files.close();
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    composer.open(null);
    for (const id of ["welcome", "log", "composer", "exportbtn", "info", "place", "placenote", "claudenote", "filesbtn"]) $(id).hidden = true;
    settings.reset();
    $("title").textContent = "Scheduled";
    $("subtitle").textContent = "";
    document.title = "Scheduled - Talaria";
    banner("");
    scheduled.show();
  }

  // route shows what the hash names. A page opens over its chat, and going
  // between a chat and its page keeps the chat as it is.
  let lastHash = "";
  function route() {
    const prev = lastHash;
    lastHash = location.hash;
    const m = location.hash.match(/^#\/s\/([^/]+)\/(.+)$/);
    const mem = location.hash.match(/^#\/memory(?:\/(.*))?$/);
    const pg = location.hash.match(/^#\/page\/([^/]+)\/([^/]+)\/(.+)$/);
    const fl = location.hash.match(/^#\/files\/([^/]+)\/([^/]+)$/);
    const us = location.hash.match(/^#\/usage(?:\/(.*))?$/);
    const sc = /^#\/scheduled\/?$/.test(location.hash);
    const dec = (x) => { try { return decodeURIComponent(x); } catch (_) { return null; } };
    const same = (b, s) => view && view.backend === b && view.session === s;
    if (page) {
      const b = m && dec(m[1]), s = m && dec(m[2]);
      if (b !== null && s !== null && m) { if (!same(b, s)) showChat(b, s); } else showWelcome();
      refreshList();
      return;
    }
    if (fl && me.files && dec(fl[1]) !== null && dec(fl[2]) !== null) {
      const b = dec(fl[1]), s = dec(fl[2]);
      pagesView.hide();
      if (!same(b, s)) showChat(b, s);
      files.open(view, $("title").textContent);
    } else if (pg && dec(pg[1]) !== null && dec(pg[2]) !== null && dec(pg[3]) !== null) {
      files.hide();
      const b = dec(pg[1]), s = dec(pg[2]);
      if (!same(b, s)) showChat(b, s);
      pagesView.open(view, dec(pg[3]));
    } else {
      pagesView.openedHere = false;
      pagesView.hide();
      files.hide();
      const under = m && /^#\/(page|files)\//.test(prev) && same(decodeURIComponent(m[1]), decodeURIComponent(m[2]));
      if (m && !under) showChat(decodeURIComponent(m[1]), decodeURIComponent(m[2]));
      else if (!m && mem && me.memory) showMemory(mem[1] || "");
      else if (!m && us && me.usage) showUsage(us[1] || "");
      else if (!m && sc && me.scheduled) showScheduled();
      else if (!m) showWelcome();
    }
    menuOpen = false;
    drawList();
    refreshList();
  }

  async function send(ev, extra) {
    ev.preventDefault();
    const input = $("input");
    const text = input.value.trim();
    if ((!text && !attachments.list.length) || !view) return;
    const chat = view;
    const bgCmd = text.match(/^\/(?:bg|background)(?:\s+([\s\S]*))?$/i);
    if (bgCmd && !attachments.list.length) {
      const prompt = (bgCmd[1] || "").trim();
      if (!prompt) { banner("Type the prompt after /bg, such as /bg summarize today's Renovate PRs.", false); return; }
      input.value = "";
      grow();
      composer.sent(chat);
      try {
        await activity.background(prompt);
      } catch (e) {
        if (chat === view) { input.value = text; grow(); }
        banner("Could not start the background prompt: " + e.message, true);
      }
      return;
    }
    await deliver(chat, text, attachments.take(), extra);
  }

  // deliver sends text to chat as the user's message, as the message box
  // does, so it steers a running turn or queues. A page's text (page set)
  // leaves the message box and its files alone and throws when it fails.
  async function deliver(chat, text, files, extra, page) {
    const input = $("input");
    const steer = !!chat.running;
    const cid = window.HubSend.newID();
    const shown = files.map((a) => ({ name: a.file.name || "attachment", image: a.image,
      url: a.url ? URL.createObjectURL(a.file) : "", pdf: a.pdf, pages: a.pages }));
    const bubble = chat.userText(text, true, files.length ? shown : undefined);
    const mine = chat.pending.find((x) => x.bubble === bubble);
    if (mine) {
      mine.files = files.map((a) => a.file);
      mine.atts = files.length ? shown : undefined;
      mine.cid = cid;
      mine.id = cid;
      mine.inflight = true;
    }
    const item = () => chat.pending.find((x) => x.cid === cid);
    chat.bottom();
    if (!page) {
      input.value = "";
      grow();
    }
    let body = steer ? { text, steer: true, id: cid } : { text, id: cid };
    if (extra) Object.assign(body, extra);
    if (files.length) {
      body = new FormData();
      body.append("id", cid);
      body.append("text", text);
      if (steer) body.append("steer", "true");
      for (const a of files) {
        body.append("file", a.file, a.file.name || "attachment");
        if (a.pdf && a.range) body.append("pages", a.range);
      }
      sending++;
      composer.update();
    }
    try {
      const r = await window.HubSend.post(() => api("POST", chat.base + "/prompt", body), {
        retry: () => {
          const p = item();
          if (!p || p.label) return;
          chat.label(p, el("div", "queued", "Not sent yet. Trying again."));
          p.retrying = true;
        },
      });
      const p = item();
      if (p) {
        p.inflight = false;
        if (p.retrying && p.label) {
          p.label.remove();
          p.label = null;
        }
        p.retrying = false;
      }
      if (r && r.status === "held" && p) {
        if (!p.sent) chat.held(p, r.id);
      } else {
        if (p && r) p.id = r.id;
        if (r && r.status === "command" && p) {
          chat.pending = chat.pending.filter((x) => x !== p);
          p.bubble.classList.remove("pending");
          if (p.label) p.label.remove();
          if (r.output) chat.add(el("div", "msg result", r.output));
        } else if (r && r.steered && p) {
          chat.steered(p);
          toast("Sent; Talaria reads it after its current step");
        } else if (p) {
          chat.queued(p.bubble);
          if (steer) toast("Queued: Talaria reads it when the current turn ends");
        }
      }
      composer.sent(chat);
      if (r && r.notice && chat === view) chat.add(el("div", "note", r.notice));
      for (const a of files) if (a.url) URL.revokeObjectURL(a.url);
    } catch (e) {
      const p = item();
      if (p) {
        p.bubble.remove();
        if (p.label) p.label.remove();
        chat.pending = chat.pending.filter((x) => x !== p);
      }
      for (const a of shown) if (a.url) URL.revokeObjectURL(a.url);
      if (page) throw e;
      if (chat === view) {
        input.value = text;
        attachments.restore(files);
      } else {
        for (const a of files) if (a.url) URL.revokeObjectURL(a.url);
      }
      banner("Could not send: " + e.message + (e.mayBeSent ? ". It may have reached Talaria: look at the chat before you send it again." : ""), true);
    } finally {
      if (files.length) sending--;
      composer.update();
    }
  }
  let sending = 0;

  // pageSend sends from the page chat: it starts the page chat on the
  // first message, then sends with the tabs the extension lets it read.
  let pageStarting = false;

  // applyIdlePicks sets the model and thinking level that the user picked
  // before the first message on the chat that message created. It is false
  // when it could not, and the chat then waits for the user.
  async function applyIdlePicks(chat) {
    const m = idleModel();
    const d = idle.data;
    try {
      if (m && d && idle.pick && (m.model !== d.model || m.provider !== d.provider)) {
        const r = await api("POST", chat.base + "/model", { provider: m.provider, model: m.model, confirm: "" });
        if (r.status === "confirm") {
          banner((r.message || "This model needs a confirmation.") + " Open the model settings to confirm it, then send again.", true);
          return false;
        }
        chat.showInfo({ model: m.model, provider: m.provider });
      }
      if (idle.effort) {
        await api("POST", chat.base + "/reasoning", { effort: idle.effort });
        chat.info.reasoning_effort = idle.effort;
      }
    } catch (e) {
      banner("Could not set the model for this chat: " + e.message, true);
      return false;
    }
    idle.pick = null;
    idle.effort = "";
    return true;
  }
  async function pageSend(ev) {
    ev.preventDefault();
    const input = $("input");
    const text = input.value.trim();
    if (!text || pageStarting) return;
    if (!view) {
      pageStarting = true;
      try {
        const pc = await api("POST", "/api/pagechats", { backend_id: idleBackend(), device_id: page.device() });
        history.replaceState(null, "", chatHash(pc.backend_id, pc.id));
        showChat(pc.backend_id, pc.id);
        const chat = view;
        await new Promise((resolve) => { chat.onLoad = resolve; setTimeout(resolve, 5000); });
        if (chat !== view) return;
        input.value = text;
        if (pc.browser_error) banner(pc.browser_error, false);
        refreshList();
        if (!(await applyIdlePicks(chat))) return;
      } catch (e) {
        banner("Could not start a page chat: " + e.message, true);
        return;
      } finally {
        pageStarting = false;
      }
    }
    let ctx = null;
    try {
      ctx = await page.context(view.session);
    } catch (e) {
      banner("Could not read the tabs: " + e.message, true);
      return;
    }
    await send(ev, ctx ? { page: ctx } : undefined);
  }

  function dropZone() {
    const form = $("composer");
    const hasFiles = (e) => e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");
    const main = document.querySelector(".main");
    main.addEventListener("dragover", (e) => {
      if (!hasFiles(e) || form.hidden) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
      form.classList.add("drop");
    });
    main.addEventListener("dragleave", (e) => {
      if (!main.contains(e.relatedTarget)) form.classList.remove("drop");
    });
    main.addEventListener("drop", (e) => {
      form.classList.remove("drop");
      if (!hasFiles(e) || form.hidden) return;
      e.preventDefault();
      attachments.add(Array.from(e.dataTransfer.files));
    });
    $("attach").onclick = () => $("files").click();
    $("files").onchange = () => {
      attachments.add(Array.from($("files").files));
      $("files").value = "";
    };
    $("input").addEventListener("paste", (e) => {
      const files = Array.from((e.clipboardData && e.clipboardData.files) || []);
      if (!files.length) return;
      if (e.clipboardData.types.includes("text/plain")) {
        if (files.every((f) => imageTypes.includes(f.type))) return;
      } else {
        e.preventDefault();
      }
      attachments.add(files);
    });
  }

  // sameText matches a message to an event's text, which another replica
  // may have cut short with "...".
  function sameText(text, evText) {
    if (text === evText) return true;
    return evText.endsWith("...") && text.startsWith(evText.slice(0, -3));
  }

  // resend sends a message that was not sent again, as a new message.
  function resend(chat, text, p, box) {
    if (chat !== view) return;
    box.remove();
    if (p) {
      p.bubble.remove();
      chat.failed = chat.failed.filter((x) => x !== p);
      if (p.files && p.files.length) attachments.add(p.files);
    }
    $("input").value = text;
    send({ preventDefault() {} });
  }

  const grow = () => composer.grow();

  // picker chooses a computer and a folder on it, to start a chat there or
  // to move a chat there.
  const picker = {
    async open(opts) {
      this.opts = opts || {};
      this.dlg = $("picker");
      this.dlg.onclose = () => { this.dlg.replaceChildren(); };
      this.frame(this.opts.move ? "Move to a computer" : "New chat on a computer", [el("p", "status", "Loading your computers...")]);
      if (!this.dlg.open) this.dlg.showModal();
      await loadComputers();
      this.list = computers.filter((c) => !this.opts.move || c.backend_id === this.opts.move.backend_id);
      const online = this.list.filter((c) => c.online);
      const want = this.opts.computer && this.list.find((c) => c.id === this.opts.computer.id && c.online);
      if (want) this.folders(want, "");
      else if (online.length === 1 && this.list.length === 1) this.folders(online[0], "");
      else this.computers();
    },
    close() { if (this.dlg && this.dlg.open) this.dlg.close(); },
    intro() {
      const m = this.opts.move;
      if (!m) return "Talaria runs this chat's commands and file changes on the computer, in the folder you pick.";
      const p = this.opts.place;
      if (p && p.kind === "computer") {
        return "Talaria will run this chat's commands and file changes on the computer and in the folder you pick, " +
          "instead of " + (p.mine ? p.name : "the computer it uses now") + (p.dir ? ", " + p.dir : "") + ".";
      }
      return "Talaria will run this chat's commands and file changes on the computer and in the folder you pick. " +
        (p && p.kind === "server" ? "They then no longer run on the server." : "The chat then stops using the sandbox.");
    },
    frame(title, body, foot) {
      const h = el("h2", "", title);
      h.id = "picker-title";
      const b = el("div", "pbody");
      b.append(...body);
      const f = el("div", "pfoot");
      if (foot) f.append(...foot);
      f.append(button("", "Cancel", () => this.close()));
      this.dlg.replaceChildren(h, el("p", "intro", this.intro()), b, f);
    },
    computers() {
      const title = this.opts.move ? "Move to a computer" : "New chat on a computer";
      if (!this.list.length) {
        this.frame(title, [el("p", "", this.opts.move ? "None of your computers works with this chat's agent." :
          "You have no computers yet. To connect one, run talaria setup on it.")]);
        return;
      }
      const body = [el("div", "label", "Pick a computer")];
      for (const c of this.list) {
        const b = button("choice" + (c.online ? " on" : ""), "", () => this.folders(c, ""));
        b.append(el("span", "", c.name), el("span", "meta", c.online ? "Online" : "Offline"));
        b.disabled = !c.online;
        body.push(b);
      }
      if (!this.list.some((c) => c.online)) {
        body.push(el("p", "status", "All your computers are offline. Turn one on and check that talaria runs on it."));
      }
      this.frame(title, body);
    },
    // crumbs splits path into the steps from the computer root that holds it.
    crumbs(c, path) {
      const root = (c.roots || []).filter((r) => path === r || path.startsWith(r.replace(/\/$/, "") + "/"))
        .sort((x, y) => y.length - x.length)[0];
      const out = [];
      let acc = root || "/";
      out.push({ label: root ? (root === c.home ? "Home" : root) : "/", path: acc });
      for (const part of path.slice(acc.length).split("/").filter(Boolean)) {
        acc = acc.replace(/\/$/, "") + "/" + part;
        out.push({ label: part, path: acc });
      }
      return out;
    },
    async folders(c, path) {
      const title = (this.opts.move ? "Move to " : "New chat on ") + c.name;
      const pick = button("primary", this.opts.move ? "Move here" : "Start chat here");
      pick.disabled = true;
      const loading = el("p", "status", "Loading folders...");
      this.frame(title, [loading], [pick]);
      let f;
      try {
        f = await api("GET", "/api/computers/" + encodeURIComponent(c.id) + "/folders" + (path ? "?path=" + encodeURIComponent(path) : ""));
      } catch (e) {
        const body = [el("p", "err", "Could not open this folder: " + e.message)];
        if (path) body.push(button("", "Back to the start", () => this.folders(c, "")));
        if (this.list.length > 1) body.push(button("link", "Pick another computer", () => this.computers()));
        this.frame(title, body);
        return;
      }
      const body = [];
      if (this.list.length > 1) body.push(button("link", "Pick another computer", () => this.computers()));
      const recent = (c.recent || []).filter((d) => d !== f.path);
      if (recent.length && !path) {
        body.push(el("div", "label", "Recent folders"));
        const box = el("div", "recent");
        for (const d of recent) {
          const b = button("", base(d), () => this.folders(c, d));
          b.title = d;
          box.append(b);
        }
        body.push(box);
      }
      body.push(el("div", "label", "Folder"));
      const nav = el("nav", "crumbs");
      nav.setAttribute("aria-label", "Folder path");
      const steps = this.crumbs(c, f.path);
      steps.forEach((st, i) => {
        if (i) nav.append(el("span", "", "/"));
        if (i === steps.length - 1) nav.append(el("span", "here", st.label));
        else nav.append(button("", st.label, () => this.folders(c, st.path)));
      });
      body.push(nav);
      if (f.folders.length) {
        const ul = el("ul", "folders");
        for (const name of f.folders) {
          const li = el("li");
          li.append(button("", name, () => this.folders(c, f.path.replace(/\/$/, "") + "/" + name)));
          ul.append(li);
        }
        body.push(ul);
      } else {
        body.push(el("p", "none", "No folders in here."));
      }
      const chosen = el("span", "chosen", f.path);
      pick.disabled = false;
      pick.onclick = () => this.choose(c, f.path, pick, chosen);
      this.frame(title, body, [chosen, pick]);
    },
    async choose(c, dir, btn, note) {
      btn.disabled = true;
      note.textContent = this.opts.move ? "Moving..." : "Starting...";
      note.classList.remove("err");
      try {
        if (this.opts.move) {
          const s = this.opts.move;
          const p = await api("POST", "/api/sessions/" + encodeURIComponent(s.backend_id) + "/" + encodeURIComponent(s.id) + "/move",
            { device_id: c.id, dir });
          places.set(placeKey(s.backend_id, s.id), p);
          if (view && view.backend === s.backend_id && view.session === s.id) showPlace(p);
          this.close();
          drawList();
        } else {
          const s = await api("POST", "/api/sessions", { device_id: c.id, dir });
          places.set(placeKey(s.backend_id, s.id), s.place);
          this.close();
          openSide(false);
          location.hash = chatHash(s.backend_id, s.id);
        }
        loadComputers();
      } catch (e) {
        btn.disabled = false;
        note.textContent = (this.opts.move ? "Could not move the chat: " : "Could not start the chat: ") + e.message;
        note.classList.add("err");
      }
    },
  };

  // jump is the chat and message a search result opens.
  let jump = null;
  const search = window.HubSearch({ api, el, chatHash, redraw: () => drawList(), jump: (j) => {
    jump = j;
    if (view && view.backend === j.b && view.session === j.s) view.jumpTo();
  },
    closeSide: () => openSide(false) });
  const activity = window.HubActivity({ api, el, showText, drawn: () => drawSubs(), highlight: highlightBash, diff: diffView,
    page: (p) => (view ? pageCard(view.base, p) : null) });
  const claude = window.HubClaude({ api, el, me: () => me, redraw: () => { if (view) drawInfo(view.info); else if (page) pageIdle(); } });
  const claudeCode = window.HubClaudeCode({ api, el });
  const usage = window.HubUsage({ api, el, chatHash, defaultBackend: () => me.default_backend || "",
    claudePanel: (b) => (me.claude ? claude.panel(b) : Promise.resolve(null)),
    claudeCodePanel: (range) => (me.claude_code ? claudeCode.panel(range) : null) });
  const files = window.HubFiles({ api, el, back: () => {
    if (view) location.hash = chatHash(view.backend, view.session);
    else history.back();
  } });

  const scheduled = window.HubScheduled({ api, el, chatHash, defaultBackend: () => me.default_backend || "" });
  const memory = window.HubMemory({ api, el, sessions: () => lastList, chatHash, closeSide: () => openSide(false),
    defaultBackend: () => me.default_backend || "" });
  const notices = window.HubNotify({ api, el, csrf: () => csrf,
    current: () => (view && view.backend && view.session ? { backend: view.backend, session: view.session } : null) });
  const composer = window.HubComposer({ api, el, chat: () => view,
    submit: () => (page ? pageSend({ preventDefault() {} }) : send({ preventDefault() {} })),
    stop: () => stopTurn(), editLast: () => msgActs.editLast(), busy: () => sending > 0, hasFiles: () => attachments.list.length > 0,
    prompts: () => !!(me && me.prompts), managePrompts: () => prefs.open("prompts", true),
    canFiles: (chat) => {
      const pl = chat && chat.place;
      return !!me.files && !!pl && ((pl.kind === "computer" && pl.mine) || pl.kind === "sandbox");
    } });
  window.HubTimes.press(document.body, toast);
  const msgActs = window.HubMsgActions({ api, el, chat: () => view, running: () => !!(view && view.running), stop: () => stopTurn(),
    reopen: (chat) => { if (chat === view) showChat(chat.backend, chat.session); }, toast, banner,
    open: (b, s) => { openSide(false); location.hash = chatHash(b, s); } });
  const chats = window.HubChatList({ api, el, toast, banner, refresh: () => refreshList(),
    exportChat: (b, s, title) => msgActs.exportChat(b, s, title), computers: () => computers,
    newChat: () => newChat(), pickFolder: (c) => { openSide(false); picker.open({ computer: c }); }, me: () => me });

  const worksWith = window.HubWorksWith.create({ api, el, toast, banner, me: () => me,
    backends: () => (lastList ? lastList.backends : []), sessions: () => (lastList ? lastList.sessions : []),
    where: (s) => {
      const p = placeOf(s);
      if (!p) return "";
      if (p.kind === "computer") return (p.mine ? p.name : "another computer") + (p.dir ? ", " + p.dir : "");
      return p.kind === "sandbox" ? "sandbox" : "";
    },
    current: () => current, redraw: () => { if (!menuOpen || !document.querySelector("#sessions .menu")) drawList(); },
    phone: () => matchMedia("(max-width: 760px)").matches });

  const pinOrder = window.HubPinOrder({ api, el, toast, banner, pinned: pinnedChats, redraw: () => drawList(), refresh: () => refreshList(),
    nav: () => $("sessions"),
    find: (key) => (lastList ? lastList.sessions.find((s) => s.backend_id + "\n" + s.id === key) : null),
    unpin: (s) => { s.pinned = false; s.pin_pos = 0; drawList(); chats.pin(s, false); },
    onMoved: (s, byKeyboard) => { if (byKeyboard) refocus(s.backend_id + "\n" + s.id, true); } });

  const page = pageMode ? window.HubPageChat({ api, el, toast, banner, view: () => view,
    sessions: () => (lastList ? lastList.sessions : []), open: (b, s) => { location.hash = chatHash(b, s); },
    fresh: () => { history.replaceState(null, "", location.pathname); route(); }, hash: chatHash, ago,
    submit: () => pageSend({ preventDefault() {} }), idle: () => { if (!view) pageIdle(); } }) : null;

  const updates = window.HubUpdate({ api, version: () => me.version, flush: () => composer.flush(), save: saveView,
    busy: () => !!($("input").value.trim() || attachments.list.length || sending > 0 ||
      document.querySelector("dialog[open], #sessions .menu, .editbox, .lightbox") || composer.popOpen() || chats.newOpen()) });

  // saveView is what a reload for a new release keeps; restoreView puts it
  // back once the chat's history is on the page.
  let sideTop = null;
  // logAnchor is the first message in view and how far its top is below
  // the top of the conversation, counted without the action rows.
  const logItems = (log) => Array.from(log.children).filter((n) => !n.classList.contains("acts"));
  function logAnchor(log) {
    const top = log.getBoundingClientRect().top;
    const items = logItems(log);
    const i = items.findIndex((n) => n.getBoundingClientRect().bottom > top);
    return i < 0 ? null : { i, off: items[i].getBoundingClientRect().top - top };
  }

  function saveView() {
    const log = $("log");
    const input = $("input");
    const tab = document.querySelector("#settabs [aria-selected=\"true\"]");
    return {
      hash: location.hash,
      log: { top: log.scrollTop, pinned: stick.pinned(), anchor: logAnchor(log) },
      side: $("sessions").scrollTop,
      details: Array.from(log.querySelectorAll("details")).map((d) => d.open),
      act: view ? activity.view() : null,
      files: files.state(),
      settings: $("settings").open && tab ? tab.id.replace(/^settab-/, "") : "",
      focus: document.activeElement === input ? [input.selectionStart, input.selectionEnd] : null,
    };
  }

  function restoreView(chat) {
    const st = window.HubRestore.state();
    if (!st) return;
    window.HubRestore.done();
    if (lastList) $("sessions").scrollTop = st.side || 0;
    else sideTop = st.side || 0;
    if (chat) {
      const ds = chat.log.querySelectorAll("details");
      (st.details || []).forEach((open, i) => { if (ds[i]) ds[i].open = open; });
      const place = () => {
        if (!st.log || st.log.pinned) { chat.bottom(); return; }
        const a = st.log.anchor;
        const n = a && logItems(chat.log)[a.i];
        if (!n) chat.log.scrollTop = st.log.top;
        else chat.log.scrollTop += n.getBoundingClientRect().top - chat.log.getBoundingClientRect().top - a.off;
        stick.release();
      };
      place();
      requestAnimationFrame(() => requestAnimationFrame(place));
      activity.restore(st.act);
      if (st.files) files.restore(st.files);
      if (st.focus) {
        const input = $("input");
        focusInput();
        try { input.setSelectionRange(st.focus[0], st.focus[1]); } catch (_) { /* text changed */ }
      }
    }
    if (st.settings) prefs.open(st.settings);
  }

  async function stopTurn() {
    if (!view) return;
    try { await api("POST", view.base + "/interrupt"); } catch (e) { banner("Could not stop: " + e.message, true); }
  }

  const modelsUI = window.HubModels.create({ api, el, me: () => me, toast,
    backend: () => (view && view.backend) || me.default_backend || (lastList && lastList.backends && lastList.backends[0] ? lastList.backends[0].id : ""),
    connected: () => modelsChanged(), startChat: () => newChat() });
  const agentUI = window.HubAgent.create({ api, el, me: () => me, refreshMe: async () => { await refreshMe(); },
    openSettings: (k) => prefs.open(k, true) });
  const prefs = window.HubSettings({ api, el, me: () => me, computers: () => computers, notices, logout, refreshMe, models: modelsUI, agent: agentUI,
    closeSide: () => openSide(false), promptsChanged: () => composer.promptsChanged() });

  // quickPanel opens or closes the chat's panel of tools, skills,
  // connections and safety. A narrow window gets it as a sheet from the
  // bottom; Esc or a click outside closes it.
  let qpFor = "";
  function quickPanel(on) {
    const box = $("qpanel");
    if (!box) return;
    const open = !!on && !!view;
    box.hidden = !open;
    $("qpscrim").hidden = !open;
    $("qpbtn").setAttribute("aria-expanded", String(open));
    $("qpbtn").classList.toggle("on", open);
    if (!open) {
      if (qpFor) $("qpbtn").focus({ preventScroll: true });
      qpFor = "";
      return;
    }
    qpFor = view.backend + "/" + view.session;
    agentUI.quick(box, { backend: view.backend, session: view.session }, () => quickPanel(false)).then(() => {
      box.querySelector("input, button:not(.ib)")?.focus({ preventScroll: true });
    });
  }

  // refreshMe reads /api/me again: a new password gives this browser a new session and CSRF token.
  async function refreshMe() {
    me = await api("GET", "/api/me");
    csrf = me.csrf;
  }

  async function logout() {
    await notices.logout();
    try { await api("POST", "/auth/logout"); } catch (_) { /* ignore */ }
    closeStream();
    if (me.logout_url) { location.href = me.logout_url; return; }
    const page = el("main", "page");
    const a = el("a", "", "Log in again");
    a.href = "/auth/login";
    page.append(el("h1", "", "Logged out"), a);
    document.body.replaceChildren(page);
  }

  // showSearch opens the chat list and puts the cursor in its search box.
  function showSearch() {
    openSide(true);
    $("search").focus();
    $("search").select();
  }

  async function init() {
    try {
      me = await api("GET", "/api/me");
      csrf = me.csrf;
      $("who").textContent = me.display || me.user || "";
    } catch (_) { return; }
    $("menu").onclick = () => openSide(true);
    $("qpbtn").onclick = () => quickPanel($("qpanel").hidden);
    $("qpscrim").onclick = () => quickPanel(false);
    $("qpanel").addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); quickPanel(false); } });
    document.addEventListener("click", (e) => {
      const box = $("qpanel");
      // composedPath is fixed when the click starts, so a control the click
      // removed (Show more) still counts as inside the panel.
      const path = e.composedPath();
      if (!box.hidden && !path.includes(box) && !e.target.closest("#qpbtn")) quickPanel(false);
    });
    agentUI.updateBanner($("updbanner"));
    $("scrim").onclick = () => openSide(false);
    $("newchat").onclick = () => newChat();
    $("place").onclick = () => {
      if (!view || !computers.some((c) => c.backend_id === view.backend)) return;
      picker.open({ move: { backend_id: view.backend, id: view.session }, place: view.place });
    };
    $("composer").onsubmit = (e) => {
      e.preventDefault();
      if (composer.mode() === "stop") stopTurn();
      else if (page) pageSend(e);
      else send(e);
    };
    $("exportbtn").onclick = () => { if (view) msgActs.exportChat(view.backend, view.session, $("title").textContent); };
    if (!page) dropZone();
    $("search").oninput = () => { search.query($("search").value); drawList(); };
    $("search").onkeydown = (e) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      if ($("search").value) { $("search").value = ""; search.query(""); drawList(); return; }
      openSide(false);
      $("search").blur();
      if (view) focusInput();
    };
    $("usagenav").hidden = !me.usage;
    $("filesbtn").onclick = () => {
      if (view) location.hash = "#/files/" + encodeURIComponent(view.backend) + "/" + encodeURIComponent(view.session);
    };
    $("info").ontoggle = () => { if ($("info").open) settings.load(); };
    prefs.start();
    notices.start(!!me.notify);
    $("model").onchange = () => settings.pickModel();
    $("effort").onchange = () => settings.pickEffort();
    $("subagents").onchange = () => settings.pickSubagents();
    window.HubShortcuts({ search: showSearch, newChat: () => newChat(), stop: stopTurn, running: () => !!(view && view.running),
      hasChat: () => !!view, exportChat: () => $("exportbtn").click(), activity: () => $("actbtn").click(),
      closeMenus: () => {
        if (composer.popOpen()) { composer.closePop(); return true; }
        if (chats.newOpen()) { chats.closeNew(); return true; }
        if (document.querySelector("#sessions .menu")) { menuOpen = false; drawList(); return true; }
        if ($("side").classList.contains("open")) { openSide(false); return true; }
        return false;
      } });
    document.addEventListener("visibilitychange", () => { if (!document.hidden) refreshList(); });
    setInterval(() => { if (!document.hidden) refreshList(); }, 20000);
    window.onhashchange = route;
    if (me.scheduled) {
      scheduled.refresh();
      setInterval(() => { if (!document.hidden) scheduled.refresh(); }, 60000);
    }
    if (me.memory) {
      memory.refresh();
      setInterval(() => { if (!document.hidden) memory.refresh(); }, 60000);
    }
    if (me.computers) {
      await loadComputers();
      setInterval(() => { if (!document.hidden) { loadComputers(); loadPlace(); } }, 20000);
    }
    if (page) {
      page.start();
      await refreshList();
      const last = page.last();
      if (!location.hash && last && lastList && lastList.sessions.some((s) => s.backend_id === last.b && s.id === last.s && s.page)) {
        history.replaceState(null, "", chatHash(last.b, last.s));
      }
    }
    route();
    if (!view) setTimeout(() => restoreView(null), 300);
  }

  init();
})();
