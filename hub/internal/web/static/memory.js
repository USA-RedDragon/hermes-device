"use strict";
// HubMemory builds the Memory page: Hermes's notes (MEMORY.md), its profile
// of the user (USER.md) and what Honcho learned from the user's chats. ctx
// gives it the app's api and el helpers, the latest chat list and a way to
// open a chat.
window.HubMemory = (ctx) => {
  const { api, el } = ctx;
  const root = document.getElementById("memory");
  const nav = document.getElementById("memnav");
  const count = document.getElementById("memcount");
  const fmt = (n) => Number(n || 0).toLocaleString("en-US");
  const tabs = [
    { id: "notes", target: "memory", label: "Notes" },
    { id: "you", target: "user", label: "About you" },
    { id: "learned", label: "Learned" },
  ];
  const copy = {
    notes: "Hermes reads these notes at the start of every chat. Keep them short. Changes apply to new chats.",
    you: "What Hermes knows about you as a person. It reads this in every chat, on every device and in Telegram. Changes apply to new chats.",
    learned: "Facts Hermes picked up from your chats over time. It looks these up when they seem relevant. Delete anything wrong or out of date.",
    help: "Hermes keeps two short files that it reads at the start of every chat: its own notes and what it knows about you. It also learns facts from your chats over time and looks them up when they seem relevant. Here you can read, fix and delete all of it. Chats that are already open keep what they started with; changes apply to new chats.",
    full: "Almost full. Shorten or remove a line before Hermes needs room for a new one.",
    off: "This file is turned off in Hermes's memory settings, so Hermes does not read it.",
  };

  let backends = [];
  let backend = "";
  let tab = "notes";
  let shown = false;
  let notes = null;
  let notesErr = "";
  let ui = { edit: null, adding: false, raw: false, confirm: null, more: null, help: false, busy: false, note: "", bad: false };
  let learned = null;

  function freshLearned() {
    return { q: "", view: "all", oldest: false, items: [], page: 0, pages: 0, total: 0, loading: false, err: "",
      sel: new Set(), confirm: false, info: null, infoErr: "", card: false, adding: false, gen: 0, armed: "" };
  }

  function setBadge() {
    const n = backends.reduce((a, b) => a + (b.pending || 0), 0);
    nav.hidden = !backends.length;
    count.hidden = !n;
    count.textContent = n ? String(n) : "";
    nav.classList.toggle("on", shown);
  }

  async function refresh() {
    try {
      backends = await api("GET", "/api/memory");
    } catch (_) {
      return;
    }
    setBadge();
  }

  function setHash() {
    const h = "#/memory/" + encodeURIComponent(backend) + "/" + tab;
    if (location.hash !== h) history.replaceState(null, "", h);
  }

  async function show(rest) {
    shown = true;
    root.hidden = false;
    nav.classList.add("on");
    const [b, t] = (rest || "").split("/").map((x) => decodeURIComponent(x || ""));
    await refresh();
    if (!shown) return;
    const pick = backends.find((x) => x.id === b) || backends.find((x) => x.id === backend) ||
      backends.find((x) => x.id === ctx.defaultBackend()) || backends[0];
    if (pick && pick.id !== backend) { backend = pick.id; notes = null; learned = null; }
    tab = tabs.some((x) => x.id === t) ? t : tab;
    if (!tabOK(tab)) tab = tabs.find((x) => tabOK(x.id)).id;
    resetUI();
    if (backend) setHash();
    draw();
    load();
  }

  function hide() {
    shown = false;
    root.hidden = true;
    nav.classList.remove("on");
  }

  function current() { return backends.find((x) => x.id === backend); }
  function tabOK(id) {
    const b = current();
    if (!b) return id !== "learned";
    return id === "learned" ? b.learned : (b.notes || !b.learned);
  }
  function resetUI() { ui = { edit: null, adding: false, raw: false, confirm: null, more: null, help: ui.help, busy: false, note: "", bad: false }; }
  function path(p) { return "/api/memory/" + encodeURIComponent(backend) + p; }

  async function load() {
    if (!backend) return;
    if (tab === "learned") {
      if (!learned) { learned = freshLearned(); loadFacts(true); loadInfo(); }
      return;
    }
    await loadNotes();
  }

  async function loadNotes() {
    const b = backend;
    try {
      const n = await api("GET", path("/notes"));
      if (b !== backend) return;
      notes = n;
      notesErr = "";
      countPending();
    } catch (e) {
      if (b !== backend) return;
      notesErr = e.message;
    }
    if (shown) draw();
  }

  function countPending() {
    const b = current();
    if (b && notes) { b.pending = notes.pending.length; setBadge(); }
  }

  // change runs one change and redraws with the answer, or shows why not.
  async function change(method, p, body, after) {
    if (ui.busy) return;
    ui.busy = true;
    ui.note = "";
    draw();
    try {
      const n = await api(method, path(p), body);
      notes = n;
      countPending();
      if (after) after();
    } catch (e) {
      ui.note = e.message;
      ui.bad = true;
    } finally {
      ui.busy = false;
    }
    draw();
  }

  function btn(cls, text, onclick, label) {
    const b = el("button", cls, text);
    b.type = "button";
    if (label) b.setAttribute("aria-label", label);
    b.onclick = onclick;
    b.disabled = ui.busy;
    return b;
  }

  function draw() {
    if (!shown) return;
    const head = el("div", "mem-head");
    if (backends.length > 1) {
      const sel = el("select", "mem-backend");
      sel.setAttribute("aria-label", "Hermes");
      for (const b of backends) {
        const o = el("option", "", b.name);
        o.value = b.id;
        o.selected = b.id === backend;
        sel.append(o);
      }
      sel.onchange = () => { backend = sel.value; notes = null; learned = null; resetUI(); if (!tabOK(tab)) tab = tabs.find((x) => tabOK(x.id)).id; setHash(); draw(); load(); };
      head.append(sel);
    }
    head.append(el("span", "grow"));
    head.append(btn("ghost", "What is this?", () => { ui.help = !ui.help; draw(); }));
    const bar = el("div", "mem-tabs");
    bar.setAttribute("role", "tablist");
    for (const x of tabs) {
      if (!tabOK(x.id)) continue;
      const b = el("button", "mem-tab", x.label);
      b.type = "button";
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", x.id === tab ? "true" : "false");
      if (x.id === "learned" && learned && learned.info) b.append(" ", el("span", "mem-n", "(" + fmt(learned.info.total) + ")"));
      b.onclick = () => { if (tab === x.id) return; tab = x.id; resetUI(); setHash(); draw(); load(); };
      bar.append(b);
    }
    const body = el("div", "mem-body");
    body.setAttribute("role", "tabpanel");
    if (ui.help) body.append(el("p", "mem-hint", copy.help));
    if (!backend) {
      body.append(el("p", "mem-hint", "None of your Hermes backends has memory the hub can show."));
    } else if (tab === "learned") {
      drawLearned(body);
    } else {
      drawNotes(body, tabs.find((x) => x.id === tab));
    }
    const scroll = root.querySelector(".mem-body");
    const top = scroll ? scroll.scrollTop : 0;
    const act = document.activeElement;
    const key = act && root.contains(act) ? act.dataset.k : "";
    const range = key && "selectionStart" in act ? [act.selectionStart, act.selectionEnd] : null;
    root.replaceChildren(head, bar, body);
    body.scrollTop = top;
    const again = key && root.querySelector('[data-k="' + key + '"]');
    if (again) {
      again.focus({ preventScroll: true });
      if (range) { try { again.setSelectionRange(range[0], range[1]); } catch (_) { /* not a text field */ } }
    }
  }

  function status(body) {
    if (ui.note) {
      const p = el("p", ui.bad ? "mem-note bad" : "mem-note", ui.note);
      p.setAttribute("role", ui.bad ? "alert" : "status");
      body.append(p);
    }
  }

  function meter(store) {
    const pct = store.limit > 0 ? Math.min(100, Math.round((store.used / store.limit) * 100)) : 0;
    const m = el("div", pct >= 85 ? "mem-meter warn" : "mem-meter");
    const row = el("div", "row");
    row.append(el("span", "", "Space used"), el("span", "", fmt(store.used) + " / " + fmt(store.limit) + " characters"));
    const track = el("div", "track");
    const fill = el("div", "fill");
    fill.style.width = pct + "%";
    track.append(fill);
    track.setAttribute("role", "meter");
    track.setAttribute("aria-valuemin", "0");
    track.setAttribute("aria-valuemax", String(store.limit));
    track.setAttribute("aria-valuenow", String(store.used));
    track.setAttribute("aria-label", "Space used");
    m.append(row, track);
    return { m, pct };
  }

  function drawNotes(body, t) {
    body.append(el("p", "mem-hint", copy[t.id]));
    if (!notes) {
      body.append(el("p", notesErr ? "mem-note bad" : "mem-hint", notesErr ? "Could not load memory: " + notesErr : "Loading."));
      return;
    }
    const store = notes.targets[t.target];
    if (!store.enabled) body.append(el("p", "mem-note", copy.off));
    const { m, pct } = meter(store);
    body.append(m);
    status(body);
    const list = el("div", "mem-entries");
    for (const p of notes.pending.filter((x) => x.target === t.target)) list.append(pendingRow(p));
    if (ui.raw) {
      list.append(rawEditor(t, store));
    } else {
      store.entries.forEach((text) => list.append(entryRow(t, store, text)));
      if (ui.adding) list.append(editor(t, store, "", null));
      if (!store.entries.length && !ui.adding && !notes.pending.some((x) => x.target === t.target)) {
        list.append(el("p", "mem-hint", t.id === "you" ? "Hermes knows nothing about you yet." : "Hermes has no notes yet."));
      }
    }
    body.append(list);
    if (!ui.raw && !ui.adding) {
      const acts = el("div", "mem-acts");
      acts.append(btn("", t.id === "you" ? "Add a line" : "Add a note", () => { ui.adding = true; ui.edit = null; ui.note = ""; draw(); focusEditor(); }));
      body.append(acts);
    }
    if (pct >= 90) body.append(el("p", "mem-hint", copy.full));
    body.append(advancedNotes(t, store));
  }

  function changeWord(c) {
    return { add: "add", replace: "replace", patch: "change", remove: "delete" }[c.action] || c.action;
  }

  function pendingRow(p) {
    const row = el("div", "mem-entry pending");
    const text = el("div", "mem-text");
    for (const c of p.changes) {
      text.append(el("div", "who", "Hermes wants to " + changeWord(c)));
      if (c.action === "remove") {
        text.append(el("div", "", c.entry || c.old_text));
      } else if (c.action === "add") {
        text.append(el("div", "", c.content));
      } else {
        if (c.entry) text.append(el("div", "was", c.entry));
        text.append(el("div", "", c.action === "patch" ? "“" + c.old_text + "” becomes “" + c.content + "”" : c.content));
      }
    }
    if (!p.changes.length) text.append(el("div", "", p.summary));
    const acts = el("div", "mem-entry-acts");
    acts.append(btn("", "Reject", () => change("POST", "/pending/" + encodeURIComponent(p.id) + "/reject")),
      btn("primary", "Approve", () => change("POST", "/pending/" + encodeURIComponent(p.id) + "/approve")));
    row.append(text, acts);
    return row;
  }

  function entryRow(t, store, text) {
    if (ui.edit === text) return editor(t, store, text, text);
    const row = el("div", "mem-entry");
    if (ui.more === text) row.classList.add("open");
    row.append(el("div", "mem-text", text));
    const acts = el("div", "mem-entry-acts");
    if (ui.confirm === text) {
      acts.append(el("span", "mem-ask", "Delete this note?"), btn("ghost", "Cancel", () => { ui.confirm = null; draw(); }),
        btn("danger", "Delete", () => change("POST", "/notes/" + t.target + "/remove", { entry: text }, () => { ui.confirm = null; })));
    } else {
      acts.append(btn("ghost mem-more", "⋯", () => { ui.more = ui.more === text ? null : text; draw(); }, "More"),
        btn("ghost wide", "Edit", () => { ui.edit = text; ui.adding = false; ui.note = ""; draw(); focusEditor(); }),
        btn("ghost danger-text wide", "Delete", () => { ui.confirm = text; draw(); }));
    }
    row.append(acts);
    return row;
  }

  function focusEditor() {
    const ta = root.querySelector(".mem-entry.editing textarea");
    if (ta) { ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length); }
  }

  function editor(t, store, initial, entry) {
    const row = el("div", "mem-entry editing");
    const ta = el("textarea");
    ta.value = ui.draft !== undefined && ui.draftFor === entry ? ui.draft : initial;
    ta.setAttribute("aria-label", entry === null ? "New note" : "Edit note");
    ta.dataset.k = "note";
    const n = el("span", "", "");
    const room = () => store.limit - store.used + (entry !== null ? entry.length : store.entries.length ? -3 : 0);
    const tell = () => {
      const left = room() - ta.value.trim().length;
      n.textContent = fmt(ta.value.trim().length) + " characters" + (left < 0 ? ", " + fmt(-left) + " over the limit" : "");
      n.classList.toggle("bad", left < 0);
    };
    ta.oninput = () => { ui.draft = ta.value; ui.draftFor = entry; tell(); };
    tell();
    const done = () => { ui.edit = null; ui.adding = false; ui.draft = undefined; };
    const save = () => {
      const v = ta.value.trim();
      if (!v) return;
      if (entry === null) change("POST", "/notes/" + t.target + "/add", { content: v }, done);
      else change("POST", "/notes/" + t.target + "/edit", { entry, content: v }, done);
    };
    ta.onkeydown = (e) => {
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); save(); }
      if (e.key === "Escape") { e.stopPropagation(); done(); draw(); }
    };
    const er = el("div", "mem-editrow");
    er.append(n, btn("ghost", "Cancel", () => { done(); draw(); }), btn("primary", "Save", save));
    row.append(ta, er);
    return row;
  }

  function rawEditor(t, store) {
    const row = el("div", "mem-entry editing raw");
    const ta = el("textarea");
    const base = ui.rawBase || store.entries.slice();
    ta.value = ui.draft !== undefined && ui.draftFor === "raw" ? ui.draft : base.join("\n§\n");
    ta.setAttribute("aria-label", "Edit " + (t.target === "user" ? "USER.md" : "MEMORY.md"));
    ta.dataset.k = "raw";
    ta.rows = Math.min(24, Math.max(8, store.entries.length * 3));
    ta.oninput = () => { ui.draft = ta.value; ui.draftFor = "raw"; };
    const er = el("div", "mem-editrow");
    const done = () => { ui.raw = false; ui.rawBase = null; ui.draft = undefined; };
    er.append(el("span", "", "Put a line with only § between notes."), btn("ghost", "Cancel", () => { done(); draw(); }),
      btn("primary", "Save", () => change("POST", "/notes/" + t.target + "/replace", { content: ta.value, base }, done)));
    row.append(ta, er);
    return row;
  }

  function advancedNotes(t, store) {
    const d = el("details", "mem-adv");
    d.open = !!ui.advOpen;
    d.ontoggle = () => { ui.advOpen = d.open; };
    d.append(el("summary", "", "Advanced"));
    const dl = el("dl");
    const add = (k, v) => dl.append(el("dt", "", k), el("dd", "", v));
    add("File", store.file);
    add("Ask before Hermes saves", (notes.approval ? "On" : "Off") + " (memory.write_approval)");
    add("Limit", fmt(store.limit) + " characters");
    d.append(dl);
    const acts = el("div", "mem-acts");
    if (!ui.raw) acts.append(btn("", "Edit as one text file", () => { ui.raw = true; ui.rawBase = store.entries.slice(); ui.edit = null; ui.adding = false; ui.note = ""; draw(); }));
    acts.append(btn("", notes.approval ? "Turn off approval" : "Turn on approval",
      () => change("POST", "/approval", { enabled: !notes.approval })));
    d.append(acts);
    if (notes.approval) d.append(el("p", "mem-hint", "Hermes asks before it saves a note. Its proposed notes wait here, and the sidebar shows a count."));
    return d;
  }

  // Learned facts.
  function day(iso) {
    const d = new Date(iso);
    if (isNaN(d)) return "";
    const now = new Date();
    if (d.toDateString() === now.toDateString()) return "Today";
    const o = { month: "short", day: "numeric" };
    if (d.getFullYear() !== now.getFullYear()) o.year = "numeric";
    return d.toLocaleDateString("en-US", o);
  }

  function chatTitle(sid) {
    const l = ctx.sessions();
    const s = l && l.sessions.find((x) => x.backend_id === backend && x.id === sid);
    return s ? (s.title || "Untitled chat") : "";
  }

  async function loadFacts(reset) {
    const L = learned;
    if (reset) { L.items = []; L.page = 0; L.pages = 0; L.sel.clear(); L.confirm = false; L.armed = ""; }
    const gen = ++L.gen;
    L.loading = true;
    L.err = "";
    draw();
    const q = new URLSearchParams({ view: L.view, page: String(L.page + 1), order: L.oldest ? "oldest" : "newest" });
    if (L.q.trim()) q.set("q", L.q.trim());
    try {
      const p = await api("GET", path("/learned?" + q));
      if (gen !== L.gen || L !== learned) return;
      let items = p.items;
      if (L.q.trim() && !L.oldest) items = items.slice().sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
      const have = new Set(L.items.map((c) => c.id));
      L.items = L.items.concat(items.filter((c) => !have.has(c.id)));
      L.page = p.page;
      L.pages = p.pages;
      L.total = p.total;
    } catch (e) {
      if (gen !== L.gen || L !== learned) return;
      L.err = e.message;
    }
    L.loading = false;
    draw();
  }

  async function loadInfo() {
    const L = learned;
    try {
      const info = await api("GET", path("/learned/info"));
      if (L !== learned) return;
      L.info = info;
      L.infoErr = "";
    } catch (e) {
      if (L !== learned) return;
      L.infoErr = e.message;
    }
    draw();
  }

  let searchTimer = 0;
  function drawLearned(body) {
    body.append(el("p", "mem-hint", copy.learned));
    const L = learned;
    if (!L) {
      body.append(el("p", "mem-hint", "Loading."));
      return;
    }
    const f = el("div", "mem-filters");
    const s = el("input");
    s.type = "search";
    s.dataset.k = "search";
    s.placeholder = "Search what Hermes learned";
    s.setAttribute("aria-label", "Search learned facts");
    s.value = L.q;
    s.oninput = () => {
      L.q = s.value;
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => { if (L === learned) loadFacts(true); }, 400);
    };
    const chip = (text, on, onclick) => {
      const c = el("button", on ? "mem-chip on" : "mem-chip", text);
      c.type = "button";
      c.setAttribute("aria-pressed", on ? "true" : "false");
      c.onclick = onclick;
      return c;
    };
    f.append(s,
      chip("Newest first", !L.oldest, () => { L.oldest = !L.oldest; loadFacts(true); }),
      chip("Hermes's view", L.view === "hermes", () => { L.view = L.view === "hermes" ? "all" : "hermes"; loadFacts(true); }),
      chip("Your own words", L.view === "self", () => { L.view = L.view === "self" ? "all" : "self"; loadFacts(true); }));
    body.append(f);
    status(body);
    if (L.sel.size) body.append(selBar(L));
    if (L.err) body.append(el("p", "mem-note bad", "Could not load what Hermes learned: " + L.err));
    if (L.items.length) {
      const list = el("div", "mem-facts");
      for (const c of L.items) list.append(factRow(L, c));
      body.append(list);
    } else if (!L.loading && !L.err) {
      body.append(el("p", "mem-hint", L.q.trim() ? "Nothing matches that search." : "Hermes has not learned anything here yet."));
    }
    if (L.loading) body.append(el("p", "mem-hint", "Loading."));
    else if (!L.q.trim() && L.page < L.pages) {
      const a = el("div", "mem-acts");
      a.append(btn("", "Show more", () => loadFacts(false)));
      body.append(a);
    }
    body.append(advancedLearned(L));
  }

  function selBar(L) {
    const bar = el("div", "mem-selbar");
    const n = L.sel.size;
    const word = n === 1 ? "fact" : "facts";
    if (L.confirm) {
      bar.append(el("span", "", "Delete " + fmt(n) + " " + word + " for good? This cannot be undone."),
        btn("", "Cancel", () => { L.confirm = false; draw(); }),
        btn("danger", "Delete " + fmt(n) + " " + word, () => deleteFacts(L, [...L.sel])));
    } else {
      bar.append(el("span", "", fmt(n) + " selected"), btn("", "Clear", () => { L.sel.clear(); draw(); }),
        btn("danger", "Delete " + fmt(n) + " " + word, () => { L.confirm = true; draw(); }));
    }
    return bar;
  }

  async function deleteFacts(L, ids) {
    if (ui.busy || !ids.length) return;
    ui.busy = true;
    ui.note = "";
    draw();
    let deleted = 0;
    try {
      for (let i = 0; i < ids.length; i += 100) {
        const r = await api("POST", path("/learned/delete"), { ids: ids.slice(i, i + 100) });
        deleted += r.deleted;
      }
      ui.note = "Deleted " + fmt(deleted) + " " + (deleted === 1 ? "fact." : "facts.");
      ui.bad = false;
    } catch (e) {
      ui.note = "Could not delete every fact: " + e.message;
      ui.bad = true;
    } finally {
      ui.busy = false;
    }
    if (L !== learned) return;
    L.confirm = false;
    L.armed = "";
    L.sel.clear();
    loadFacts(true);
    loadInfo();
  }

  function factRow(L, c) {
    const row = el("div", L.sel.has(c.id) ? "mem-fact sel" : "mem-fact");
    const box = el("input");
    box.type = "checkbox";
    box.checked = L.sel.has(c.id);
    box.setAttribute("aria-label", "Select this fact");
    box.onchange = () => { if (box.checked) L.sel.add(c.id); else L.sel.delete(c.id); L.confirm = false; L.armed = ""; draw(); };
    const txt = el("div", "txt", c.content);
    txt.onclick = () => { box.checked = !box.checked; box.onchange(); };
    const meta = el("div", "meta");
    meta.append(el("span", "", day(c.created_at)));
    meta.append(el("span", "", c.view === "self" ? "Your own words" : c.view === "hermes" ? "Hermes's view" : ""));
    const title = c.session_id ? chatTitle(c.session_id) : "";
    if (title) {
      const a = el("a", "", "from “" + title + "”");
      a.href = ctx.chatHash(backend, c.session_id);
      meta.append(a);
    }
    const x = btn("danger-text x", L.armed === c.id ? "Delete for good" : "Delete", () => {
      if (L.armed !== c.id) { L.armed = c.id; draw(); return; }
      deleteFacts(L, [c.id]);
    });
    row.append(box, txt, meta, x);
    return row;
  }

  function advancedLearned(L) {
    const d = el("details", "mem-adv");
    d.open = !!ui.advOpen;
    d.ontoggle = () => { ui.advOpen = d.open; };
    d.append(el("summary", "", "Advanced"));
    if (L.infoErr) d.append(el("p", "mem-note bad", "Could not load the details: " + L.infoErr));
    if (L.info) {
      const dl = el("dl");
      const add = (k, v) => dl.append(el("dt", "", k), el("dd", "", v));
      add("Stored in", "Honcho, workspace “" + L.info.workspace + "”");
      add("Total facts", fmt(L.info.total) + " (Hermes's view " + fmt(L.info.hermes) + ", your own words " + fmt(L.info.self) + ")");
      add("Profile card", fmt(L.info.card.length) + (L.info.card.length === 1 ? " line" : " lines"));
      d.append(dl);
    }
    if (L.adding) d.append(factEditor(L));
    if (L.card && L.info) {
      const ul = el("ul", "mem-card");
      for (const line of L.info.card) ul.append(el("li", "", line));
      if (!L.info.card.length) ul.append(el("li", "", "The profile card is empty."));
      d.append(ul);
    }
    const acts = el("div", "mem-acts");
    if (!L.adding) acts.append(btn("", "Add a fact", () => { L.adding = true; draw(); const i = root.querySelector(".mem-addfact textarea"); if (i) i.focus(); }));
    acts.append(btn("", L.card ? "Hide profile card" : "View profile card", () => { L.card = !L.card; draw(); }));
    const a = el("a", "mem-button", "Export as JSON");
    a.href = path("/learned/export");
    a.download = "hermes-learned-" + backend + ".json";
    acts.append(a);
    d.append(acts);
    return d;
  }

  function factEditor(L) {
    const row = el("div", "mem-entry editing mem-addfact");
    const ta = el("textarea");
    ta.setAttribute("aria-label", "New fact");
    ta.dataset.k = "fact";
    ta.value = L.draft || "";
    ta.oninput = () => { L.draft = ta.value; };
    ta.placeholder = "A fact about you, such as: Prefers short answers.";
    const er = el("div", "mem-editrow");
    er.append(el("span", "", "Saved as Hermes's view of you."), btn("ghost", "Cancel", () => { L.adding = false; draw(); }),
      btn("primary", "Save", async () => {
        const v = ta.value.trim();
        if (!v || ui.busy) return;
        ui.busy = true;
        try {
          await api("POST", path("/learned"), { content: v });
          L.adding = false;
          L.draft = "";
          ui.note = "Saved.";
          ui.bad = false;
        } catch (e) {
          ui.note = "Could not save the fact: " + e.message;
          ui.bad = true;
        } finally {
          ui.busy = false;
        }
        if (L === learned) { loadFacts(true); loadInfo(); }
      }));
    row.append(ta, er);
    return row;
  }

  nav.onclick = () => ctx.closeSide();
  return { refresh, show, hide, shown: () => shown };
};
