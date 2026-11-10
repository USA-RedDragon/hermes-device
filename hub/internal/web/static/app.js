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
  // hidden unless open. Backend headings show only when there is more than one.
  let listing = false;
  async function refreshList() {
    if (listing) return;
    listing = true;
    try {
      const [backends, sessions] = await Promise.all([api("GET", "/api/backends"), api("GET", "/api/sessions")]);
      drawList(backends, sessions);
    } catch (e) {
      if (e.message !== "login required") banner("Could not load chats: " + e.message, true);
    } finally {
      listing = false;
    }
  }

  function drawList(backends, sessions) {
    const nav = $("sessions");
    nav.replaceChildren();
    if (!backends.length) {
      nav.append(el("p", "none", "No assistant is set up for you yet."));
      $("newchat").disabled = true;
      return;
    }
    $("newchat").disabled = false;
    const many = backends.length > 1;
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
      const mine = sessions
        .filter((s) => s.backend_id === b.id)
        .filter((s) => s.running || s.message_count > 0 || (current && current.b === b.id && current.s === s.id))
        .sort((x, y) => (y.running - x.running) || (Date.parse(y.updated_at) - Date.parse(x.updated_at)));
      if (!mine.length && !b.error) group.append(el("p", "none", "No chats yet."));
      for (const s of mine) {
        const a = el("a", "item");
        a.href = chatHash(b.id, s.id);
        if (current && current.b === b.id && current.s === s.id) a.classList.add("active");
        if (s.running) { const d = el("span", "dot"); d.title = "Working"; a.append(d); }
        a.append(el("span", "name", s.title || t("newchat")), el("span", "when", ago(s.updated_at)));
        a.onclick = () => openSide(false);
        group.append(a);
      }
      nav.append(group);
    }
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

    assistantText(text) {
      const tn = this.ensureTurn();
      if (!tn.text) {
        tn.text = this.add(el("div", "msg assistant"));
        tn.raw = "";
        if (tn.reasoning) tn.reasoning.open = false;
      }
      tn.raw += text;
      this.follow(() => render(tn.text, tn.raw));
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

    toolCard(id, name, args) {
      let card = this.tools.get(id);
      if (card) return card;
      card = el("div", "tool running");
      const head = el("div", "head");
      head.append(el("span", "name", name || "tool"), el("span", "state", t("running")));
      card.append(head);
      if (args && Object.keys(args).length) {
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
      card.classList.remove("running");
      const failed = p.error || (p.result && typeof p.result === "object" && p.result.error);
      if (failed) card.classList.add("failed");
      const state = card.querySelector(".state");
      state.textContent = failed ? t("failed") : t("done");
      if (typeof p.duration_s === "number") state.textContent += " in " + p.duration_s.toFixed(1) + "s";
      const prog = card.querySelector("progress, .progress");
      if (prog) prog.remove();
      let out = p.summary || p.result_text;
      if (!out && p.result !== undefined && p.result !== null) out = typeof p.result === "string" ? p.result : JSON.stringify(p.result, null, 2);
      if (p.inline_diff) card.append(el("pre", "", p.inline_diff));
      if (out) {
        const d = el("details");
        d.append(el("summary", "", t("result")), el("pre", "", out.length > 20000 ? out.slice(0, 20000) + "\n..." : out));
        card.append(d);
      }
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
      $("title").textContent = s.title || t("newchat");
      document.title = (s.title || t("newchat")) + " - Hermes";
      this.setRunning(s.running);
      for (const m of s.messages || []) this.row(m);
      this.turn = null;
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
        case "tool.start": this.setStatus(""); this.toolCard(p.tool_id, p.name, p.args); break;
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
        case "message.delta": this.setStatus(""); this.assistantText(p.text === undefined ? "" : p.text); break;
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
          this.turn = null;
          this.setRunning(false);
          refreshList();
          break;
        }
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

  // render shows text with fenced code blocks as <pre>; everything else stays plain.
  function render(node, raw) {
    node.replaceChildren();
    const parts = raw.split(/```[^\n]*\n?/);
    parts.forEach((part, i) => {
      if (i % 2 === 1) node.append(el("pre", "", part.replace(/\n$/, "")));
      else if (part) node.append(document.createTextNode(part));
    });
  }

  function showWelcome() {
    closeStream();
    view = null;
    current = null;
    $("welcome").hidden = false;
    $("log").hidden = true;
    $("composer").hidden = true;
    $("stop").hidden = true;
    $("title").textContent = "Hermes";
    $("subtitle").textContent = "";
    document.title = "Hermes Hub";
    banner("");
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
    view = new Chat(backend, session);
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
