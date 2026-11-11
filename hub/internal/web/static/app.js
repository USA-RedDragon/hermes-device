"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  };
  let csrf = "";
  let stream = null;
  let view = null;
  let me = {};
  let current = null;
  const words = { approval: "Hermes asks to run this", allow: "Allow", deny: "Deny",
    args: "Input", result: "Output", running: "Running", done: "Done", failed: "Failed",
    working: "Working", queued: "Waiting for Hermes to start", waiting: "Waiting to run", newchat: "New chat",
    lost: "Hermes restarted before it finished replying to this message. Messages sent after it wait until you dismiss it.",
    held: "Hermes is restarting. Your message will send when it is back.", notsent: "Not sent.", again: "Send again" };
  const t = (k) => words[k];
  // User rows Hermes writes itself (a cron result, a finished background job); shown as notes.
  // The first line of a message another agent sent through the hub.
  const agentHead = /^\[Agent message (am_[0-9a-f]{16}) from ([^\]\n]{1,400})\]\n/;
  const noteKinds = new Set(["internal_notification", "process_complete", "async_delegation_complete"]);

  async function api(method, path, body) {
    const opts = { method, headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" };
    if (body instanceof FormData) {
      opts.body = body;
    } else if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    const r = await fetch(path, opts);
    if (r.status === 401) {
      location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.hash);
      throw new Error("login required");
    }
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch (_) { /* not JSON */ }
      throw new Error(msg);
    }
    const json = (r.headers.get("Content-Type") || "").startsWith("application/json");
    return r.status === 200 || r.status === 201 || (r.status === 202 && json) ? r.json() : null;
  }

  function banner(text, bad) {
    const b = $("banner");
    b.hidden = !text;
    b.textContent = text || "";
    b.classList.toggle("bad", !!bad);
  }

  function ago(iso) {
    const v = Date.parse(iso);
    if (!v || v < 0) return "";
    const s = Math.max(0, (Date.now() - v) / 1000);
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }

  const chatHash = (b, s) => "#/s/" + encodeURIComponent(b) + "/" + encodeURIComponent(s);

  function openSide(open) {
    $("side").classList.toggle("open", open);
    $("scrim").hidden = !open;
  }

  // Sidebar: every chat on every backend the user may use. Empty chats are
  // hidden unless open, archived chats unless asked for. Backend headings
  // show only when there is more than one. While a chat's menu is open the
  // list is not redrawn, so a refresh cannot throw away a half-typed name.
  let listing = false;
  let notice = "";
  // unhid is a chat just unarchived. Hermes lists archived and hidden
  // chats together, so one that stays in that list was hidden, not archived.
  let unhid = "";
  let lastList = null;
  let menuOpen = false;
  let showArchived = false;
  async function refreshList() {
    if (listing) return;
    listing = true;
    try {
      const [backends, sessions] = await Promise.all([api("GET", "/api/backends"), api("GET", "/api/sessions")]);
      lastList = { backends, sessions };
      if (unhid) {
        if (sessions.some((s) => s.id === unhid && s.archived)) banner("Hermes keeps this chat hidden, so it stays with the archived chats.");
        unhid = "";
      }
      if (!menuOpen || !document.querySelector("#sessions .menu")) drawList();
      if (me.computers) loadPlaces(sessions);
    } catch (e) {
      if (e.message !== "login required") banner("Could not load chats: " + e.message, true);
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
    $("newon-row").hidden = !computers.length;
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

  function drawList() {
    if (!lastList) return;
    const { backends, sessions } = lastList;
    menuOpen = false;
    const nav = $("sessions");
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
    for (const b of backends) {
      const group = el("div", "group");
      if (many) {
        const h = el("div", "group-head");
        h.append(el("span", "grow", b.name));
        const nb = el("button", "ghost", "+");
        nb.type = "button";
        nb.title = "New chat on " + b.name;
        nb.onclick = () => newChat(b.id);
        h.append(nb);
        group.append(h);
      }
      if (b.error) group.append(el("p", "err", "Unreachable: " + b.error));
      const mine = sessions.filter((s) => s.backend_id === b.id && !s.archived && shown(s)).sort(order);
      const hits = mine.filter(match);
      found += hits.length;
      if (!mine.length && !b.error) group.append(el("p", "none", "No chats yet."));
      for (const s of hits) group.append(chatItem(s));
      nav.append(group);
    }
    if (q && !found) nav.append(el("p", "none", "No chat titles match."));
    search.draw(nav);
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

  function chatItem(s) {
    const row = el("div", "item");
    if (isCurrent(s)) row.classList.add("active");
    const a = el("a", "go");
    a.href = chatHash(s.backend_id, s.id);
    if (s.running) { const d = el("span", "dot"); d.title = "Working"; a.append(d); }
    const badge = s.activity > 0 ? el("span", "badge", String(s.activity)) : null;
    if (badge) badge.title = s.activity + " running beside this chat: subagents and background commands";
    const p = placeOf(s);
    const where = computers.length || (p && p.kind === "computer") ? placeText(p, false) : "";
    if (where) {
      const col = el("span", "col");
      col.append(el("span", "name", s.title || t("newchat")), el("span", "where" + (p.online ? "" : " off"), where));
      a.append(col);
    } else {
      a.append(el("span", "name", s.title || t("newchat")));
    }
    if (badge) a.append(badge);
    a.append(el("span", "when", ago(s.updated_at)));
    a.onclick = () => openSide(false);
    const more = el("button", "more", "\u22EF");
    more.type = "button";
    more.title = "Chat options";
    more.setAttribute("aria-label", "Options for " + (s.title || t("newchat")));
    more.setAttribute("aria-expanded", "false");
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
    const close = () => drawList();
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
    menu.append(rename, archive);
    const p = placeOf(s);
    if (computers.some((c) => c.backend_id === s.backend_id)) {
      menu.append(button("", "Move to computer...", () => { close(); picker.open({ move: s, place: p }); }));
    }
    if (p && p.kind === "computer" && p.mine && !p.inherited) {
      const rel = button("", "Stop using this computer", () => {
        const box = el("div", "confirm");
        box.append(el("p", "", "Stop using " + p.name + " for this chat? Hermes then cannot run commands or change files " +
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
    row.after(menu);
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
      banner("Could not start a chat: " + e.message, true);
    } finally {
      btn.disabled = false;
    }
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
    input.placeholder = "Message Hermes";
    if (chat.place && chat.place.kind === "computer" && !chat.place.online) {
      const name = chat.place.mine ? chat.place.name : "The computer";
      note.append(el("span", "", name + " is offline. Hermes cannot run commands or change files there until it is back, " +
        "so messages you send now will fail. Nothing waits for it."));
      input.placeholder = name + " is offline";
      note.hidden = false;
    } else if (chat.place && chat.place.kind === "sandbox" && !chat.place.online) {
      note.append(el("span", "", "The sandbox is not running right now. Hermes cannot run commands for this chat until it is back."));
      input.placeholder = "The sandbox is not running";
      note.hidden = false;
    } else if (chat.place && chat.place.kind === "none") {
      note.append(el("span", "", "This chat is not on a computer, so Hermes cannot run commands or change files for it."));
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
  class Chat {
    constructor(backend, session) {
      this.backend = backend;
      this.session = session;
      this.base = "/api/sessions/" + encodeURIComponent(backend) + "/" + encodeURIComponent(session);
      this.log = $("log");
      this.reset();
    }

    reset() {
      this.log.replaceChildren();
      this.turn = null;
      this.tools = new Map();
      this.approvals = new Map();
      this.pending = [];
      this.failed = [];
      this.status = null;
      this.info = {};
      this.compactions = undefined;
      for (const u of this.urls || []) URL.revokeObjectURL(u);
      this.urls = [];
    }

    stick() {
      const l = this.log;
      return l.scrollHeight - l.scrollTop - l.clientHeight < 120;
    }

    bottom() { this.log.scrollTop = this.log.scrollHeight; }

    add(node) {
      const stick = this.stick();
      if (this.status && this.status.parentNode === this.log) this.log.insertBefore(node, this.status);
      else this.log.append(node);
      if (stick) this.bottom();
      return node;
    }

    follow(fn) {
      const stick = this.stick();
      fn();
      if (stick) this.bottom();
    }

    setRunning(on) {
      $("stop").hidden = !on;
      $("subtitle").textContent = on ? t("working") : "";
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
        if (tn.reasoning) tn.reasoning.open = false;
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
      const pg = pageBlocks(node.raw || "", !final);
      const m = mediaDirectives(pg.text);
      render(node, final ? m.text : hidePending(m.text));
      if (!final && (pg.writing || pg.pages.length)) node.append(el("div", "note page-wait", "Writing a page"));
      if (final && pg.pages.length) {
        const row = el("div", "media");
        for (const html of pg.pages) row.append(pageCard(this.base, { html }));
        node.append(row);
      }
      if (final && m.paths.length) node.append(mediaRow(this.base, m.paths));
    }

    // endText shows the files of the reply segment that ends here.
    endText() {
      const tn = this.turn;
      if (!tn || !tn.text) return;
      tn.text.queued = false;
      this.follow(() => this.showText(tn.text, true));
    }

    // userText adds a user message. atts are the files the page sent with
    // it; without them they come from the lines Hermes keeps in the text.
    // agentText shows a message another agent sent through the hub (its
    // first line is "[Agent message am_<id> from <sender>]") with a tag
    // that names the sender, and without the header the model reads.
    // A queued one shows as waiting until it starts.
    agentText(text, waiting) {
      const m = agentHead.exec(text || "");
      if (!m) return false;
      if (!this.agentWaiting) this.agentWaiting = new Map();
      const shown = this.agentWaiting.get(m[1]);
      if (shown) {
        this.agentWaiting.delete(m[1]);
        shown.classList.remove("pending");
        if (shown.waitLabel) shown.waitLabel.remove();
        return true;
      }
      const body = text.slice(m[0].length).replace(/^[^\n]*\n\n/, "");
      const box = el("div", "msg agent" + (waiting ? " pending" : ""));
      box.title = m[1];
      box.append(el("div", "agent-tag", "Agent message from " + m[2]));
      box.append(body);
      this.add(box);
      if (waiting) {
        box.waitLabel = el("div", "queued", t("waiting"));
        box.after(box.waitLabel);
        this.agentWaiting.set(m[1], box);
      }
      return true;
    }

    userText(text, pending, atts) {
      const shown = atts ? { text, atts } : splitAttachments(text);
      const m = el("div", "msg user" + (pending ? " pending" : ""));
      if (shown.atts.length) m.append(attachmentRow(shown.atts, this.base));
      if (shown.text) m.append(shown.text);
      if (atts) for (const a of atts) if (a.url) this.urls.push(a.url);
      this.add(m);
      if (pending) this.pending.push({ bubble: m, label: null, text, id: null });
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
      this.label(p, label);
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

    started(text, note) {
      if (note) {
        if (text) this.add(el("div", "msg result", text));
        return;
      }
      if (this.agentText(text)) return;
      const p = this.pending.shift();
      if (p) {
        p.bubble.classList.remove("pending");
        if (p.label) p.label.remove();
      } else if (text) {
        this.userText(text);
      }
    }

    toolCard(id, name, args, context) {
      let card = this.tools.get(id);
      if (card) return card;
      card = el("div", "tool running");
      card.toolName = name || "tool";
      card.started = Date.now();
      const head = el("div", "head");
      head.append(el("span", "name", name || "tool"));
      if (activity.opens(name, args)) {
        card.classList.add("opens");
        head.title = "Show in Activity";
        head.onclick = () => activity.fromTool(name, args, card.result);
      }
      const preview = name === "terminal" && args && typeof args.command === "string" ? "" : context;
      if (preview) head.append(el("span", "context", preview));
      head.append(el("span", "state", t("running")));
      card.append(head);
      if (name === "terminal" && args && typeof args.command === "string") {
        card.append(el("pre", "command", args.command));
      } else if (name === "agent_send" && args && typeof args.text === "string") {
        const sent = el("div", "msg agent sent");
        sent.append(el("div", "agent-tag", "Agent message to " + String(args.to || "")), args.text);
        card.append(sent);
      } else if (args && Object.keys(args).length) {
        const d = el("details");
        d.append(el("summary", "", t("args")), el("pre", "", JSON.stringify(args, null, 2)));
        card.append(d);
      }
      this.tools.set(id, card);
      this.endText();
      this.ensureTurn().text = null;
      this.add(card);
      return card;
    }

    toolDone(card, p) {
      if (!card.classList.contains("running") && card.finished) return;
      card.finished = true;
      if (p.result !== undefined) card.result = p.result;
      card.classList.remove("running");
      const res = parseResult(p.result);
      const exit = res && typeof res.exit_code === "number" ? res.exit_code : null;
      const failed = !!(p.is_error || p.error || (res && res.error) || (exit !== null && exit !== 0));
      card.failed = failed;
      if (failed) card.classList.add("failed");
      const state = card.querySelector(".state");
      state.textContent = failed ? t("failed") : t("done");
      if (typeof p.duration_s === "number") {
        card.duration = p.duration_s;
        state.textContent += " in " + duration(p.duration_s);
      }
      for (const n of card.querySelectorAll("progress, .progress")) n.remove();
      if (card.toolName === "browser_screenshot" && !failed && res && typeof res.path === "string" && mediaPath(res.path)) {
        card.append(mediaRow(this.base, [res.path], "shot"));
      }
      if (p.inline_diff) card.append(diffView(p.inline_diff));
      let out = p.result_text;
      if (!out && res && card.toolName === "terminal" && "output" in res) {
        out = String(res.output || "");
        if (res.error) out += (out ? "\n" : "") + res.error;
        if (exit !== null && exit !== 0) out += (out ? "\n" : "") + "(exit code " + exit + ")";
      }
      if (!out && p.result !== undefined && p.result !== null) out = typeof p.result === "string" ? p.result : JSON.stringify(p.result, null, 2);
      if (out && !(p.inline_diff && !failed)) {
        const d = el("details");
        if (failed || card.toolName === "terminal") d.open = failed || out.length < 600;
        d.append(el("summary", "", t("result")), el("pre", "", out.length > 20000 ? out.slice(0, 20000) + "\n..." : out));
        card.append(d);
      }
    }

    // groupTools folds each run of finished tool cards into one line that
    // opens to show them. keep is a node after which cards stay as they are.
    groupTools(keep) {
      let run = [];
      const fold = () => {
        if (!run.length) return;
        const g = toolGroup(run);
        this.log.insertBefore(g, run[0]);
        g.lastChild.append(...run);
        run = [];
      };
      let live = false;
      for (const n of Array.from(this.log.children)) {
        if (n === keep) live = true;
        if (!live && n.classList.contains("tool") && !n.classList.contains("running")) run.push(n);
        else fold();
      }
      fold();
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

    deleted() {
      if (view !== this) return;
      notice = "This chat was deleted.";
      closeStream();
      location.hash = "";
    }

    approval(p) {
      const id = p.prompt_id;
      if (!id || this.approvals.has(id)) return;
      const box = el("div", "approval");
      const clarify = p.kind === "clarify";
      box.append(el("b", "", clarify ? "Question" : t("approval")));
      if (p.description) box.append(el("div", "", p.description));
      if (p.command) box.append(el("pre", "", p.command));
      if (p.question) box.append(el("div", "", p.question));
      const actions = el("div", "actions");
      const buttons = [];
      const answer = async (body) => {
        for (const b of buttons) b.disabled = true;
        try {
          if (body.allow !== undefined) await api("POST", this.base + "/approvals/" + encodeURIComponent(id), body);
          else await api("POST", this.base + "/prompts/" + encodeURIComponent(id) + "/answer", body);
        } catch (e) {
          for (const b of buttons) b.disabled = false;
          banner("Could not answer: " + e.message, true);
        }
      };
      const button = (cls, text, body) => {
        const b = el("button", cls, text);
        b.type = "button";
        b.onclick = () => answer(typeof body === "function" ? body() : body);
        buttons.push(b);
        return b;
      };
      const choices = Array.isArray(p.choices) ? p.choices : [];
      if (!clarify && !choices.length) {
        actions.append(button("primary", t("allow"), { allow: true }), button("danger", t("deny"), { allow: false }));
      } else if (!clarify) {
        const names = { once: t("allow"), session: "Allow in this chat", always: "Always allow", deny: t("deny") };
        for (const c of choices) actions.append(button(c === "deny" ? "danger" : c === "once" ? "primary" : "", names[c] || c, { choice: c }));
      } else {
        const picks = [];
        if (p.multi_select) {
          const list = el("div", "picks");
          for (const c of choices) {
            const row = el("label", "pick");
            const cb = el("input");
            cb.type = "checkbox";
            cb.value = c;
            picks.push(cb);
            buttons.push(cb);
            row.append(cb, el("span", "", c));
            list.append(row);
          }
          box.append(list);
          actions.append(button("primary", "Send", () => ({ picks: picks.filter((b) => b.checked).map((b) => b.value) })));
        } else {
          for (const c of choices) actions.append(button("", c, { picks: [c] }));
        }
        const input = el("input", "answer");
        input.type = "text";
        input.placeholder = choices.length ? "Or type an answer" : "Type your answer";
        input.setAttribute("aria-label", "Answer");
        buttons.push(input);
        const send = button(choices.length ? "" : "primary", "Answer", () => ({ text: input.value }));
        input.onkeydown = (e) => { if (e.key === "Enter" && input.value.trim()) { e.preventDefault(); send.click(); } };
        const typed = el("div", "typed");
        typed.append(input, send);
        box.append(typed);
        actions.append(button("ghost", "Skip", { skip: true }));
      }
      box.append(actions);
      this.approvals.set(id, box);
      this.add(box);
    }

    settle(id, text) {
      const box = this.approvals.get(id);
      if (!box) return;
      const actions = box.querySelector(".actions");
      for (const extra of box.querySelectorAll(".picks, .typed")) extra.remove();
      if (actions) actions.replaceChildren(el("span", "status", text));
    }

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

    snapshot(s) {
      this.reset();
      setTitle(s.title);
      this.setRunning(s.running);
      activity.running(s.running);
      this.showInfo(s.info || {});
      // Stored tool rows carry no arguments; the assistant message that made the call has them.
      this.calls = new Map();
      for (const m of s.messages || []) this.row(m);
      this.turn = null;
      const users = this.log.querySelectorAll(".msg.user");
      this.groupTools(s.running && users.length ? users[users.length - 1] : null);
      this.agentWaiting = new Map();
      for (const p of s.pending || []) {
        if (p.status === "lost") this.lost(p.id, p.text);
        else if (!this.agentText(p.text, true)) this.queued(this.userText(p.text, true), p.id);
      }
      for (const p of s.prompts || []) this.approval(p);
      this.bottom();
      this.jumpTo();
    }

    // jumpTo scrolls to the message a search result opened the chat at.
    jumpTo() {
      const j = jump;
      if (!j || j.b !== this.backend || j.s !== this.session) return;
      jump = null;
      const n = Number.isInteger(j.row) ? this.log.querySelector('[data-row="' + j.row + '"]') : null;
      if (!n) {
        this.add(el("div", "note", "The message you searched for is further back than this chat shows."));
        return;
      }
      n.scrollIntoView({ block: "center" });
      n.classList.add("flash");
      setTimeout(() => n.classList.remove("flash"), 2500);
    }

    row(m) {
      if (m.display_kind === "hidden") return;
      const text = m.text != null ? m.text : (typeof m.content === "string" ? m.content : "");
      if (m.role === "user" && noteKinds.has(m.display_kind)) {
        this.turn = null;
        const shown = (m.display_metadata && m.display_metadata.display_text) || text;
        if (shown) this.add(el("div", "msg result", shown));
        return;
      }
      const rid = Number.isInteger(m.row_id) ? m.row_id : m._row_id;
      if (m.role === "user") {
        this.turn = null;
        if (this.agentText(text)) return;
        const b = this.userText(text);
        if (b && Number.isInteger(rid)) b.dataset.row = rid;
        return;
      }
      if (m.role === "assistant") {
        for (const c of m.tool_calls || []) {
          const fn = c.function || {};
          let args = fn.arguments;
          if (typeof args === "string") {
            try { args = JSON.parse(args); } catch (_) { args = { arguments: args }; }
          }
          const call = { name: fn.name, args };
          if (c.id) this.calls.set(c.id, call);
          if (c.call_id) this.calls.set(c.call_id, call);
        }
        const r = m.reasoning || m.reasoning_content;
        if (r) { this.reasoning(r); this.turn.reasoning.open = false; }
        if (text) {
          this.assistantText(text);
          if (Number.isInteger(rid)) this.turn.text.dataset.row = rid;
          this.turn.text = null;
        }
        return;
      }
      if (m.role === "tool") {
        const call = (this.calls && this.calls.get(m.tool_call_id)) || {};
        const card = this.toolCard(m.tool_call_id || ("row" + m.row_id), m.name || m.tool_name || call.name, m.args || call.args);
        this.toolDone(card, { result: m.content != null ? m.content : text });
        return;
      }
      if (text) this.add(el("div", "note", text));
    }

    event(kind, p) {
      p = p || {};
      this.dispatch(kind, p);
      activity.event(kind, p);
    }

    dispatch(kind, p) {
      switch (kind) {
        case "snapshot": this.snapshot(p); break;
        case "backend.status":
          banner(p.connected ? "" : "Lost the connection to Hermes. Reconnecting.", false);
          break;
        case "message.start":
          this.turn = null;
          this.setRunning(true);
          this.started(p.text, p.note);
          break;
        case "reasoning.delta": this.reasoning(p.text || ""); break;
        case "reasoning.available":
          if (!this.ensureTurn().reasoning) { this.reasoning(p.text || ""); this.turn.reasoning.open = false; }
          break;
        case "thinking.delta": this.setStatus(p.text || ""); break;
        case "status.update":
          if (p.kind === "compacted") { this.setStatus(""); this.compacted(); }
          else if (p.kind === "compacting") this.setStatus("Summarizing older messages to make room...");
          else if (p.text) this.setStatus(p.text);
          break;
        case "tool.generating": this.setStatus("Preparing " + (p.name || "a tool") + "..."); break;
        case "tool.start": this.setStatus(""); this.toolCard(p.tool_id, p.name, p.args, p.context); break;
        case "tool.progress": {
          const card = this.toolCard(p.tool_id, p.name);
          let bar = card.querySelector("progress");
          if (!bar && p.total) { bar = el("progress"); card.append(bar); }
          if (bar && p.total) { bar.max = p.total; bar.value = p.progress; }
          let msg = card.querySelector(".progress");
          if (p.message) {
            if (!msg) { msg = el("div", "status progress"); card.append(msg); }
            msg.textContent = p.message;
          }
          break;
        }
        case "tool.complete": this.toolDone(this.toolCard(p.tool_id, p.name, p.args), p); break;
        case "message.delta": this.setStatus(""); if (typeof p.text === "string" && p.text) this.assistantText(p.text, true); break;
        case "message.interim": if (p.text && !p.already_streamed) { this.assistantText(p.text); this.turn.text = null; } break;
        case "message.complete": {
          this.setStatus("");
          const tn = this.ensureTurn();
          if (!tn.text && !tn.raw && typeof p.text === "string" && p.text) this.assistantText(p.text);
          else this.endText();
          if (tn.reasoning) tn.reasoning.open = false;
          if (p.status && p.status !== "complete" && p.status !== "completed") {
            this.add(el("div", "note" + (p.status === "error" || p.status === "failed" ? " bad" : ""), "Turn " + p.status + (p.error ? ": " + p.error : "")));
          }
          for (const card of this.tools.values()) if (card.classList.contains("running")) this.toolDone(card, {});
          this.groupTools(null);
          if (p.usage) this.showInfo({ usage: p.usage });
          claude.afterReply(this.backend, this.info.provider);
          this.turn = null;
          this.setRunning(false);
          refreshList();
          break;
        }
        case "session.info": this.showInfo(p); break;
        case "session.usage": this.showInfo({ usage: p.usage }); break;
        case "session.updated":
          if (typeof p.title === "string") setTitle(p.title);
          if (p.operation === "model" && p.model) this.showInfo({ model: p.model, provider: p.provider });
          if (p.operation === "delete") this.deleted();
          break;
        case "session.deleted": this.deleted(); break;
        case "input.held": this.heldEvent(p); break;
        case "input.sent": this.sentEvent(p); break;
        case "input.not_sent": this.notSentEvent(p); break;
        case "approval.request": case "clarify.request": this.approval(p); break;
        case "approval.settled": case "clarify.settled": this.settle(p.prompt_id, "Answered"); break;
        case "approval.outcome": this.settle(p.prompt_id, outcomeText(p)); break;
        case "approval.cancelled":
          for (const id of p.request_ids || []) this.settle(id, "Cancelled");
          break;
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

  const render = (node, raw) => window.hubMarkdown.render(node, raw);

  // MEDIA:<path> directives in a reply name files on the Hermes server or
  // the chat's computer for the user. This follows hub.MediaDirectives in the hub, which decides
  // what the media route serves; the page only shows what it finds.
  const mediaExts = new Set(("png jpg jpeg gif webp bmp tiff svg mp4 mov avi mkv webm 3gp mp3 m2a wav ogg opus m4a " +
    "flac pdf docx doc odt rtf txt md epub xlsx xls ods csv tsv json xml yaml yml kmz kml geojson gpx pptx ppt odp key " +
    "zip tar gz tgz bz2 xz 7z rar apk ipa html htm").split(" "));
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
    const close = () => { box.remove(); document.removeEventListener("keydown", onKey, true); if (prev && prev.focus) prev.focus(); };
    const onKey = (e) => { if (e.key === "Escape") { e.stopPropagation(); close(); } };
    box.addEventListener("click", (e) => { if (e.target !== open) close(); });
    document.addEventListener("keydown", onKey, true);
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
      openImage(src, name);
    });
  }

  // mediaImage is a thumbnail of an image the agent named that opens
  // full size. It turns into the file's name when the hub cannot show it.
  function mediaImage(base, p, cls) {
    const a = el("a", cls || "media-img");
    a.href = mediaURL(base, p);
    a.target = "_blank";
    a.rel = "noopener";
    a.title = baseName(p);
    const img = el("img");
    img.alt = baseName(p);
    img.loading = "lazy";
    img.src = a.href;
    img.onerror = () => a.replaceWith(mediaFile(base, p, true));
    a.append(img);
    zoomable(a, a.href, baseName(p));
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
  const pageOpen = /^ {0,3}(`{3,}|~{3,})[ \t]*(?:page|html[ \t]+page|hermes-page)[ \t]*$/i;
  const fenceStart = /^ {0,3}(`{3,}|~{3,})/;
  const pagePath = (p) => mediaPath(p) && (extOf(p) === "html" || extOf(p) === "htm");

  function fenceEnd(lines, from, marker) {
    for (let j = from; j < lines.length; j++) {
      const l = lines[j].replace(/\r$/, "").replace(/[ \t]+$/, "");
      const t = l.replace(/^ +/, "");
      if (l.length - t.length > 3 || t.length < marker.length) continue;
      if ([...t].every((c) => c === marker[0])) return j;
    }
    return -1;
  }

  // pageBlocks returns the page blocks of a reply and its text without
  // them. While a reply streams, a page block that is not closed yet is cut
  // too, and writing says so.
  function pageBlocks(text, streaming) {
    if (!text.includes("```") && !text.includes("~~~")) return { pages: [], text, writing: false };
    const lines = text.split("\n");
    const keep = [];
    const pages = [];
    let found = false;
    let writing = false;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].replace(/\r$/, "");
      const m = line.match(fenceStart);
      if (!m) { keep.push(lines[i]); continue; }
      let end = fenceEnd(lines, i + 1, m[1]);
      const page = pageOpen.test(line);
      if (page && end < 0 && streaming) { found = writing = true; break; }
      if (!page || end < 0) {
        if (end < 0) end = lines.length - 1;
        keep.push(...lines.slice(i, end + 1));
        i = end;
        continue;
      }
      const body = lines.slice(i + 1, end).join("\n");
      if (!body.trim()) {
        keep.push(...lines.slice(i, end + 1));
        i = end;
        continue;
      }
      i = end;
      found = true;
      if (!pages.includes(body)) pages.push(body);
    }
    if (!found) return { pages: [], text, writing: false };
    return { pages, text: keep.join("\n").replace(/\n{3,}/g, "\n\n").trim(), writing };
  }

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
    const name = el("span", "name", page.html !== undefined ? pageTitle(page.html) || "Page" : baseName(page.path));
    const kind = el("span", "kind", "Page");
    const open = el("button", "primary", "Open");
    open.type = "button";
    open.disabled = true;
    const text = el("span", "text");
    text.append(name, kind);
    card.append(el("span", "file", "html"), text, open);
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
  // frame left the page, so the port closes. Closing hides the frame and
  // keeps its state until another page or chat opens.
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
      box.append(bar, frame);
      const cur = { chat, ref, box, frame, port: null, blob: save.href.startsWith("blob:") ? save.href : "", status, timer: 0 };
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
      reload.onclick = () => { loads = 0; this.say(cur, ""); frame.src = src; };
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
      if (d.type === "send" && text !== null) this.toComposer(cur, text);
      else if (d.type === "copy" && text !== null) this.copy(cur, text);
      else if (d.type === "copied") this.say(cur, "Copied", 2000);
      else if (d.type === "send" || d.type === "copy") this.say(cur, "The page sent too much text.", 4000);
    },

    toComposer(cur, text) {
      if (!text.trim() || view !== cur.chat) return;
      const input = $("input");
      const had = input.value.replace(/\s+$/, "");
      input.value = had ? had + "\n\n" + text : text;
      this.back();
      grow();
      input.focus();
      banner("The page put its answer in the message box. Check it, then send it.");
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

    hide() {
      if (this.cur) this.cur.box.hidden = true;
    },

    destroy() {
      const c = this.cur;
      this.cur = null;
      if (!c) return;
      clearTimeout(c.timer);
      if (c.port) c.port.close();
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

  // Lines Hermes and the hub keep in a user message for its attachments:
  // the gateway's image hints and placeholders (agent/image_routing.py,
  // agent/image_replay.py), @image refs, inline image data, and the hub's
  // note for a file (hub.FileNote).
  const imageHint = /^\[Image attached(?: at)?: (.+)\]$/;
  const defaultCaption = "What do you see in this image?";
  const imageRef = /^@image:(?:`([^`]+)`|"([^"]+)"|'([^']+)'|(\S+))$/;
  const imageData = /^data:image\/(?:png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$/;
  const placeholder = /^\[screenshot(?: detail=\w+)?\]$/;
  const fileNote = /^\[The user attached a file: (.+)\. It is saved at (\/.*)\. Read it with your tools when the request needs its contents\.\]$/;
  // The hub's lines for a PDF it read (internal/service/pdf.go): a summary,
  // then the PDF's text between markers with a random id.
  const pdfNote = /^\[The PDF (.+) has (\d+) pages?\. .*\]$/;
  const pdfFailed = /^\[The hub could not read the PDF .+\. Its pages are not shown\.\]$/;
  const pdfStart = /^<untrusted-pdf-text id="([0-9a-f]{16})">$/;
  const baseName = (p) => p.replace(/\/+$/, "").split("/").pop() || p;

  function splitAttachments(text) {
    const atts = [];
    const keep = [];
    let skipTo = "";
    for (const line of String(text || "").split("\n")) {
      let m;
      if (skipTo) {
        if (line === skipTo) skipTo = "";
        continue;
      }
      const hubLines = atts.some((a) => a.file);
      if (hubLines && (m = line.match(pdfStart))) skipTo = "</untrusted-pdf-text id=\"" + m[1] + "\">";
      else if (hubLines && (m = line.match(pdfNote))) {
        const f = atts.find((a) => a.file && !a.pdf && a.name === m[1]);
        if (f) { f.pdf = true; f.pages = +m[2]; }
      } else if (hubLines && pdfFailed.test(line)) continue;
      else if ((m = line.match(imageHint))) atts.push({ name: baseName(m[1]), image: true, title: m[1], path: m[1] });
      else if ((m = line.match(imageRef))) {
        const p = m[1] || m[2] || m[3] || m[4];
        atts.push({ name: baseName(p), image: true, title: p, path: p });
      } else if (imageData.test(line)) atts.push({ name: "image", image: true, url: line });
      else if ((m = line.match(fileNote))) atts.push({ name: m[1], image: false, title: m[2], file: true });
      else if (!placeholder.test(line)) keep.push(line);
    }
    if (!atts.length) return { text, atts };
    let rest = keep.join("\n").trim();
    if (rest === defaultCaption && atts.some((a) => a.image)) rest = "";
    return { text: rest, atts };
  }

  // attachmentRow shows a message's attachments: a thumbnail when the page
  // has the image or the hub can fetch it from Hermes, else the file's name.
  function attachmentRow(atts, base) {
    const row = el("div", "atts");
    for (const a of atts) {
      const fetched = !a.url && base && a.path && imagePath(a.path) && mediaPath(a.path);
      if (fetched) {
        row.append(mediaImage(base, a.path, "att image"));
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
        else if (image && f.size > limits.image) why = f.name + " is larger than 25 MB, the most Hermes takes for an image.";
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
        if (a.url) {
          const img = el("img");
          img.alt = "";
          img.src = a.url;
          chip.append(img);
        } else if (a.pdf) {
          chip.append(pdfIcon());
        } else {
          chip.append(el("span", "file", "file"));
        }
        chip.append(el("span", "name", a.file.name || "attachment"),
          el("span", "size", a.pdf && a.pages ? pages(a.pages) : size(a.file.size)));
        if (a.pdf) chip.append(this.rangeBox(a));
        const x = el("button", "", "\u00d7");
        x.type = "button";
        x.title = "Remove " + (a.file.name || "attachment");
        x.setAttribute("aria-label", x.title);
        x.onclick = () => this.remove(a);
        chip.append(x);
        box.append(chip);
      }
    },
  };

  function setTitle(title) {
    $("title").textContent = title || t("newchat");
    document.title = (title || t("newchat")) + " - Hermes";
  }

  function parseResult(r) {
    if (r && typeof r === "object") return r;
    if (typeof r !== "string" || r[0] !== "{") return null;
    try { return JSON.parse(r); } catch (_) { return null; }
  }

  function duration(sec) {
    if (sec < 1) return Math.max(1, Math.round(sec * 1000)) + "ms";
    if (sec < 10) return sec.toFixed(1) + "s";
    if (sec < 60) return Math.round(sec) + "s";
    return Math.floor(sec / 60) + "m " + Math.round(sec % 60) + "s";
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
      else if (line.startsWith("+")) cls = "add";
      else if (line.startsWith("-")) cls = "del";
      else if (colored && i > 0 && !line.startsWith(" ")) cls = "file";
      pre.append(el("span", cls, line));
    });
    return pre;
  }

  const toolWords = {
    terminal: ["ran a command", "ran %d commands"],
    read_file: ["read a file", "read %d files"],
    write_file: ["edited a file", "edited %d files"],
    patch: ["edited a file", "edited %d files"],
    search_files: ["searched files", "searched files %d times"],
    web_search: ["searched the web", "searched the web %d times"],
    web_extract: ["read a web page", "read %d web pages"],
    todo: ["updated the to-do list", "updated the to-do list %d times"],
  };

  // toolGroup is the one line a run of finished tool cards folds into.
  function toolGroup(cards) {
    const counts = new Map();
    let total = 0;
    let failed = 0;
    for (const c of cards) {
      const w = toolWords[c.toolName] || ["used " + c.toolName, "used " + c.toolName + " %d times"];
      const key = w[0];
      counts.set(key, { w, n: (counts.get(key)?.n || 0) + 1 });
      if (typeof c.duration === "number") total += c.duration;
      if (c.failed) failed++;
    }
    const parts = Array.from(counts.values()).map(({ w, n }) => (n === 1 ? w[0] : w[1].replace("%d", n)));
    let text = parts.join(", ");
    text = text[0].toUpperCase() + text.slice(1);
    if (total) text += " (" + duration(total) + ")";
    const d = el("details", "toolgroup" + (failed ? " failed" : ""));
    const sum = el("summary", "", text);
    if (failed) sum.append(el("span", "bad", failed === 1 ? " 1 step failed" : " " + failed + " steps failed"));
    d.append(sum, el("div", "group-body"));
    return d;
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

  // contextPart shows how much of the model's context window the chat
  // fills, with a meter that turns the warning color when nearly full.
  function contextPart(info) {
    const u = info.usage || {};
    const max = u.context_max || info.context_length;
    if (!max) return null;
    const sp = el("span", "context");
    const used = u.context_used;
    if (typeof used !== "number") {
      sp.textContent = tokens(max) + " context";
      sp.title = "The model's context window. It shows how full it is after the next reply.";
      return sp;
    }
    const pct = Math.min(100, Math.round((used / max) * 100));
    const meter = el("span", "meter");
    meter.setAttribute("aria-hidden", "true");
    const fill = el("span");
    fill.style.width = pct + "%";
    meter.append(fill);
    sp.append(meter, (u.context_estimated ? "~" : "") + tokens(used) + " / " + tokens(max) + " context");
    if (pct >= 80) sp.classList.add("full");
    sp.title = pct + "% of the model's context window is in use" + (u.context_estimated ? " (estimated)" : "") +
      ". When it fills up, Hermes summarizes older messages to make room.";
    return sp;
  }

  // drawInfo fills the line under the title: model, how much of the
  // model's context the chat uses, reply speed and working folder.
  function drawInfo(info) {
    const u = info.usage || {};
    const model = info.model || u.model;
    const parts = [];
    if (model) parts.push(["model", model, "Model"]);
    if (view) parts.push(...claude.parts(view.backend, info));
    const ctx = contextPart(info);
    if (ctx) parts.push(ctx);
    const p = view ? view.place : null;
    let cwd = info.cwd;
    if (p) cwd = p.kind === "computer" ? p.dir : p.kind === "server" ? info.cwd : "";
    else if (p === undefined && me.computers) cwd = "";
    if (cwd) parts.push(["cwd", base(cwd), "Working folder: " + cwd + (p && p.kind === "computer" && p.mine ? " on " + p.name : "")]);
    const line = $("infoline");
    line.replaceChildren();
    for (const part of parts) {
      if (part instanceof Node) { line.append(part); continue; }
      const [cls, text, title] = part;
      const sp = el("span", cls, text);
      sp.title = title;
      line.append(sp);
    }
    if (!parts.length) line.append(el("span", "", "Chat settings"));
    if (view && !$("info").hidden) claude.note($("claudenote"), view.backend, info);
    settings.sync(info);
  }

  const effortNames = { "": "Default", none: "Off", minimal: "Minimal", low: "Low", medium: "Medium",
    high: "High", xhigh: "Extra high", max: "Max", ultra: "Ultra" };

  // settings is the model and thinking level panel that opens from the
  // line under the title. It loads the model list when first opened.
  const settings = {
    models: null,
    info: {},
    note(text, bad) {
      const n = $("settings-note");
      n.replaceChildren(text || "");
      n.classList.toggle("bad", !!bad);
    },
    reset() {
      this.busy = false;
      this.models = null;
      this.info = {};
      $("info").open = false;
      this.note("");
    },
    sync(info) {
      this.info = info;
      if ($("info").open) this.fill();
    },
    async load() {
      const chat = view;
      if (!chat) return;
      this.fill();
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
        for (const p of providers) {
          const parent = providers.length > 1 ? el("optgroup") : sel;
          if (parent !== sel) { parent.label = p.name; sel.append(parent); }
          for (const m of p.models) {
            const o = el("option", "", m.id);
            o.value = p.id + "\n" + m.id;
            if (cur.m === m) o.selected = true;
            parent.append(o);
          }
        }
        sel.disabled = !providers.length || this.busy;
      }
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
    async pickModel(confirm, picked) {
      const chat = view;
      const [provider, model] = picked || $("model").value.split("\n");
      if (!chat || !model || this.busy) return;
      this.busy = true;
      $("model").disabled = true;
      this.note("Switching model...");
      try {
        const r = await api("POST", chat.base + "/model", { provider, model, confirm: confirm || "" });
        if (chat !== view) return;
        if (r.status === "confirm") {
          const box = el("span", "confirm");
          box.append(r.message || "This model needs a confirmation.", " ");
          const yes = button("primary", "Use it anyway", () => this.pickModel(r.confirm, [provider, model]));
          const no = button("", "Cancel", () => { this.note(""); this.fill(); });
          box.append(yes, " ", no);
          this.note("");
          $("settings-note").append(box);
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

  function showWelcome() {
    pagesView.destroy();
    memory.hide();
    usage.hide();
    files.close();
    $("filesbtn").hidden = true;
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    $("welcome").hidden = false;
    $("log").hidden = true;
    $("composer").hidden = true;
    $("stop").hidden = true;
    $("info").hidden = true;
    $("place").hidden = true;
    $("placenote").hidden = true;
    $("claudenote").hidden = true;
    settings.reset();
    $("title").textContent = "Hermes";
    $("subtitle").textContent = "";
    document.title = "Hermes Hub";
    banner(notice);
    notice = "";
  }

  function showChat(backend, session) {
    pagesView.destroy();
    memory.hide();
    usage.hide();
    files.close();
    $("filesbtn").hidden = true;
    closeStream();
    attachments.clear();
    current = { b: backend, s: session };
    $("welcome").hidden = true;
    $("log").hidden = false;
    $("composer").hidden = false;
    $("title").textContent = t("newchat");
    $("subtitle").textContent = "";
    banner("");
    settings.reset();
    view = new Chat(backend, session);
    activity.show(view);
    $("place").hidden = true;
    $("placenote").hidden = true;
    $("claudenote").hidden = true;
    $("input").placeholder = "Message Hermes";
    drawInfo({});
    $("info").hidden = false;
    const known = places.get(placeKey(backend, session));
    if (known) showPlace(known);
    loadPlace();
    const es = new EventSource(view.base + "/events");
    stream = es;
    const chat = view;
    let moving = false;
    es.onmessage = (m) => {
      let e;
      try { e = JSON.parse(m.data); } catch (_) { return; }
      if (e.kind === "hub.reconnect") { moving = true; return; }
      chat.event(e.kind, e.data);
    };
    es.onopen = () => { moving = false; banner(""); };
    es.onerror = async () => {
      if (moving && es.readyState === EventSource.CONNECTING) return;
      if (es.readyState === EventSource.CLOSED) {
        try { await api("GET", "/api/me"); } catch (_) { return; }
        banner("Could not open this chat. Pick it again to retry.", true);
      } else {
        banner("Connection lost. Reconnecting.", false);
      }
    };
    $("input").focus({ preventScroll: true });
  }

  // showMemory opens the Memory page in place of the chat.
  function showMemory(rest) {
    pagesView.destroy();
    usage.hide();
    files.close();
    $("filesbtn").hidden = true;
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    for (const id of ["welcome", "log", "composer", "stop", "info", "place", "placenote", "claudenote"]) $(id).hidden = true;
    settings.reset();
    $("title").textContent = "Memory";
    $("subtitle").textContent = "";
    document.title = "Memory - Hermes Hub";
    banner("");
    memory.show(rest);
  }

  // showUsage opens the Usage page in place of the chat.
  function showUsage(rest) {
    pagesView.destroy();
    memory.hide();
    files.close();
    closeStream();
    attachments.clear();
    view = null;
    activity.show(null);
    current = null;
    for (const id of ["welcome", "log", "composer", "stop", "info", "place", "placenote", "claudenote", "filesbtn"]) $(id).hidden = true;
    settings.reset();
    $("title").textContent = "Usage";
    $("subtitle").textContent = "";
    document.title = "Usage - Hermes Hub";
    banner("");
    usage.show(rest);
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
    const dec = (x) => { try { return decodeURIComponent(x); } catch (_) { return null; } };
    const same = (b, s) => view && view.backend === b && view.session === s;
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
      else if (!m) showWelcome();
    }
    menuOpen = false;
    drawList();
    refreshList();
  }

  async function send(ev) {
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
      try {
        await activity.background(prompt);
      } catch (e) {
        if (chat === view) { input.value = text; grow(); }
        banner("Could not start the background prompt: " + e.message, true);
      }
      return;
    }
    const files = attachments.take();
    const shown = files.map((a) => ({ name: a.file.name || "attachment", image: a.image,
      url: a.url ? URL.createObjectURL(a.file) : "", pdf: a.pdf, pages: a.pages }));
    const bubble = chat.userText(text, true, files.length ? shown : undefined);
    const mine = chat.pending.find((x) => x.bubble === bubble);
    if (mine) mine.files = files.map((a) => a.file);
    chat.bottom();
    input.value = "";
    grow();
    let body = { text };
    if (files.length) {
      body = new FormData();
      body.append("text", text);
      for (const a of files) {
        body.append("file", a.file, a.file.name || "attachment");
        if (a.pdf && a.range) body.append("pages", a.range);
      }
      sending++;
      $("send").disabled = true;
    }
    try {
      const r = await api("POST", chat.base + "/prompt", body);
      const p = chat.pending.find((x) => x.bubble === bubble);
      if (r && r.status === "held" && p) {
        if (!p.sent) chat.held(p, r.id);
      } else {
        if (p && r) p.id = r.id;
        chat.queued(bubble);
      }
      if (r && r.notice && chat === view) chat.add(el("div", "note", r.notice));
      for (const a of files) if (a.url) URL.revokeObjectURL(a.url);
    } catch (e) {
      bubble.remove();
      chat.pending = chat.pending.filter((p) => p.bubble !== bubble);
      for (const a of shown) if (a.url) URL.revokeObjectURL(a.url);
      if (chat === view) {
        input.value = text;
        attachments.restore(files);
      } else {
        for (const a of files) if (a.url) URL.revokeObjectURL(a.url);
      }
      banner("Could not send: " + e.message, true);
    } finally {
      if (files.length) sending--;
      $("send").disabled = sending > 0;
    }
  }
  let sending = 0;

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

  function grow() {
    const ta = $("input");
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, window.innerHeight * 0.4) + "px";
  }

  // devices is the list of the user's computers in Settings: when each was
  // last used and from where, a note when one connected from a new
  // address, and a way to remove one.
  const devices = {
    async load() {
      const box = $("devices");
      try {
        this.draw(await api("GET", "/api/devices"));
      } catch (e) {
        box.replaceChildren(el("p", "err", "Could not load your computers: " + e.message));
      }
    },
    // closeText says how a computer's latest connection to Hermes ended.
    closeText(c) {
      const who = { device: "The computer", backend: "Hermes", hub: "The hub" }[c.by] || "Something";
      return "Disconnected " + this.when(c.at) + ". " + who + " closed the connection" +
        (c.reason ? ": " + c.reason : "") + " (code " + c.code + ").";
    },
    when(iso) {
      const v = Date.parse(iso);
      return v ? new Date(v).toLocaleString() : "";
    },
    draw(list) {
      const box = $("devices");
      box.replaceChildren();
      if (!list.length) {
        box.append(el("p", "none", "No computers are connected."));
        return;
      }
      const many = new Set(list.map((d) => d.backend_id)).size > 1;
      for (const d of list) {
        const row = el("div", "device");
        row.append(el("div", "dname", d.name));
        if (d.kind === "sandbox") {
          row.append(el("div", "dmeta", "Sandbox: where Hermes runs commands for chats that are not on one of your computers. It is set up by the owner."));
          if (many) row.append(el("div", "dmeta", "Works with " + d.backend_name));
          row.append(el("div", "dmeta", d.online === true ? "Online" : d.online === false ? "Offline" : "Could not check if it is online"));
          box.append(row);
          continue;
        }
        const meta = [];
        if (many) meta.push("Works with " + d.backend_name);
        if (!d.paired) meta.push("Waiting for the computer to finish connecting");
        else if (!d.last_seen_at) meta.push("Not used yet");
        else meta.push("Last used " + this.when(d.last_seen_at) + (d.last_addr ? " from " + d.last_addr : ""));
        if (d.hostname) meta.push("Computer name: " + d.hostname);
        for (const m of meta) row.append(el("div", "dmeta", m));
        if (d.last_close) row.append(el("div", "dnote", this.closeText(d.last_close)));
        const acts = el("div", "dacts");
        if (d.addr_changed_at) {
          row.append(el("div", "dnote", "Connected from a new address on " + this.when(d.addr_changed_at) +
            (d.prev_addr ? ". Before that it used " + d.prev_addr + "." : ".") +
            " If that was not you, remove this computer."));
          const ok = el("button", "", "That was me");
          ok.type = "button";
          ok.onclick = () => this.act(d, "dismiss-address");
          acts.append(ok);
        }
        const rm = el("button", "", "Remove");
        rm.type = "button";
        rm.onclick = () => {
          acts.replaceChildren(el("span", "dmeta", "Remove " + d.name + "? Hermes can no longer use it until you connect it again."));
          const yes = el("button", "danger", "Remove");
          yes.type = "button";
          yes.onclick = () => this.act(d, "revoke");
          const no = el("button", "", "Cancel");
          no.type = "button";
          no.onclick = () => this.load();
          acts.append(yes, no);
        };
        acts.append(rm);
        row.append(acts);
        box.append(row);
      }
    },
    async act(d, what) {
      try {
        await api("POST", "/api/devices/" + encodeURIComponent(d.id) + "/" + what);
      } catch (e) {
        banner("Could not change " + d.name + ": " + e.message, true);
      }
      this.load();
    },
  };

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
      if (online.length === 1 && this.list.length === 1) this.folders(online[0], "");
      else this.computers();
    },
    close() { if (this.dlg && this.dlg.open) this.dlg.close(); },
    intro() {
      const m = this.opts.move;
      if (!m) return "Hermes runs this chat's commands and file changes on the computer, in the folder you pick.";
      const p = this.opts.place;
      if (p && p.kind === "computer") {
        return "Hermes will run this chat's commands and file changes on the computer and in the folder you pick, " +
          "instead of " + (p.mine ? p.name : "the computer it uses now") + (p.dir ? ", " + p.dir : "") + ".";
      }
      return "Hermes will run this chat's commands and file changes on the computer and in the folder you pick. " +
        (p && p.kind === "server" ? "They then no longer run on the Hermes server." : "The chat then stops using the sandbox.");
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
        this.frame(title, [el("p", "", this.opts.move ? "None of your computers works with this chat's Hermes." :
          "You have no computers yet. To connect one, run hermes-device login on it.")]);
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
        body.push(el("p", "status", "All your computers are offline. Turn one on and check that hermes-device runs on it."));
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
  const activity = window.HubActivity({ api, el, showText });
  const claude = window.HubClaude({ api, el, me: () => me, redraw: () => { if (view) drawInfo(view.info); } });
  const usage = window.HubUsage({ api, el, chatHash, defaultBackend: () => me.default_backend || "",
    claudePanel: (b) => (me.claude ? claude.panel(b) : Promise.resolve(null)) });
  const files = window.HubFiles({ api, el, back: () => {
    if (view) location.hash = chatHash(view.backend, view.session);
    else history.back();
  } });

  const memory = window.HubMemory({ api, el, sessions: () => lastList, chatHash, closeSide: () => openSide(false),
    defaultBackend: () => me.default_backend || "" });
  const notices = window.HubNotify({ api, el, csrf: () => csrf,
    current: () => (view && view.backend && view.session ? { backend: view.backend, session: view.session } : null) });

  async function init() {
    try {
      me = await api("GET", "/api/me");
      csrf = me.csrf;
      $("who").textContent = me.display || me.user || "";
    } catch (_) { return; }
    $("menu").onclick = () => openSide(true);
    $("scrim").onclick = () => openSide(false);
    $("newchat").onclick = () => newChat();
    $("newon").onclick = () => { openSide(false); picker.open({}); };
    $("place").onclick = () => {
      if (!view || !computers.some((c) => c.backend_id === view.backend)) return;
      picker.open({ move: { backend_id: view.backend, id: view.session }, place: view.place });
    };
    $("stop").onclick = async () => {
      if (!view) return;
      try { await api("POST", view.base + "/interrupt"); } catch (e) { banner("Could not stop: " + e.message, true); }
    };
    $("logout").onclick = async () => {
      await notices.logout();
      try { await api("POST", "/auth/logout"); } catch (_) { /* ignore */ }
      closeStream();
      const page = el("main", "page");
      const a = el("a", "", "Log in again");
      a.href = "/auth/login";
      page.append(el("h1", "", "Logged out"), a);
      document.body.replaceChildren(page);
    };
    $("composer").onsubmit = send;
    dropZone();
    $("search").oninput = () => { search.query($("search").value); drawList(); };
    $("usagenav").hidden = !me.usage;
    $("filesbtn").onclick = () => {
      if (view) location.hash = "#/files/" + encodeURIComponent(view.backend) + "/" + encodeURIComponent(view.session);
    };
    $("info").ontoggle = () => { if ($("info").open) settings.load(); };
    $("prefs").hidden = !me.devices && !me.notify;
    $("devices-prefs").hidden = !me.devices;
    $("notify-prefs").hidden = !me.notify;
    $("prefs").ontoggle = () => {
      if (!$("prefs").open) return;
      if (me.devices) devices.load();
      if (me.notify) notices.load();
    };
    notices.start(!!me.notify);
    $("model").onchange = () => settings.pickModel();
    $("effort").onchange = () => settings.pickEffort();
    const input = $("input");
    input.oninput = grow;
    input.onkeydown = (e) => {
      if (e.key === "Enter" && !e.shiftKey && !matchMedia("(pointer: coarse)").matches) send(e);
    };
    document.addEventListener("keydown", (e) => { if (e.key === "Escape") openSide(false); });
    document.addEventListener("visibilitychange", () => { if (!document.hidden) refreshList(); });
    setInterval(() => { if (!document.hidden) refreshList(); }, 20000);
    window.onhashchange = route;
    if (me.memory) {
      memory.refresh();
      setInterval(() => { if (!document.hidden) memory.refresh(); }, 60000);
    }
    if (me.computers) {
      await loadComputers();
      setInterval(() => { if (!document.hidden) { loadComputers(); loadPlace(); } }, 20000);
    }
    route();
  }

  init();
})();
