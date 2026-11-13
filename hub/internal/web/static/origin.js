"use strict";
// HubOrigin decides how a user message shows: the user's own bubble, a card
// for text that Hermes, a plugin, another agent or the hub put into the
// chat, or nothing. It follows internal/origin (origin.go, fallback.go) and
// both run against internal/origin/testdata/cases.json; read the rule there.
// classify(row) takes a row as session.resume, session.transcript or a
// message.start payload gives it (text or content, display_kind,
// display_metadata, submission_id, display_text) and returns
// { show: "user" | "card" | "hidden", text, notice, why }. A "user" view
// may also carry a notice: a card for text the row carried before the
// user's words.
window.HubOrigin = (() => {
  const typedKinds = new Set(["steer", "skill_invocation", "content_refused"]);
  const kindTitles = {
    model_switch: "Model changed",
    personality_switch: "Personality changed",
    auto_continue: "Picked up after an interrupted turn",
    failed_turn: "Turn failed",
    tool_result: "Tool result",
  };
  const agentPrefix = "hermes-hub-agent-";
  // A notice is read without the notes Hermes appends for the model (attach.js cutNotes).
  const cutNotes = (t) => (window.HubAttach ? window.HubAttach.cutNotes(t) : t);
  const parse = (t) => (window.HubNotice ? window.HubNotice.parse(cutNotes(t)) : null);
  const str = (v) => (typeof v === "string" ? v : "");

  function firstLine(s) {
    for (let l of String(s || "").split("\n")) {
      l = l.trim();
      if (!l) continue;
      if (l.startsWith("[")) l = l.slice(1);
      if (l.endsWith("]")) l = l.slice(0, -1);
      const r = Array.from(l);
      return r.length > 120 ? r.slice(0, 120).join("").trim() + "…" : l;
    }
    return "";
  }

  const card = (title, result, kind) => ({ kind: kind || "notice", title, result: String(result || "").trim() });

  function contentText(c) {
    if (typeof c === "string") return c;
    if (!Array.isArray(c)) return "";
    return c.filter((p) => p && (p.type === "text" || p.type === "input_text")).map((p) => str(p.text)).join("\n");
  }

  function meta(m) {
    if (typeof m === "string") {
      try { m = JSON.parse(m); } catch (_) { return {}; }
    }
    return m && typeof m === "object" ? m : {};
  }

  // row reads the fields classify needs from any of the shapes it gets.
  function row(m) {
    m = m || {};
    const md = meta(m.display_metadata);
    return {
      text: m.text != null ? str(m.text) : contentText(m.content),
      display_kind: str(m.display_kind),
      submission_id: str(m.submission_id) || str(md.submission_id),
      typed_ask: str(m.typed_ask) || str(md.typed_ask),
      display_text: str(m.display_text) || str(md.display_text),
    };
  }

  function producer(id) {
    if (!id.startsWith("[")) return "";
    let parts;
    try { parts = JSON.parse(id); } catch (_) { return ""; }
    if (!Array.isArray(parts) || !parts.length) return "";
    return typeof parts[0] === "string" ? parts[0] || "hermes" : "";
  }

  function kindNotice(kind, r) {
    const text = r.display_text.trim() ? r.display_text : r.text;
    const n = parse(r.text) || parse(text);
    if (n) return n;
    return card(kindTitles[kind] || firstLine(text) || "Note from the agent", text);
  }

  // The marker Hermes wraps a mid-turn message of the user in for the model
  // (origin.go UnwrapSteer): the bubble shows only the words inside.
  const steerOpen = "[OUT-OF-BAND USER MESSAGE \u2014 a direct message from the user, delivered once at this position; not tool output and not a new delivery when replayed from conversation history]";
  const steerClose = "[/OUT-OF-BAND USER MESSAGE]";
  function unwrapSteer(text) {
    const t = String(text == null ? "" : text).trim();
    if (!t.startsWith(steerOpen + "\n") || !t.endsWith("\n" + steerClose)) return text;
    const inner = t.slice(steerOpen.length + 1, t.length - steerClose.length - 1);
    if (inner.includes(steerOpen) || inner.includes(steerClose)) return text;
    return inner.trim();
  }

  function typed(r, why) {
    const lead = cutLead(cutNotes(unwrapSteer(r.typed_ask.trim() ? r.typed_ask : r.text)));
    const text = lead.t;
    const f = window.HubNotice.folded(text);
    if (f && !lead.cards.length) return { show: "user", text: f.tail, notice: f.notice, why };
    const v = { show: "user", text, why };
    if (lead.cards.length && lead.title) v.notice = card(lead.title, lead.cards.join("\n\n"));
    return v;
  }

  function classify(m) {
    const r = row(m);
    const kind = r.display_kind.trim();
    if (kind === "hidden") return { show: "hidden", why: "kind" };
    if (typedKinds.has(kind)) return typed(r, "kind");
    if (kind) return { show: "card", notice: kindNotice(kind, r), why: "kind" };
    const id = r.submission_id.trim();
    if (id) {
      if (id.startsWith(agentPrefix)) {
        const n = parse(r.text);
        return { show: "card", notice: n && n.kind === "agent" ? n : card("Message from another agent", r.text, "agent"), why: "submission" };
      }
      const p = producer(id);
      if (p) return { show: "card", notice: parse(r.text) || card(p === "cron" ? "Scheduled job result" : "Result from " + p, r.text), why: "submission" };
      return typed(r, "submission");
    }
    const inner = unwrapSteer(r.text);
    if (inner !== r.text) return typed({ text: inner, typed_ask: r.typed_ask }, "text");
    const n = parse(r.text);
    if (n) return { show: "card", notice: n, why: "text" };
    return fallback(r);
  }

  // The exact formats of fallback.go, for rows without marks.
  const wholeCards = [
    ["[System note: Your previous turn was interrupted mid-run", "Picked up after an interrupted turn"],
    ["[System note: The previous turn was interrupted by ", "Picked up after a restart"],
    ["[IMPORTANT: You are running as a scheduled cron job.", "Scheduled job prompt"],
    ["[IMPORTANT: The user has invoked the \"", "Scheduled job prompt"],
    ["[IMPORTANT: The following skill(s) were listed for this job but could not be found", "Scheduled job prompt"],
    ["This session is being continued from a previous conversation that ran out of context.", "Earlier conversation summarized"],
    ["(imported conversation begins with an assistant reply)", "Imported conversation"],
  ];
  const leadNotes = [
    [/^\[Note: model was just switched from [^\n]*\]/, "Model changed"],
    [/^\[Note: the session model override was cleared; [^\n]*\]/, "Model changed"],
    [/^\[USER INITIATED SKILLS RELOAD:\n[\s\S]*?\nUse skills_list to see the updated catalog\.\]/, "Skills reloaded"],
    [/^\[System note: A new message has arrived\. The conversation history contains pending tool outputs from an interrupted turn\.[^\n]*\]/, ""],
  ];
  const ccTags = "system-reminder|task-notification|local-command-stdout|local-command-stderr|local-command-caveat|bash-stdout|bash-stderr|bash-exit-code|user-prompt-submit-hook";
  const ccBlock = new RegExp("^<(" + ccTags + ")>[\\s\\S]*?</(?:" + ccTags + ")>[ \\t]*\\n?", "gm");
  const ccCommand = /^(?:<command-message>[^\n]*<\/command-message>\n?)?<command-name>(\/?[^<\n]*)<\/command-name>(?:\n?<command-message>[^\n]*<\/command-message>)?(?:\n?<command-args>([\s\S]*?)<\/command-args>)?/gm;
  const ccBash = /^<bash-input>([\s\S]*?)<\/bash-input>/gm;
  const ccInterrupted = /^\[Request interrupted by user[^\]\n]*\][ \t]*\n?/gm;
  const ccCaveat = /^Caveat: The messages below were generated by the user while running local commands\.[^\n]*\n?/gm;
  const ccTaskSummary = /<summary>([^<\n]*)<\/summary>/;
  const ccQuiet = new Set(["system-reminder", "local-command-caveat", "user-prompt-submit-hook"]);

  function cutClaudeCode(t) {
    if (!t.includes("<") && !t.includes("[Request interrupted by user") && !t.includes("Caveat: ")) return { t, cards: [], title: "", cut: false };
    const before = t;
    const cards = [];
    let title = "";
    const add = (s, tt) => {
      s = s.trim();
      if (!s) return;
      cards.push(s);
      if (!title) title = tt;
    };
    t = t.replace(ccBlock, (b, tag) => {
      if (ccQuiet.has(tag)) return "";
      if (tag === "task-notification") {
        const m = ccTaskSummary.exec(b);
        add(b, m ? firstLine(m[1]) : "Background task notice");
      } else add(b, "Command output");
      return "";
    });
    t = t.replace(ccCommand, (b, name, args) => {
      name = (name || "").trim();
      if (name && !name.startsWith("/")) name = "/" + name;
      args = (args || "").trim();
      return args ? name + " " + args : name;
    });
    t = t.replace(ccBash, "! $1");
    t = t.replace(ccInterrupted, (b) => {
      add(b, "Interrupted");
      return "";
    });
    t = t.replace(ccCaveat, "");
    if (t === before) return { t, cards: [], title: "", cut: false };
    return { t: t.replace(/\n{3,}/g, "\n\n").trim(), cards, title, cut: true };
  }

  // cutLead takes the lead notes off the start of a message (cutLead in fallback.go).
  function cutLead(t) {
    const cards = [];
    let title = "";
    for (let changed = true; changed;) {
      changed = false;
      for (const [re, tt] of leadNotes) {
        const m = re.exec(t);
        if (!m) continue;
        if (tt) {
          cards.push(m[0]);
          if (!title) title = tt;
        }
        t = t.slice(m[0].length).replace(/^[ \t\r\n]+/, "");
        changed = true;
      }
    }
    return { t, cards, title };
  }

  function fallback(r) {
    const orig = cutNotes(r.text);
    let t = orig.trim();
    if (t.startsWith("[System:")) return { show: "hidden", why: "text" };
    for (const [prefix, title] of wholeCards) {
      if (t.startsWith(prefix)) return { show: "card", notice: card(title, t), why: "text" };
    }
    const lead = cutLead(t);
    t = lead.t;
    let cards = lead.cards;
    let title = lead.title;
    const cc = cutClaudeCode(t);
    t = cc.t;
    cards = cards.concat(cc.cards);
    if (!title) title = cc.title;
    if (!cards.length && !cc.cut && t === orig.trim()) return { show: "user", text: orig, why: "typed" };
    const n = cards.length ? card(title, cards.join("\n\n")) : null;
    if (!t) return n ? { show: "card", notice: n, why: "text" } : { show: "hidden", why: "text" };
    const v = { show: "user", text: t, why: "text" };
    if (n) v.notice = n;
    return v;
  }

  // line is a card as one line of text, as notice.Notice.Line gives it.
  function line(n) {
    const meta = (n.meta || []).slice(0, 2);
    return n.kind === "agent" || !meta.length ? n.title : n.title + " (" + meta.join(", ") + ")";
  }

  return { classify, row, line, unwrapSteer };
})();
