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
    lost: "Hermes restarted before it finished replying to this message. Messages sent after it wait until you dismiss it." };
  const t = (k) => words[k];

  async function api(method, path, body) {
    const opts = { method, headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" };
    if (body !== undefined) {
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
    return r.status === 200 || r.status === 201 ? r.json() : null;
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
    } catch (e) {
      if (e.message !== "login required") banner("Could not load chats: " + e.message, true);
    } finally {
      listing = false;
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
    if (q && !found) nav.append(el("p", "none", "No chats match."));
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
    a.append(el("span", "name", s.title || t("newchat")), el("span", "when", ago(s.updated_at)));
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
    menu.append(rename, archive, del);
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
      this.status = null;
      this.info = {};
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
        this.follow(() => render(node, node.raw));
        return;
      }
      if (node.queued) return;
      node.queued = true;
      requestAnimationFrame(() => {
        if (!node.queued) return;
        node.queued = false;
        this.follow(() => render(node, node.raw));
      });
    }

    userText(text, pending) {
      const m = this.add(el("div", "msg user" + (pending ? " pending" : ""), text));
      if (pending) this.pending.push({ bubble: m, label: null });
      return m;
    }

    // queued marks a sent message the backend has accepted but not started.
    // With an id (from a snapshot) it also offers Cancel.
    queued(bubble, id, text) {
      const p = this.pending.find((x) => x.bubble === bubble);
      if (!p || p.label) return;
      p.label = el("div", "queued", id ? t("waiting") : t("queued"));
      if (id) {
        const c = el("button", "link", "Cancel");
        c.type = "button";
        c.onclick = () => this.inputAction(id, "cancel", c);
        p.label.append(" ", c);
      }
      bubble.after(p.label);
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

    started(text) {
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
      const preview = name === "terminal" && args && typeof args.command === "string" ? "" : context;
      if (preview) head.append(el("span", "context", preview));
      head.append(el("span", "state", t("running")));
      card.append(head);
      if (name === "terminal" && args && typeof args.command === "string") {
        card.append(el("pre", "command", args.command));
      } else if (args && Object.keys(args).length) {
        const d = el("details");
        d.append(el("summary", "", t("args")), el("pre", "", JSON.stringify(args, null, 2)));
        card.append(d);
      }
      this.tools.set(id, card);
      this.ensureTurn().text = null;
      this.add(card);
      return card;
    }

    toolDone(card, p) {
      if (!card.classList.contains("running") && card.finished) return;
      card.finished = true;
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
      for (const k of ["model", "provider", "cwd", "reasoning_effort", "context_length"]) {
        if (p[k] !== undefined && p[k] !== null) this.info[k] = p[k];
      }
      if (p.usage && typeof p.usage === "object") this.info.usage = p.usage;
      drawInfo(this.info);
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
      box.append(el("b", "", p.kind === "clarify" ? "Question" : t("approval")));
      if (p.description) box.append(el("div", "", p.description));
      if (p.command) box.append(el("pre", "", p.command));
      if (p.question) box.append(el("div", "", p.question));
      const actions = el("div", "actions");
      if (p.kind !== "clarify") {
        const ok = el("button", "primary", t("allow"));
        const no = el("button", "danger", t("deny"));
        ok.type = no.type = "button";
        const answer = async (allow) => {
          ok.disabled = no.disabled = true;
          try {
            await api("POST", this.base + "/approvals/" + encodeURIComponent(id), { allow });
          } catch (e) {
            ok.disabled = no.disabled = false;
            banner("Could not answer: " + e.message, true);
          }
        };
        ok.onclick = () => answer(true);
        no.onclick = () => answer(false);
        actions.append(ok, no);
      } else {
        actions.append(el("span", "status", "Answer this question in the app where the chat started."));
      }
      box.append(actions);
      this.approvals.set(id, box);
      this.add(box);
    }

    settle(id, text) {
      const box = this.approvals.get(id);
      if (!box) return;
      const actions = box.querySelector(".actions");
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
      this.showInfo(s.info || {});
      for (const m of s.messages || []) this.row(m);
      this.turn = null;
      const users = this.log.querySelectorAll(".msg.user");
      this.groupTools(s.running && users.length ? users[users.length - 1] : null);
      for (const p of s.pending || []) {
        if (p.status === "lost") this.lost(p.id, p.text);
        else this.queued(this.userText(p.text, true), p.id);
      }
      for (const p of s.prompts || []) this.approval(p);
      this.bottom();
    }

    row(m) {
      if (m.display_kind === "hidden") return;
      const text = m.text != null ? m.text : (typeof m.content === "string" ? m.content : "");
      if (m.role === "user") { this.turn = null; this.userText(text); return; }
      if (m.role === "assistant") {
        const r = m.reasoning || m.reasoning_content;
        if (r) { this.reasoning(r); this.turn.reasoning.open = false; }
        if (text) { this.assistantText(text); this.turn.text = null; }
        return;
      }
      if (m.role === "tool") {
        const card = this.toolCard(m.tool_call_id || ("row" + m.row_id), m.name || m.tool_name, m.args);
        this.toolDone(card, { result: m.content != null ? m.content : text });
        return;
      }
      if (text) this.add(el("div", "note", text));
    }

    event(kind, p) {
      p = p || {};
      switch (kind) {
        case "snapshot": this.snapshot(p); break;
        case "backend.status":
          banner(p.connected ? "" : "Lost the connection to Hermes. Reconnecting.", false);
          break;
        case "message.start":
          this.turn = null;
          this.setRunning(true);
          this.started(p.text);
          break;
        case "reasoning.delta": this.reasoning(p.text || ""); break;
        case "reasoning.available":
          if (!this.ensureTurn().reasoning) { this.reasoning(p.text || ""); this.turn.reasoning.open = false; }
          break;
        case "thinking.delta": this.setStatus(p.text || ""); break;
        case "status.update": if (p.text) this.setStatus(p.text); break;
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
        case "message.delta": this.setStatus(""); this.assistantText(p.text === undefined ? "" : p.text, true); break;
        case "message.interim": if (p.text && !p.already_streamed) { this.assistantText(p.text); this.turn.text = null; } break;
        case "message.complete": {
          this.setStatus("");
          const tn = this.ensureTurn();
          if (!tn.text && !tn.raw && typeof p.text === "string" && p.text) this.assistantText(p.text);
          if (tn.reasoning) tn.reasoning.open = false;
          if (p.status && p.status !== "complete" && p.status !== "completed") {
            this.add(el("div", "note" + (p.status === "error" || p.status === "failed" ? " bad" : ""), "Turn " + p.status + (p.error ? ": " + p.error : "")));
          }
          for (const card of this.tools.values()) if (card.classList.contains("running")) this.toolDone(card, {});
          this.groupTools(null);
          if (p.usage) this.showInfo({ usage: p.usage });
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
        case "approval.request": case "clarify.request": this.approval(p); break;
        case "approval.settled": case "clarify.settled": this.settle(p.prompt_id, "Answered"); break;
        case "approval.cancelled":
          for (const id of p.request_ids || []) this.settle(id, "Cancelled");
          break;
        case "error": this.add(el("div", "note bad", p.message || "Error")); break;
        default: break;
      }
    }
  }

  const render = (node, raw) => window.hubMarkdown.render(node, raw);

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

  // drawInfo fills the line under the title: model, how much of the
  // model's context the chat uses, reply speed and working folder.
  function drawInfo(info) {
    const u = info.usage || {};
    const model = info.model || u.model;
    const parts = [];
    if (model) parts.push(["model", model, "Model"]);
    const max = u.context_max || info.context_length;
    if (typeof u.context_percent === "number" && max) {
      parts.push(["context", u.context_percent + "% of " + size(max) + " memory used",
        "How much of the model's working memory this chat fills. Hermes compacts the chat when it gets full."]);
    }
    if (typeof u.avg_tps === "number" && u.avg_tps > 0) parts.push(["speed", Math.round(u.avg_tps) + " tokens/s", "Reply speed"]);
    if (info.cwd) parts.push(["cwd", info.cwd.replace(/\/+$/, "").split("/").pop() || info.cwd, "Working folder: " + info.cwd]);
    const line = $("infoline");
    line.replaceChildren();
    for (const [cls, text, title] of parts) {
      const sp = el("span", cls, text);
      sp.title = title;
      line.append(sp);
    }
    if (!parts.length) line.append(el("span", "", "Chat settings"));
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
    closeStream();
    view = null;
    current = null;
    $("welcome").hidden = false;
    $("log").hidden = true;
    $("composer").hidden = true;
    $("stop").hidden = true;
    $("info").hidden = true;
    settings.reset();
    $("title").textContent = "Hermes";
    $("subtitle").textContent = "";
    document.title = "Hermes Hub";
    banner(notice);
    notice = "";
  }

  function showChat(backend, session) {
    closeStream();
    current = { b: backend, s: session };
    $("welcome").hidden = true;
    $("log").hidden = false;
    $("composer").hidden = false;
    $("title").textContent = t("newchat");
    $("subtitle").textContent = "";
    banner("");
    settings.reset();
    view = new Chat(backend, session);
    drawInfo({});
    $("info").hidden = false;
    const es = new EventSource(view.base + "/events");
    stream = es;
    const chat = view;
    es.onmessage = (m) => {
      let e;
      try { e = JSON.parse(m.data); } catch (_) { return; }
      chat.event(e.kind, e.data);
    };
    es.onopen = () => banner("");
    es.onerror = async () => {
      if (es.readyState === EventSource.CLOSED) {
        try { await api("GET", "/api/me"); } catch (_) { return; }
        banner("Could not open this chat. Pick it again to retry.", true);
      } else {
        banner("Connection lost. Reconnecting.", false);
      }
    };
    $("input").focus({ preventScroll: true });
  }

  function route() {
    const m = location.hash.match(/^#\/s\/([^/]+)\/(.+)$/);
    if (m) showChat(decodeURIComponent(m[1]), decodeURIComponent(m[2]));
    else showWelcome();
    menuOpen = false;
    drawList();
    refreshList();
  }

  async function send(ev) {
    ev.preventDefault();
    const input = $("input");
    const text = input.value.trim();
    if (!text || !view) return;
    const chat = view;
    const bubble = chat.userText(text, true);
    chat.bottom();
    input.value = "";
    grow();
    try {
      await api("POST", chat.base + "/prompt", { text });
      chat.queued(bubble);
    } catch (e) {
      bubble.remove();
      chat.pending = chat.pending.filter((p) => p.bubble !== bubble);
      input.value = text;
      banner("Could not send: " + e.message, true);
    }
  }

  function grow() {
    const ta = $("input");
    ta.style.height = "auto";
    ta.style.height = Math.min(ta.scrollHeight, window.innerHeight * 0.4) + "px";
  }

  async function init() {
    try {
      me = await api("GET", "/api/me");
      csrf = me.csrf;
      $("who").textContent = me.display || me.user || "";
    } catch (_) { return; }
    $("menu").onclick = () => openSide(true);
    $("scrim").onclick = () => openSide(false);
    $("newchat").onclick = () => newChat();
    $("stop").onclick = async () => {
      if (!view) return;
      try { await api("POST", view.base + "/interrupt"); } catch (e) { banner("Could not stop: " + e.message, true); }
    };
    $("logout").onclick = async () => {
      try { await api("POST", "/auth/logout"); } catch (_) { /* ignore */ }
      closeStream();
      const page = el("main", "page");
      const a = el("a", "", "Log in again");
      a.href = "/auth/login";
      page.append(el("h1", "", "Logged out"), a);
      document.body.replaceChildren(page);
    };
    $("composer").onsubmit = send;
    $("search").oninput = () => drawList();
    $("info").ontoggle = () => { if ($("info").open) settings.load(); };
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
    route();
  }

  init();
})();
