"use strict";
// HubMsgActions adds the actions of each message in the open chat: Copy and
// Branch on every message, Edit on the user's last message and Retry on
// the last reply. Edit and Retry go through the hub's rewind, which drops
// the user's last turn and sends it again; Branch starts a new chat from a
// message. It also exports a chat as Markdown.
// ctx: api, el, chat(), running(), stop(), reopen(chat), toast(text),
// banner(text, bad), open(backend, session).
window.HubMsgActions = (ctx) => {
  const { api, el } = ctx;
  const $ = (id) => document.getElementById(id);
  const I = window.HubIcons;
  const log = $("log");
  const coarse = () => matchMedia("(pointer: coarse)").matches;
  const agentHead = /^\[Agent message am_[0-9a-f]{16} from [^\]\n]{1,400}\]\n/;

  const bubbles = () => Array.from(log.querySelectorAll(":scope > .msg.user:not(.pending):not(.failed), :scope > .msg.assistant.md, " +
    ":scope > .older > .part > .msg.user, :scope > .older > .part > .msg.assistant.md"));
  const isUser = (b) => b.classList.contains("user");
  const textOf = (b) => (typeof b.raw === "string" ? b.raw : b.rawText != null ? b.rawText : b.textContent);

  let editing = null;
  let retrying = null;

  function decorate() {
    if (!ctx.chat()) return;
    const all = bubbles();
    const users = all.filter(isUser);
    const lastUser = users[users.length - 1];
    const last = all[all.length - 1];
    const lastReply = last && !isUser(last) ? last : null;
    const run = ctx.running();
    for (const b of all) {
      if (b === editing) continue;
      const state = (b === lastUser ? "u" : "") + (b === lastReply && !run ? "r" : "");
      let acts = b.nextElementSibling;
      while (acts && (acts.classList.contains("queued") || acts.classList.contains("lost"))) acts = acts.nextElementSibling;
      if (!acts || !acts.classList.contains("acts") || acts.owner !== b) {
        acts = el("div", "acts" + (isUser(b) ? " mine" : ""));
        acts.owner = b;
        acts.dataset.state = "-";
        b.after(acts);
      }
      if (acts.dataset.state === state) continue;
      acts.dataset.state = state;
      acts.replaceChildren();
      const add = (name, label, fn) => {
        const x = I.button(name, label, "");
        x.hubAct = fn;
        acts.append(x);
      };
      add("copy", "Copy", () => copy(textOf(b)));
      if (state.includes("u")) add("edit", "Edit and resend (↑)", () => edit(b));
      if (state.includes("r")) add("retry", "Run the last message again", () => retry(b));
      add("branch", "Start a new chat from here", (x) => branch(b, x));
    }
    const shown = new Set(all);
    for (const a of log.querySelectorAll(":scope > .acts, :scope > .older > .part > .acts")) if (!a.owner || !a.owner.isConnected || !shown.has(a.owner)) { if (a.owner !== editing) a.remove(); }
  }

  // One listener on the log serves every message: a tap on a bubble shows
  // its actions (on a touch screen), a tap on an action runs it.
  log.addEventListener("click", (e) => {
    const t = e.target;
    if (!t || !t.closest) return;
    const act = t.closest(".acts > button");
    if (act && act.hubAct) {
      e.stopPropagation();
      act.hubAct(act);
      return;
    }
    const b = t.closest(".msg");
    if (!b || !coarse() || t.closest("a, button, details, summary, pre, img")) return;
    let acts = b.nextElementSibling;
    while (acts && (acts.classList.contains("queued") || acts.classList.contains("lost"))) acts = acts.nextElementSibling;
    if (!acts || !acts.classList.contains("acts") || acts.owner !== b) return;
    for (const x of log.querySelectorAll(".acts.show")) if (x !== acts) x.classList.remove("show");
    acts.classList.toggle("show");
  });

  let queued = false;
  const soon = () => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => { queued = false; decorate(); });
  };
  new MutationObserver(soon).observe(log, { childList: true });

  async function copy(text) {
    try {
      await navigator.clipboard.writeText(text);
      ctx.toast("Copied");
    } catch (_) {
      ctx.toast("The browser did not allow copying");
    }
  }

  // editLast opens the editor on the user's last message; false when
  // there is none.
  function editLast() {
    const users = bubbles().filter(isUser);
    const b = users[users.length - 1];
    if (!b || editing) return false;
    edit(b);
    return true;
  }

  function closeEdit() {
    if (!editing) return;
    const b = editing;
    editing = null;
    b.hidden = false;
    if (b.editBox) b.editBox.remove();
    b.editBox = null;
    soon();
  }

  function edit(b) {
    closeEdit();
    closeRetry();
    editing = b;
    const box = el("div", "editbox");
    const ta = el("textarea");
    ta.value = textOf(b);
    ta.setAttribute("aria-label", "Edit your message");
    const row = el("div", "row");
    row.append(el("span", "note", "Replaces this message and everything after it"));
    const cancel = el("button", "", "Cancel");
    cancel.type = "button";
    const go = el("button", "primary", "Resend");
    go.type = "button";
    cancel.onclick = () => closeEdit();
    go.onclick = async () => {
      const text = ta.value.trim();
      if (!text) { ta.focus(); return; }
      go.disabled = cancel.disabled = true;
      if (await rewind({ text })) closeEdit();
      else go.disabled = cancel.disabled = false;
    };
    ta.onkeydown = (e) => {
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeEdit(); $("input").focus(); }
      if (e.key === "Enter" && !e.shiftKey && !coarse()) { e.preventDefault(); go.click(); }
    };
    row.append(cancel, go);
    box.append(ta, row);
    b.hidden = true;
    const acts = b.nextElementSibling && b.nextElementSibling.classList.contains("acts") ? b.nextElementSibling : null;
    if (acts) acts.remove();
    b.after(box);
    b.editBox = box;
    ta.focus();
    ta.setSelectionRange(ta.value.length, ta.value.length);
  }

  function closeRetry() {
    if (retrying) retrying.remove();
    retrying = null;
  }

  async function retry(b) {
    closeEdit();
    if (retrying) { closeRetry(); return; }
    const chat = ctx.chat();
    const box = el("div", "editbox retrybox");
    const row = el("div", "row");
    row.append(el("span", "note", "Run your last message again on:"));
    const sel = el("select");
    sel.setAttribute("aria-label", "Model for the retry");
    const cur = (chat.info && chat.info.model) || "";
    const curP = (chat.info && chat.info.provider) || "";
    const o = el("option", "", cur || "The current model");
    o.value = "";
    sel.append(o);
    const cancel = el("button", "", "Cancel");
    cancel.type = "button";
    cancel.onclick = closeRetry;
    const go = el("button", "primary", "Retry");
    go.type = "button";
    go.onclick = async () => {
      const [provider, model] = sel.value ? sel.value.split("\n") : ["", ""];
      go.disabled = cancel.disabled = true;
      if (await rewind({ retry: true, provider, model })) closeRetry();
      else go.disabled = cancel.disabled = false;
    };
    row.append(sel, cancel, go);
    box.append(row);
    const acts = b.nextElementSibling && b.nextElementSibling.classList.contains("acts") ? b.nextElementSibling : b;
    acts.after(box);
    retrying = box;
    try {
      const m = await api("GET", chat.base + "/models");
      if (ctx.chat() !== chat) return;
      const many = (m.providers || []).length > 1;
      for (const p of m.providers || []) {
        const parent = many ? el("optgroup") : sel;
        if (many) { parent.label = p.name; sel.append(parent); }
        for (const x of p.models) {
          if (x.id === cur && (!curP || p.id === curP)) continue;
          const opt = el("option", "", x.id);
          opt.value = p.id + "\n" + x.id;
          parent.append(opt);
        }
      }
    } catch (_) { /* only the current model */ }
  }

  // rewind sends the edit or the retry. A running turn stops first, since
  // Hermes rewinds only an idle chat.
  async function rewind(body) {
    const chat = ctx.chat();
    if (!chat) return false;
    try {
      if (ctx.running()) {
        await ctx.stop();
        for (let i = 0; i < 40 && ctx.running() && ctx.chat() === chat; i++) await new Promise((r) => setTimeout(r, 250));
      }
      const r = await api("POST", chat.base + "/rewind", body);
      if (r && r.status === "confirm") {
        ctx.banner((r.message || "That model needs a confirmation.") + " Pick it in the model settings under the title first.", true);
        return false;
      }
      ctx.toast(body.retry ? "Retrying" + (body.model ? " on " + body.model : "") : "Resent");
      if (ctx.chat() === chat) ctx.reopen(chat);
      return true;
    } catch (e) {
      ctx.banner((body.retry ? "Could not retry: " : "Could not resend: ") + e.message, true);
      return false;
    }
  }

  const rowID = (m) => (Number.isInteger(m.row_id) ? m.row_id : Number.isInteger(m._row_id) ? m._row_id : null);
  const rowText = (m) => (m.text != null ? m.text : typeof m.content === "string" ? m.content : "");
  // A user row is the user's turn when HubOrigin shows it as their bubble.
  const userTurn = (m) => m.role === "user" && window.HubOrigin.classify(m).show === "user";
  const visible = (m) => m.display_kind !== "hidden" && (userTurn(m) || (m.role === "assistant" && rowText(m)));

  async function transcript(chat) {
    return api("GET", chat.base + "/transcript");
  }

  // rowOf finds the transcript row of a bubble: its own row ID, or the row
  // in the same place counted from the end.
  function rowOf(rows, b) {
    if (b.dataset.row) {
      const id = Number(b.dataset.row);
      const i = rows.findIndex((m) => rowID(m) === id);
      if (i >= 0) return i;
    }
    const all = bubbles();
    const shown = rows.map((m, i) => ({ m, i })).filter((x) => visible(x.m));
    const k = all.length - 1 - all.indexOf(b);
    const cand = shown[shown.length - 1 - k];
    const role = isUser(b) ? "user" : "assistant";
    if (cand && cand.m.role === role) return cand.i;
    const same = all.filter((x) => isUser(x) === isUser(b));
    const kk = same.length - 1 - same.indexOf(b);
    const pick = shown.filter((x) => x.m.role === role);
    return pick.length - 1 - kk >= 0 ? pick[pick.length - 1 - kk].i : -1;
  }

  async function branch(b, btn) {
    const chat = ctx.chat();
    if (!chat) return;
    btn.disabled = true;
    try {
      const t = await transcript(chat);
      const rows = (t && t.messages) || [];
      const i = rowOf(rows, b);
      if (i < 0) throw new Error("could not find this message in the chat's history");
      let through = null;
      let prefill = "";
      if (isUser(b)) {
        for (let j = i - 1; j >= 0 && through === null; j--) through = rowID(rows[j]);
        prefill = textOf(b);
        if (through === null) {
          ctx.toast("This is the first message. Copy it into a new chat instead.");
          return;
        }
      } else {
        let end = i;
        for (let j = i + 1; j < rows.length && !userTurn(rows[j]); j++) end = j;
        for (let j = end; j >= i && through === null; j--) through = rowID(rows[j]);
      }
      if (through === null) throw new Error("this message has no row ID yet");
      const r = await api("POST", chat.base + "/branch", { row_id: through });
      const base = "/api/sessions/" + encodeURIComponent(r.backend_id) + "/" + encodeURIComponent(r.session_id);
      if (prefill) await api("PUT", base + "/draft", { text: prefill }).catch(() => {});
      ctx.toast("New chat started from this message");
      ctx.open(r.backend_id, r.session_id);
    } catch (e) {
      ctx.banner("Could not branch: " + e.message, true);
    } finally {
      btn.disabled = false;
    }
  }

  // Markdown export: messages in order, tool steps folded under <details>.
  function fence(text) {
    const s = String(text);
    let f = "```";
    while (s.includes(f)) f += "`";
    return f + "\n" + s + "\n" + f;
  }

  function argsLine(args) {
    if (args == null) return "";
    if (typeof args === "string") { try { args = JSON.parse(args); } catch (_) { return args.slice(0, 200); } }
    if (typeof args.command === "string") return args.command;
    if (typeof args.path === "string") return args.path;
    const s = JSON.stringify(args);
    return s.length > 200 ? s.slice(0, 199) + "…" : s;
  }

  function markdown(title, rows) {
    const out = ["# " + (title || "Chat"), "", "_Exported from Talaria on " + new Date().toLocaleString() + "._", ""];
    const calls = new Map();
    let steps = [];
    let who = "";
    const flush = () => {
      if (!steps.length) return;
      const names = [...new Set(steps.map((s) => s.name))];
      out.push("<details>", "<summary>" + steps.length + (steps.length === 1 ? " tool step: " : " tool steps: ") +
        names.join(", ").replace(/[<>&]/g, "") + "</summary>", "");
      for (const s of steps) {
        out.push("**" + s.name + "**" + (s.args ? " `" + s.args.replace(/`/g, "'").replace(/\n/g, " ") + "`" : ""), "");
        if (s.result) out.push(fence(s.result.length > 4000 ? s.result.slice(0, 4000) + "\n…" : s.result), "");
      }
      out.push("</details>", "");
      steps = [];
    };
    const head = (h) => { if (who !== h) { out.push("## " + h, ""); who = h; } };
    for (const m of rows) {
      if (m.display_kind === "hidden") continue;
      const text = rowText(m);
      if (m.role === "user") {
        flush();
        const v = window.HubOrigin.classify(m);
        const n = v.notice;
        if (n && n.kind === "agent") {
          who = "";
          head("Agent message");
          out.push(n.result || text.replace(agentHead, ""), "");
          continue;
        }
        if (n) out.push("> " + window.HubOrigin.line(n), "");
        if (v.show !== "user") continue;
        who = "";
        head("You");
        out.push(v.text, "");
      } else if (m.role === "assistant") {
        for (const c of m.tool_calls || []) {
          const fn = c.function || {};
          const call = { name: fn.name || "tool", args: argsLine(fn.arguments) };
          if (c.id) calls.set(c.id, call);
          if (c.call_id) calls.set(c.call_id, call);
        }
        if (text) {
          flush();
          head("Talaria");
          out.push(text, "");
        }
      } else if (m.role === "tool") {
        const call = calls.get(m.tool_call_id) || {};
        head("Talaria");
        const res = m.content != null ? m.content : text;
        steps.push({ name: m.name || m.tool_name || call.name || "tool", args: call.args || "",
          result: typeof res === "string" ? res : JSON.stringify(res, null, 2) });
      }
    }
    flush();
    return out.join("\n").replace(/\n{3,}/g, "\n\n") + "\n";
  }

  async function exportChat(backend, session, title) {
    const base = "/api/sessions/" + encodeURIComponent(backend) + "/" + encodeURIComponent(session);
    try {
      const t = await api("GET", base + "/transcript");
      const name = (title || (t && t.title) || "chat").replace(/[^\w .-]+/g, "").trim().slice(0, 80) || "chat";
      const blob = new Blob([markdown(title || (t && t.title), (t && t.messages) || [])], { type: "text/markdown" });
      const a = el("a");
      a.href = URL.createObjectURL(blob);
      a.download = name + ".md";
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 10000);
      ctx.toast("Exported " + name + ".md");
    } catch (e) {
      ctx.banner("Could not export: " + e.message, true);
    }
  }

  function reset() {
    editing = null;
    retrying = null;
  }

  return { decorate: soon, editLast, exportChat, reset, markdown };
};
