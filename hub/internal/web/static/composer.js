"use strict";
// HubComposer runs the message box: the one send button (send, stop, or
// send to the running turn), the box that grows with its text, the "/"
// command, "/prompt:" saved prompt and "@" file suggestions, and the draft
// that the hub keeps for each chat so it follows the user to other devices.
// ctx: api, el, chat() (the open chat or null), canFiles(chat), submit(),
// stop(), editLast(), busy() (attachments still uploading), hasFiles(),
// prompts() (the hub keeps saved prompts), managePrompts().
window.HubComposer = (ctx) => {
  const { api, el } = ctx;
  const $ = (id) => document.getElementById(id);
  const I = window.HubIcons;
  const input = $("input");
  const pop = $("pop");
  const note = $("draftnote");
  let running = false;

  const coarse = () => matchMedia("(pointer: coarse)").matches;
  const text = () => input.value.trim();

  // mode is what the send button does now.
  function mode() {
    if (!running) return "send";
    return text() || ctx.hasFiles() ? "steer" : "stop";
  }

  const labels = {
    send: ["Send (Enter)", "Send message", "send"],
    stop: ["Stop the turn (Esc)", "Stop the turn", "stop"],
    steer: ["Send now; Talaria reads it after its current step (Enter)", "Send message to the running turn", "send"],
  };

  function update() {
    const b = $("send");
    const m = mode();
    const [title, label, ico] = labels[m];
    b.title = title;
    b.setAttribute("aria-label", label);
    I.set(b, ico);
    b.className = m === "stop" ? "ib send stopmode" : "primary ib send";
    b.disabled = m !== "stop" && ((!text() && !ctx.hasFiles()) || ctx.busy());
  }

  function setRunning(on) {
    running = !!on;
    update();
  }

  // grow sizes the box to its text. It measures on a hidden twin, never on
  // the box itself: collapsing the box to read its scrollHeight shortens
  // the composer for a moment, the log grows, and the browser clamps the
  // log's scrollTop to the shorter content, so the log jumps on every key.
  let twin = null;
  function grow() {
    if (!twin) {
      twin = document.createElement("textarea");
      twin.setAttribute("aria-hidden", "true");
      twin.tabIndex = -1;
      twin.readOnly = true;
      twin.style.cssText = "position:absolute;left:0;top:0;visibility:hidden;pointer-events:none;height:0;min-height:0;flex:none;overflow:hidden";
      input.parentNode.appendChild(twin);
    }
    twin.style.width = input.offsetWidth + "px";
    twin.value = input.value;
    const border = input.offsetHeight - input.clientHeight;
    const cap = Math.min(200, window.innerHeight * 0.4);
    const h = Math.max(42, Math.min(twin.scrollHeight + border, cap)) + "px";
    if (input.style.height !== h) input.style.height = h;
  }

  // Drafts: saved a moment after the last keystroke, per chat.
  const draft = { base: "", timer: 0, text: "", saved: "" };

  function say(t) { note.textContent = t; }

  function saveDraft(base, value) {
    const p = putDraft(base, value);
    draft.saving = p;
    p.finally(() => { if (draft.saving === p) draft.saving = null; });
    return p;
  }

  async function putDraft(base, value) {
    if (draft.base === base && input.value === value && value.trim()) say("Saving draft…");
    try {
      await api("PUT", base + "/draft", { text: value });
      if (draft.base === base) {
        draft.saved = value;
        if (input.value === value) say(value.trim() ? "Draft synced" : "");
      }
    } catch (_) {
      if (draft.base === base) say("Draft not saved");
    }
  }

  // flush saves a draft that waits for its timer; it returns the save.
  function flush() {
    if (!draft.timer) return draft.saving || Promise.resolve();
    clearTimeout(draft.timer);
    draft.timer = 0;
    return saveDraft(draft.base, draft.text);
  }

  function changed() {
    const chat = ctx.chat();
    if (!chat) return;
    if (draft.base !== chat.base) return;
    if (input.value === draft.saved && !draft.timer) return;
    draft.text = input.value;
    // The note says "Saving draft…" only once typing pauses and the save
    // starts, not on every key.
    if (!(input.value.trim() || draft.saved)) say("");
    clearTimeout(draft.timer);
    draft.timer = setTimeout(() => { draft.timer = 0; saveDraft(draft.base, draft.text); }, 700);
  }

  async function open(chat) {
    flush();
    closePop();
    input.value = "";
    say("");
    draft.base = chat ? chat.base : "";
    draft.saved = "";
    grow();
    update();
    if (!chat) return;
    try {
      const d = await api("GET", chat.base + "/draft");
      if (ctx.chat() !== chat || input.value || !d || !d.text) return;
      input.value = d.text;
      draft.saved = d.text;
      say("Draft synced");
      grow();
      update();
    } catch (_) { /* no draft */ }
  }

  // sent clears the draft of the chat a message just went to.
  function sent(chat) {
    if (!chat) return;
    if (draft.base === chat.base) {
      clearTimeout(draft.timer);
      draft.timer = 0;
      say("");
      if (!draft.saved && !draft.text) return;
      draft.saved = "";
      draft.text = "";
    }
    api("PUT", chat.base + "/draft", { text: "" }).catch(() => {});
  }

  // Suggestions: commands and saved prompts after a leading "/", only
  // prompts after "/prompt:", files after "@".
  const sug = { kind: null, items: [], sel: 0, q: "" };
  const commands = new Map();
  const folders = new Map();
  const saved = { at: 0, p: null };
  const promptTag = "prompt:";

  function loadPrompts() {
    if (!ctx.prompts()) return Promise.resolve([]);
    if (saved.p && Date.now() - saved.at < 30000) return saved.p;
    saved.at = Date.now();
    const p = api("GET", "/api/prompts").then((l) => (Array.isArray(l) ? l : []), () => {
      if (saved.p === p) saved.p = null;
      return null;
    });
    saved.p = p;
    return p;
  }

  // promptsChanged drops the saved prompts, after Settings changed them.
  function promptsChanged() { saved.p = null; }

  // preview is a prompt's first line of text.
  function preview(t) {
    const line = t.split("\n").find((l) => l.trim()) || "";
    return line.trim();
  }

  function loadCommands(chat) {
    const key = chat.backend;
    if (commands.has(key)) return commands.get(key);
    const p = api("GET", chat.base + "/commands").then((l) => {
      const list = Array.isArray(l) ? l : [];
      commands.set(key, Promise.resolve(list));
      return list;
    }, () => { commands.delete(key); return []; });
    commands.set(key, p);
    return p;
  }

  function loadFolder(chat, path) {
    const key = chat.base + "\n" + path;
    const hit = folders.get(key);
    if (hit && Date.now() - hit.at < 30000) return hit.p;
    const p = api("GET", chat.base + "/folder" + (path ? "?path=" + encodeURIComponent(path) : "")).catch(() => null);
    folders.set(key, { at: Date.now(), p });
    if (folders.size > 200) folders.delete(folders.keys().next().value);
    return p;
  }

  function closePop() {
    pop.hidden = true;
    pop.replaceChildren();
    sug.kind = null;
    sug.items = [];
    input.removeAttribute("aria-activedescendant");
  }

  function mark(t, q) {
    const s = el("span");
    const i = q ? t.toLowerCase().indexOf(q.toLowerCase()) : -1;
    if (i < 0) { s.textContent = t; return s; }
    s.append(t.slice(0, i), el("mark", "", t.slice(i, i + q.length)), t.slice(i + q.length));
    return s;
  }

  let seq = 0;
  async function suggest() {
    const chat = ctx.chat();
    const before = input.value.slice(0, input.selectionStart);
    const slash = before.match(/^\/(\S*)$/);
    const at = before.match(/(?:^|\s)@([^\s@]*)$/);
    const my = ++seq;
    if (!slash && !at) { closePop(); return; }
    if (slash) {
      const q = slash[1].toLowerCase();
      const only = q.startsWith(promptTag) && ctx.prompts();
      const [list, got] = await Promise.all([chat && !only ? loadCommands(chat) : [], loadPrompts()]);
      if (my !== seq) return;
      const ps = got || [];
      const items = window.HubSlash.match(list, q).map((m) => ({ kind: "cmd", c: m.c, alias: m.alias }));
      const prompts = ps.filter((p) => (promptTag + p.name.toLowerCase()).startsWith(q)).map((p) => ({ kind: "prompt", p }));
      if (only) {
        const none = !got ? "Could not load your prompts" : ps.length ? "No prompt starts with " + slash[1].slice(promptTag.length)
          : "You have no prompts yet";
        show("prompt", prompts, slash[1], "Prompts", none);
        return;
      }
      const head = !prompts.length ? "Commands" : items.length ? "Commands and prompts" : "Prompts";
      show("cmd", items.concat(prompts), slash[1], head);
      return;
    }
    if (!chat || !ctx.canFiles(chat)) { closePop(); return; }
    const typed = at[1];
    const cut = typed.lastIndexOf("/");
    const dirPart = cut >= 0 ? typed.slice(0, cut + 1) : "";
    const q = typed.slice(cut + 1);
    const root = await loadFolder(chat, "");
    if (my !== seq || !root) { if (my === seq) closePop(); return; }
    const home = root.dir || root.path;
    const f = dirPart ? await loadFolder(chat, home.replace(/\/$/, "") + "/" + dirPart.replace(/\/$/, "")) : root;
    if (my !== seq) return;
    if (!f) { closePop(); return; }
    const ql = q.toLowerCase();
    const items = (f.entries || [])
      .filter((e) => e.name.toLowerCase().includes(ql) && (q.startsWith(".") || !e.name.startsWith(".")))
      .sort((x, y) => (x.name.toLowerCase().startsWith(ql) ? 0 : 1) - (y.name.toLowerCase().startsWith(ql) ? 0 : 1) ||
        ((x.kind === "dir") === (y.kind === "dir") ? x.name.localeCompare(y.name) : x.kind === "dir" ? -1 : 1))
      .slice(0, 50)
      .map((e) => ({ kind: "file", dir: dirPart, name: e.name, folder: e.kind === "dir" }));
    const where = (chat.place && chat.place.kind === "computer" ? chat.place.name : "Sandbox") + " · " + home;
    show("file", items, q, "Files on " + where);
  }

  // show draws the suggestions; with none it closes, unless empty says
  // why there are none.
  function show(kind, items, q, head, empty) {
    if (!items.length && !empty) { closePop(); return; }
    sug.empty = empty || "";
    if (sug.kind !== kind || sug.q !== q) sug.sel = 0;
    sug.kind = kind;
    sug.items = items;
    sug.q = q;
    sug.sel = Math.max(0, Math.min(sug.sel, items.length - 1));
    draw(head);
  }

  let lastHead = "";
  function draw(head) {
    if (head) lastHead = head;
    pop.replaceChildren();
    const foot = sug.kind === "prompt" || sug.items.some((it) => it.kind === "prompt");
    const ph = el("div", "ph");
    ph.append(el("span", "grow", lastHead));
    if (!foot) ph.append(el("span", "keyonly", "↑↓ Tab"));
    pop.append(ph);
    if (!sug.items.length) pop.append(el("div", "none", sug.empty));
    sug.items.forEach((it, i) => {
      const o = el("div", "opt" + (i === sug.sel ? " sel" : ""));
      o.id = "opt-" + i;
      o.setAttribute("role", "option");
      o.setAttribute("aria-selected", String(i === sug.sel));
      const c = el("span", "c");
      let d = "";
      if (it.kind === "cmd") {
        c.append(mark(it.c.name, sug.q ? "/" + sug.q : ""));
        d = it.c.description || "";
        if (it.c.args) c.append(el("span", "dir", " " + it.c.args));
        if (it.alias) c.append(el("span", "dir", " (alias "), mark(it.alias, sug.q ? "/" + sug.q : ""), el("span", "dir", ")"));
      } else if (it.kind === "prompt") {
        o.classList.add("saved");
        c.append(mark("/" + promptTag + it.p.name, sug.q ? "/" + sug.q : ""));
        d = preview(it.p.text);
      } else {
        if (it.dir) c.append(el("span", "dir", it.dir));
        c.append(mark(it.name + (it.folder ? "/" : ""), sug.q));
      }
      o.append(c, el("span", "d", d));
      o.onmousedown = (e) => { e.preventDefault(); sug.sel = i; pick(); };
      pop.append(o);
    });
    if (foot) pop.append(promptFoot());
    pop.hidden = false;
    if (sug.items.length) input.setAttribute("aria-activedescendant", "opt-" + sug.sel);
    else input.removeAttribute("aria-activedescendant");
    const s = pop.querySelector(".opt.sel");
    if (s && s.scrollIntoView) s.scrollIntoView({ block: "nearest" });
  }

  function promptFoot() {
    const f = el("div", "pf");
    const keys = (k, what) => {
      const s = el("span", "keyonly");
      k.forEach((x) => s.append(el("kbd", "", x), " "));
      s.append(what);
      return s;
    };
    const manage = el("button", "", "Manage in Settings > Prompts");
    manage.type = "button";
    manage.tabIndex = -1;
    manage.onmousedown = (e) => e.preventDefault();
    manage.onclick = () => { closePop(); ctx.managePrompts(); };
    f.append(keys(["↑", "↓"], "choose"), keys(["Enter"], "insert"), keys(["Esc"], "close"), manage);
    return f;
  }

  function pick() {
    const it = sug.items[sug.sel];
    if (!it) return;
    const pos = input.selectionStart;
    const v = input.value;
    let ins, start, keep = false;
    if (it.kind === "prompt") {
      // The prompt's text takes the place of "/prompt:<name>", to edit
      // before sending.
      const end = pos + v.slice(pos).match(/^\S*/)[0].length;
      input.value = it.p.text + v.slice(end);
      input.selectionStart = input.selectionEnd = it.p.text.length;
      closePop();
      input.focus();
      grow();
      update();
      changed();
      return;
    }
    if (it.kind === "cmd") {
      start = 0;
      ins = it.c.name + " ";
    } else {
      start = v.slice(0, pos).lastIndexOf("@");
      ins = "@" + it.dir + it.name + (it.folder ? "/" : " ");
      keep = it.folder;
    }
    input.value = v.slice(0, start) + ins + v.slice(pos);
    input.selectionStart = input.selectionEnd = start + ins.length;
    closePop();
    input.focus();
    grow();
    update();
    changed();
    if (keep) suggest();
  }

  function onKey(e) {
    if (!pop.hidden && sug.items.length) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const n = sug.items.length;
        sug.sel = (sug.sel + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
        draw();
        return;
      }
      if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey)) { e.preventDefault(); pick(); return; }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closePop(); return; }
    }
    if (!pop.hidden && sug.kind === "prompt") {
      if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); return; }
      if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closePop(); return; }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !coarse()) {
      e.preventDefault();
      if (mode() !== "stop" && !$("send").disabled) ctx.submit();
      return;
    }
    if (e.key === "ArrowUp" && !input.value && !e.altKey && !e.ctrlKey && !e.metaKey) {
      if (ctx.editLast()) e.preventDefault();
    }
  }

  input.addEventListener("input", () => { grow(); update(); changed(); suggest(); });
  input.addEventListener("click", suggest);
  input.addEventListener("keydown", onKey);
  input.addEventListener("blur", () => setTimeout(() => { if (document.activeElement !== input) closePop(); }, 150));
  window.addEventListener("resize", grow);
  window.addEventListener("pagehide", flush);
  document.addEventListener("visibilitychange", () => { if (document.hidden) flush(); });

  return { open, sent, update, grow, setRunning, mode, changed, flush, popOpen: () => !pop.hidden, closePop, promptsChanged };
};
