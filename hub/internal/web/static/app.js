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
  // One plain set of words for everyone; details stay behind "details".
  const words = { approval: "Hermes wants to do this. Is that OK?", allow: "Yes, go ahead", deny: "No",
    args: "details", result: "what happened", running: "working on it", done: "done", failed: "did not work",
    working: "working on it", idle: "ready", title: "Hermes" };
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
    const t = Date.parse(iso);
    if (!t || t < 0) return "";
    const s = Math.max(0, (Date.now() - t) / 1000);
    if (s < 60) return "now";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }

  function closeStream() {
    if (stream) stream.close();
    stream = null;
  }

  async function showList() {
    closeStream();
    view = null;
    $("list").hidden = false;
    $("chat").hidden = true;
    $("composer").hidden = true;
    $("back").hidden = false;
    $("stop").hidden = true;
    $("title").textContent = "All chats";
    $("subtitle").textContent = "";
    $("more").hidden = true;
    $("newchat").hidden = true;
    banner("");
    const list = $("list");
    let backends, sessions;
    try {
      [backends, sessions] = await Promise.all([api("GET", "/api/backends"), api("GET", "/api/sessions")]);
    } catch (e) {
      banner("Could not load sessions: " + e.message, true);
      return;
    }
    list.replaceChildren();
    if (!backends.length) list.append(el("p", "empty", "No Hermes backends are mapped to your account."));
    for (const b of backends) {
      const sec = el("section", "backend");
      const h = el("h2");
      h.append(el("span", "grow", b.name));
      const nb = el("button", "", "New session");
      nb.onclick = async () => {
        nb.disabled = true;
        try {
          const s = await api("POST", "/api/sessions", { backend_id: b.id });
          location.hash = "#/s/" + encodeURIComponent(b.id) + "/" + encodeURIComponent(s.id);
        } catch (e) {
          banner("Could not start a session: " + e.message, true);
          nb.disabled = false;
        }
      };
      h.append(nb);
      sec.append(h);
      if (b.error) sec.append(el("p", "err", "Unreachable: " + b.error));
      const ul = el("ul", "sessions");
      const mine = sessions.filter((s) => s.backend_id === b.id);
      if (!mine.length) ul.append(el("li", "empty", b.error ? "No sessions loaded." : "No sessions yet."));
      for (const s of mine) {
        const li = el("li");
        const a = el("a");
        a.href = "#/s/" + encodeURIComponent(b.id) + "/" + encodeURIComponent(s.id);
        a.append(el("span", "dot" + (s.live ? " live" : "")), el("span", "name", s.title || s.id), el("span", "when", ago(s.updated_at)));
        a.title = s.live ? "Live" : "Stored";
        li.append(a);
        ul.append(li);
      }
      sec.append(ul);
      list.append(sec);
    }
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
      const d = document.scrollingElement;
      return d.scrollHeight - d.scrollTop - d.clientHeight < 120;
    }

    add(node) {
      const stick = this.stick();
      if (this.status && this.status.parentNode === this.log) this.log.insertBefore(node, this.status);
      else this.log.append(node);
      if (stick) window.scrollTo(0, document.scrollingElement.scrollHeight);
      return node;
    }

    follow(fn) {
      const stick = this.stick();
      fn();
      if (stick) window.scrollTo(0, document.scrollingElement.scrollHeight);
    }

    setRunning(on) {
      $("stop").hidden = !on;
      $("subtitle").textContent = on ? t("working") : t("idle");
    }

    // newTurn starts the containers for one assistant turn.
    ensureTurn() {
      if (this.turn) return this.turn;
      this.turn = { reasoning: null, text: null, segment: null };
      return this.turn;
    }

    reasoning(text) {
      const t = this.ensureTurn();
      if (!t.reasoning) {
        const d = el("details", "reasoning");
        d.open = true;
        d.append(el("summary", "", "Thinking"), el("div", "body"));
        t.reasoning = this.add(d);
        t.text = null;
      }
      this.follow(() => { t.reasoning.lastChild.textContent += text; });
    }

    assistantText(text) {
      const t = this.ensureTurn();
      if (text === null) { t.text = null; return; }
      if (!t.text) {
        t.text = this.add(el("div", "msg assistant"));
        t.raw = "";
        if (t.reasoning) t.reasoning.open = false;
      }
      t.raw += text;
      this.follow(() => render(t.text, t.raw));
    }

    userText(text, pending) {
      const m = this.add(el("div", "msg user" + (pending ? " pending" : ""), text));
      if (pending) this.pending.push(m);
      return m;
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
      const dur = typeof p.duration_s === "number" ? " in " + p.duration_s.toFixed(1) + "s" : "";
      card.querySelector(".state").textContent = (failed ? t("failed") : t("done"));
      card.title = dur.trim();
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
        const answer = async (allow) => {
          ok.disabled = no.disabled = true;
          try {
            await api("POST", this.base + "/approvals/" + encodeURIComponent(id), { allow });
          } catch (e) {
            ok.disabled = no.disabled = false;
            banner("Approval failed: " + e.message, true);
          }
        };
        ok.onclick = () => answer(true);
        no.onclick = () => answer(false);
        actions.append(ok, no);
      } else {
        actions.append(el("span", "status", "Answer this question in the session's own client."));
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
      $("title").textContent = s.title || t("title");
      this.setRunning(s.running);
      for (const m of s.messages || []) this.row(m);
      this.turn = null;
      for (const p of s.prompts || []) this.approval(p);
      window.scrollTo(0, document.scrollingElement.scrollHeight);
    }

    row(m) {
      if (m.display_kind === "hidden") return;
      const text = m.text != null ? m.text : (typeof m.content === "string" ? m.content : "");
      if (m.role === "user") { this.turn = null; this.userText(text); return; }
      if (m.role === "assistant") {
        if (m.reasoning) { this.reasoning(m.reasoning); this.turn.reasoning.open = false; }
        if (text) { this.assistantText(text); this.turn.text = null; }
        return;
      }
      if (m.role === "tool") {
        const card = this.toolCard(m.tool_call_id || ("row" + m.row_id), m.name, m.args);
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
          banner(p.connected ? "" : "Lost the connection to this backend. Reconnecting.", false);
          break;
        case "message.start":
          this.turn = null;
          this.setRunning(true);
          if (p.text) {
            const m = this.pending.shift();
            if (m) m.classList.remove("pending"); else this.userText(p.text);
          }
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
          const t = this.ensureTurn();
          if (!t.text && !t.raw && typeof p.text === "string" && p.text) this.assistantText(p.text);
          if (t.reasoning) t.reasoning.open = false;
          if (p.status && p.status !== "complete" && p.status !== "completed") {
            this.add(el("div", "note" + (p.status === "error" || p.status === "failed" ? " bad" : ""), "Turn " + p.status + (p.error ? ": " + p.error : "")));
          }
          for (const card of this.tools.values()) if (card.classList.contains("running")) this.toolDone(card, {});
          this.turn = null;
          this.setRunning(false);
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

  function showChat(backend, session) {
    closeStream();
    $("list").hidden = true;
    $("chat").hidden = false;
    $("composer").hidden = false;
    $("back").hidden = true;
    $("more").hidden = false;
    $("newchat").hidden = false;
    $("title").textContent = t("title");
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
        banner("Could not open this session. Go back and try again.", true);
      } else {
        banner("Connection lost. Reconnecting.", false);
      }
    };
  }

  const chatHash = (b, s) => "#/s/" + encodeURIComponent(b) + "/" + encodeURIComponent(s);

  // home opens the default chat: the newest session on the
  // user's default backend, or a new one.
  async function home(fresh) {
    closeStream();
    $("list").hidden = true;
    try {
      const [backends, sessions] = await Promise.all([api("GET", "/api/backends"), api("GET", "/api/sessions")]);
      const b = backends.find((x) => x.id === me.default_backend) || backends[0];
      if (!b) { banner("No assistant is set up for you yet.", true); return; }
      const mine = sessions.filter((s) => s.backend_id === b.id);
      let s = fresh ? null : (mine.find((x) => x.live) || mine[0]);
      if (!s) s = await api("POST", "/api/sessions", { backend_id: b.id });
      location.replace(chatHash(b.id, s.id));
    } catch (e) {
      banner("Could not open your chat: " + e.message, true);
    }
  }

  function route() {
    const m = location.hash.match(/^#\/s\/([^/]+)\/(.+)$/);
    if (m) showChat(decodeURIComponent(m[1]), decodeURIComponent(m[2]));
    else if (location.hash !== "#/all") home(false);
    else showList();
  }

  async function send(ev) {
    ev.preventDefault();
    const input = $("input");
    const text = input.value.trim();
    if (!text || !view) return;
    const chat = view;
    const bubble = chat.userText(text, true);
    input.value = "";
    grow();
    try {
      await api("POST", chat.base + "/prompt", { text });
    } catch (e) {
      bubble.remove();
      chat.pending = chat.pending.filter((b) => b !== bubble);
      input.value = text;
      banner("Send failed: " + e.message, true);
    }
  }

  function grow() {
    const t = $("input");
    t.style.height = "auto";
    t.style.height = Math.min(t.scrollHeight, window.innerHeight * 0.4) + "px";
  }

  async function init() {
    try {
      me = await api("GET", "/api/me");
      csrf = me.csrf;
      $("logout").title = "Logged in as " + me.user;
    } catch (_) { return; }
    $("back").onclick = () => { location.hash = "#/"; };
    $("more").onclick = () => { location.hash = "#/all"; };
    $("newchat").onclick = () => home(true);
    $("refresh").onclick = route;
    $("stop").onclick = async () => {
      if (!view) return;
      try { await api("POST", view.base + "/interrupt"); } catch (e) { banner("Stop failed: " + e.message, true); }
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
    window.onhashchange = route;
    route();
  }

  init();
})();
