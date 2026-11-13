"use strict";
// Chat search, the Usage page and the file viewer. Each part gets the app's
// helpers in ctx. Text from Hermes and from files is only ever set with
// textContent; code is colored by the vendored highlight.js, which escapes
// what it is given.

const hubFmt = {
  n: (x) => Number(x || 0).toLocaleString(),
  date: (iso) => {
    const v = Date.parse(iso);
    return v > 0 ? new Date(v).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }) : "";
  },
  when: (iso) => {
    const v = Date.parse(iso);
    return v > 0 ? new Date(v).toLocaleString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }) : "";
  },
  dur: (s) => {
    s = Math.round(Number(s) || 0);
    if (s < 60) return s + " s";
    const m = Math.round(s / 60);
    if (m < 60) return m + " min";
    return Math.floor(m / 60) + " h " + (m % 60) + " min";
  },
  bytes: (n) => {
    n = Number(n) || 0;
    if (n < 1024) return n + " B";
    if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
    return (n / 1048576).toFixed(1) + " MB";
  },
};

// HubSearch finds messages in the user's chats. The sidebar shows its
// results under the chats whose titles match.
window.HubSearch = (ctx) => {
  const { el } = ctx;
  let q = "";
  let shown = "";
  let results = [];
  let errors = {};
  let loading = false;
  let failed = "";
  let timer = 0;
  let gen = 0;
  let ctl = null;

  function query(text) {
    const want = text.trim();
    if (want === q) return;
    q = want;
    clearTimeout(timer);
    gen++;
    if (ctl) { ctl.abort(); ctl = null; }
    if (want.length < 2) {
      results = []; errors = {}; loading = false; failed = ""; shown = "";
      return;
    }
    loading = true;
    const mine = gen;
    timer = setTimeout(async () => {
      const c = new AbortController();
      ctl = c;
      try {
        const res = await fetch("/api/search?q=" + encodeURIComponent(want), { credentials: "same-origin", signal: c.signal });
        let r = {};
        try { r = await res.json(); } catch (_) { /* not JSON */ }
        if (!res.ok) throw new Error(r.error || res.statusText);
        if (mine !== gen) return;
        results = r.results || [];
        errors = r.errors || {};
        failed = "";
      } catch (e) {
        if (mine !== gen) return;
        results = []; errors = {};
        failed = e.message;
      } finally {
        if (ctl === c) ctl = null;
      }
      shown = want;
      loading = false;
      ctx.redraw();
    }, 350);
  }

  // snippet renders text with >>>match<<< marks as text and <mark> nodes.
  function snippet(text) {
    const box = el("span", "snip");
    const parts = String(text || "").replace(/\s+/g, " ").split(/(>>>|<<<)/);
    let on = false;
    for (const p of parts) {
      if (p === ">>>") { on = true; continue; }
      if (p === "<<<") { on = false; continue; }
      if (p) box.append(on ? el("mark", "", p) : document.createTextNode(p));
    }
    return box;
  }

  function draw(nav) {
    if (q.length < 2) return;
    const group = el("div", "group found");
    const h = el("div", "group-head");
    h.append(el("span", "grow", "In messages"));
    group.append(h);
    if (loading && shown !== q) {
      group.append(el("p", "none", "Searching..."));
    } else if (failed) {
      group.append(el("p", "err", "Could not search: " + failed));
    } else {
      for (const r of results) group.append(item(r));
      if (!results.length) group.append(el("p", "none", "No messages match."));
      for (const [name, why] of Object.entries(errors)) group.append(el("p", "err", name + ": " + why));
    }
    nav.append(group);
  }

  function item(r) {
    const a = el("a", "hit");
    a.href = ctx.chatHash(r.backend_id, r.session_id);
    const hit = (r.hits || [])[0];
    a.onclick = () => {
      if (hit) ctx.jump({ b: r.backend_id, s: r.session_id, row: hit.row_id });
      ctx.closeSide();
    };
    const top = el("span", "hit-top");
    top.append(el("span", "name", r.title || "New chat"), el("span", "when", hubFmt.date(r.last_active)));
    a.append(top);
    for (const x of (r.hits || []).slice(0, 2)) a.append(snippet(x.snippet));
    return a;
  }

  return { query, draw };
};

// HubUsage builds the Usage page: the Claude accounts (from ctx.claudePanel),
// Claude Code's tokens on the user's computers (from ctx.claudeCodePanel),
// tokens by day, chat and model, rate limits, the local GPU's time and
// Honcho's backlog.
window.HubUsage = (ctx) => {
  const { api, el } = ctx;
  const root = document.getElementById("usage");
  const nav = document.getElementById("usagenav");
  let backend = "";
  let days = 30;
  let shown = false;
  let gen = 0;
  let ccPanel = null;
  let ccDays = null;
  // ranges are the page's time ranges; every panel follows the one picked.
  const ranges = [[1, "Today"], [7, "Last 7 days"], [30, "Last 30 days"], ["mtd", "This month"], [90, "Last 90 days"]];
  const ccRange = { 1: "today", 7: "7d", 30: "30d", mtd: "mtd", 90: "90d" };
  const dayCount = () => (days === "mtd" ? new Date().getDate() : days);

  function setHash() {
    const h = "#/usage/" + encodeURIComponent(backend) + "/" + days;
    if (location.hash !== h) history.replaceState(null, "", h);
  }

  async function show(rest) {
    shown = true;
    root.hidden = false;
    nav.classList.add("on");
    const [b, d] = (rest || "").split("/").map((x) => { try { return decodeURIComponent(x || ""); } catch (_) { return ""; } });
    if (d === "mtd") days = d;
    else if ([1, 7, 30, 90].includes(Number(d))) days = Number(d);
    let backends = [];
    try { backends = await api("GET", "/api/backends"); } catch (_) { /* drawn below */ }
    if (!shown) return;
    const pick = backends.find((x) => x.id === b) || backends.find((x) => x.id === backend) ||
      backends.find((x) => x.id === ctx.defaultBackend()) || backends[0];
    backend = pick ? pick.id : "";
    if (backend) setHash();
    load(backends);
  }

  function hide() {
    shown = false;
    gen++;
    ccPanel = null;
    root.hidden = true;
    root.replaceChildren();
    nav.classList.remove("on");
  }

  function head(backends) {
    const bar = el("div", "mem-head");
    if (backends.length > 1) {
      const sel = el("select");
      sel.setAttribute("aria-label", "Agent server");
      for (const b of backends) {
        const o = el("option", "", b.name);
        o.value = b.id;
        o.selected = b.id === backend;
        sel.append(o);
      }
      sel.onchange = () => { backend = sel.value; setHash(); load(backends); };
      bar.append(sel);
    }
    bar.append(el("span", "grow"));
    const range = el("select");
    range.setAttribute("aria-label", "Time range");
    for (const [d, label] of ranges) {
      const o = el("option", "", label);
      o.value = String(d);
      o.selected = d === days;
      range.append(o);
    }
    range.onchange = () => { days = range.value === "mtd" ? "mtd" : Number(range.value); setHash(); load(backends); };
    const again = el("button", "", "Refresh");
    again.type = "button";
    again.onclick = () => { ccPanel = null; load(backends); };
    bar.append(range, again);
    return bar;
  }

  async function load(backends) {
    const mine = ++gen;
    const body = el("div", "mem-body usage-body");
    root.replaceChildren(head(backends), body);
    // The Claude Code panel is the user's, not the backend's: it stays
    // when the Hermes changes.
    if (ccPanel && ccDays !== days) ccPanel = null;
    if (!ccPanel && ctx.claudeCodePanel) {
      ccPanel = ctx.claudeCodePanel(ccRange[days]);
      ccDays = days;
    }
    const code = ccPanel ? [ccPanel] : [];
    if (!backend) {
      body.append(...code, el("p", "none", "No assistant is set up for you yet."));
      return;
    }
    body.append(el("p", "none", "Loading..."));
    const claude = el("div");
    (ctx.claudePanel ? ctx.claudePanel(backend) : Promise.resolve(null)).then((p) => {
      if (mine === gen && p) claude.replaceChildren(p);
    }, () => {});
    let r;
    try {
      r = await api("GET", "/api/usage/" + encodeURIComponent(backend) + "?days=" + dayCount());
    } catch (e) {
      if (mine !== gen) return;
      body.replaceChildren(claude, ...code, el("p", "err", "Could not load usage: " + e.message));
      return;
    }
    if (mine !== gen) return;
    body.replaceChildren(claude, ...code, ...draw(r));
  }

  function section(title, hint) {
    const s = el("section", "usage-sec");
    s.append(el("h2", "", title));
    if (hint) s.append(el("p", "mem-hint", hint));
    return s;
  }

  function table(cols, rows) {
    const wrap = el("div", "usage-table");
    const t = el("table");
    const tr = el("tr");
    for (const c of cols) tr.append(el("th", c.num ? "num" : "", c.label));
    const thead = el("thead");
    thead.append(tr);
    const tbody = el("tbody");
    for (const r of rows) {
      const row = el("tr");
      r.forEach((v, i) => {
        const td = el("td", cols[i].num ? "num" : "");
        if (v instanceof Node) td.append(v); else td.textContent = v;
        row.append(td);
      });
      tbody.append(row);
    }
    t.append(thead, tbody);
    wrap.append(t);
    return wrap;
  }

  const cached = (t) => (t.cache_read || 0) + (t.cache_write || 0);
  const dayKey = (iso) => {
    const d = new Date(Date.parse(iso));
    return d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0");
  };

  function draw(r) {
    const out = [];
    const sessions = r.sessions || [];
    const live = new Map((r.observed && r.observed.chats || []).map((c) => [c.session_id, c]));

    const byDay = section("Tokens by day", "Each chat counts on the day it was last used, with all of its tokens.");
    if (r.tokens_error) byDay.append(el("p", "err", r.tokens_error));
    else if (!sessions.length) byDay.append(el("p", "none", "No chats used any tokens in this time."));
    else {
      const days = new Map();
      for (const s of sessions) {
        const k = dayKey(s.last_active);
        const d = days.get(k) || { input: 0, output: 0, cached: 0, calls: 0, chats: new Set() };
        d.input += s.input; d.output += s.output; d.cached += cached(s); d.calls += s.calls; d.chats.add(s.chat_id);
        days.set(k, d);
      }
      const keys = [...days.keys()].sort().reverse();
      const top = Math.max(...keys.map((k) => days.get(k).input + days.get(k).output), 1);
      byDay.append(table([{ label: "Day" }, { label: "Chats", num: true }, { label: "Input", num: true },
        { label: "Output", num: true }, { label: "Cached", num: true }, { label: "Calls", num: true }, { label: "" }],
      keys.map((k) => {
        const d = days.get(k);
        const bar = el("span", "usage-bar");
        const fill = el("span", "fill");
        fill.style.width = Math.max(1, Math.round((d.input + d.output) / top * 100)) + "%";
        bar.append(fill);
        return [new Date(k + "T12:00:00").toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric" }),
          hubFmt.n(d.chats.size), hubFmt.n(d.input), hubFmt.n(d.output), hubFmt.n(d.cached), hubFmt.n(d.calls), bar];
      })));
    }
    out.push(byDay);

    const chats = section("Chats", "The 50 chats that used the most tokens. Context shows how full the chat's context was the last time this hub saw it run.");
    if (sessions.length) {
      const by = new Map();
      for (const s of sessions) {
        const c = by.get(s.chat_id) || { id: s.chat_id, title: "", model: s.model, input: 0, output: 0, calls: 0, last: s.last_active };
        if (s.session_id === s.chat_id || !c.title) c.title = s.title;
        c.input += s.input; c.output += s.output; c.calls += s.calls;
        if (Date.parse(s.last_active) > Date.parse(c.last)) { c.last = s.last_active; c.model = s.model; }
        by.set(s.chat_id, c);
      }
      const list = [...by.values()].sort((a, b) => (b.input + b.output) - (a.input + a.output)).slice(0, 50);
      chats.append(table([{ label: "Chat" }, { label: "Model" }, { label: "Input", num: true }, { label: "Output", num: true },
        { label: "Calls", num: true }, { label: "Context", num: true }, { label: "Last used" }],
      list.map((c) => {
        const a = el("a", "", c.title || "Untitled chat");
        a.href = ctx.chatHash(r.backend_id, c.id);
        const l = live.get(c.id);
        const ctxText = l && l.context_max ? hubFmt.n(l.context_used) + " of " + hubFmt.n(l.context_max) +
          " (" + Math.round(l.context_used / l.context_max * 100) + "%)" : "";
        return [a, c.model, hubFmt.n(c.input), hubFmt.n(c.output), hubFmt.n(c.calls), ctxText, hubFmt.when(c.last)];
      })));
      if (r.truncated) chats.append(el("p", "mem-hint", "Only the newest 5000 sessions are counted."));
    } else {
      chats.append(el("p", "none", "None."));
    }
    out.push(chats);

    const models = section("Models", "Tokens and API calls of each model. Cost is the agent's estimate where it knows the price.");
    if ((r.models || []).length) {
      models.append(table([{ label: "Model" }, { label: "Provider" }, { label: "Input", num: true }, { label: "Output", num: true },
        { label: "Cached", num: true }, { label: "Calls", num: true }, { label: "Cost", num: true }],
      r.models.map((m) => [m.model, m.provider, hubFmt.n(m.input), hubFmt.n(m.output), hubFmt.n(cached(m)), hubFmt.n(m.calls),
        m.cost_usd > 0 ? "$" + m.cost_usd.toFixed(2) : ""])));
    } else {
      models.append(el("p", "none", r.tokens_error ? "Not available." : "None."));
    }
    out.push(models);

    const limits = section("API limits", "The rate limits each API provider last reported in its replies, as this hub saw them while chats ran" +
      (r.observed_since ? " (since " + hubFmt.when(r.observed_since) + ")" : "") + ".");
    const ls = (r.observed && r.observed.limits) || [];
    if (ls.length) {
      const bucket = (b) => b && b.limit ? hubFmt.n(b.remaining) + " of " + hubFmt.n(b.limit) + " left" +
        (b.reset_seconds ? ", resets in " + hubFmt.dur(b.reset_seconds) : "") : "";
      limits.append(table([{ label: "Provider" }, { label: "Requests per minute" }, { label: "Tokens per minute" },
        { label: "Requests per hour" }, { label: "Tokens per hour" }, { label: "Seen" }],
      ls.map((l) => [l.provider || "Unknown", bucket(l.requests_min), bucket(l.tokens_min), bucket(l.requests_hour),
        bucket(l.tokens_hour), hubFmt.when(l.at)])));
    } else {
      limits.append(el("p", "none", "No limits seen yet. The agent reads them from the x-ratelimit headers that some providers send. " +
        "It does not read Anthropic's own rate-limit headers, and it does not record spending limits."));
    }
    limits.append(el("p", "mem-hint", "Ollama on the local GPU has no API limits."));
    const speeds = (r.observed && r.observed.chats || []).filter((c) => c.tokens_per_s > 0);
    if (speeds.length) {
      const by = new Map();
      for (const c of speeds) if (!by.has(c.model)) by.set(c.model, c);
      limits.append(el("h3", "", "Speed"));
      limits.append(el("p", "mem-hint", "Tokens per second and seconds per API call that the agent measured in the latest chat of each model this hub saw."));
      limits.append(table([{ label: "Model" }, { label: "Tokens per second", num: true }, { label: "Seconds per call", num: true }, { label: "Seen" }],
        [...by.values()].map((c) => [c.model || "Unknown", c.tokens_per_s.toFixed(1),
          c.avg_latency_s ? c.avg_latency_s.toFixed(1) : "", hubFmt.when(c.at)])));
    }
    out.push(limits);

    const gpu = section("Local GPU", "Busy is the time Ollama had at least one request running, checked each minute. " +
      "People and Background add up the time of each request, so requests that ran at once count more than once.");
    if (r.gpu && r.gpu.length) {
      gpu.append(table([{ label: "" }, { label: "Busy", num: true }, { label: "People", num: true }, { label: "Background", num: true },
        { label: "Requests", num: true }],
      r.gpu.map((g) => [g.label, hubFmt.dur(g.busy_s), hubFmt.dur(g.people_s), hubFmt.dur(g.background_s), hubFmt.n(Math.round(g.requests))])));
    } else if (r.gpu_error) {
      gpu.append(el("p", "err", r.gpu_error));
    } else {
      gpu.append(el("p", "none", "Not set up. The hub needs usage.prometheus_url to read the GPU's time."));
    }
    out.push(gpu);

    const mem = section("Memory backlog", "Messages Honcho has not yet turned into memory. Its background work runs only when the GPU is free of people's requests.");
    if (r.backlog) {
      const b = r.backlog;
      mem.append(table([{ label: "Waiting", num: true }, { label: "Working on", num: true }, { label: "Done", num: true }, { label: "Total", num: true }],
        [[hubFmt.n(b.pending), hubFmt.n(b.in_progress), hubFmt.n(b.completed), hubFmt.n(b.total)]]));
    } else if (r.backlog_error) {
      mem.append(el("p", "err", r.backlog_error));
    } else {
      mem.append(el("p", "none", "This agent server does not use learned memory (Honcho)."));
    }
    out.push(mem);
    return out;
  }

  return { show, hide };
};

// HubFiles shows the folder of a chat that runs on one of the user's
// computers or in the sandbox, the files Hermes wrote in it and what git
// shows as changed. It never changes a file.
window.HubFiles = (ctx) => {
  const { api, el } = ctx;
  const maxText = 1024 * 1024;
  const images = { png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp", bmp: "image/bmp" };
  const langs = { go: "go", py: "python", js: "javascript", mjs: "javascript", cjs: "javascript", ts: "typescript", tsx: "typescript",
    jsx: "javascript", json: "json", md: "markdown", sh: "bash", bash: "bash", zsh: "bash", yml: "yaml", yaml: "yaml", toml: "ini",
    ini: "ini", c: "c", h: "c", cc: "cpp", cpp: "cpp", hpp: "cpp", cs: "csharp", java: "java", kt: "kotlin", rs: "rust", rb: "ruby",
    php: "php", lua: "lua", pl: "perl", r: "r", sql: "sql", swift: "swift", css: "css", scss: "scss", less: "less", html: "xml",
    xml: "xml", svg: "xml", graphql: "graphql", diff: "diff", patch: "diff", mk: "makefile", vb: "vbnet" };
  let cur = null;

  const ext = (name) => { const m = /\.([A-Za-z0-9]+)$/.exec(name || ""); return m ? m[1].toLowerCase() : ""; };
  const baseName = (p) => (p || "").replace(/\/+$/, "").split("/").pop() || p;

  function button(cls, text, fn) {
    const b = el("button", cls, text);
    b.type = "button";
    b.onclick = fn;
    return b;
  }

  function open(chat, title) {
    if (cur && cur.chat === chat) { cur.box.hidden = false; return; }
    close();
    const box = el("div", "pageview files");
    box.setAttribute("role", "dialog");
    box.setAttribute("aria-modal", "true");
    box.setAttribute("aria-label", "Files");
    const bar = el("div", "pageview-bar");
    const back = button("", "Back", () => ctx.back());
    bar.append(back, el("span", "name", "Files: " + (title || "chat")));
    const tabs = el("div", "mem-tabs");
    const body = el("div", "files-body");
    box.append(bar, tabs, body);
    box.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); ctx.back(); } });
    document.body.append(box);
    cur = { chat, box, tabs, body, tab: "folder", path: "", urls: [], gen: 0 };
    drawTabs();
    folder("");
    back.focus();
  }

  function hide() { if (cur) cur.box.hidden = true; }

  function close() {
    if (!cur) return;
    for (const u of cur.urls) URL.revokeObjectURL(u);
    cur.box.remove();
    cur = null;
  }

  function drawTabs() {
    cur.tabs.replaceChildren();
    for (const [id, label] of [["folder", "Folder"], ["changes", "Changes"]]) {
      const b = button("mem-tab", label, () => {
        if (cur.tab === id) return;
        cur.tab = id;
        drawTabs();
        if (id === "folder") folder(cur.path); else changes();
      });
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", String(cur.tab === id));
      cur.tabs.append(b);
    }
  }

  function crumbs(f) {
    const nav = el("nav", "crumbs");
    nav.setAttribute("aria-label", "Folder path");
    const root = (f.roots || []).filter((r) => f.path === r || f.path.startsWith(r.replace(/\/$/, "") + "/"))
      .sort((x, y) => y.length - x.length)[0] || "/";
    const steps = [{ label: root, path: root }];
    let acc = root;
    for (const part of f.path.slice(root.length).split("/").filter(Boolean)) {
      acc = acc.replace(/\/$/, "") + "/" + part;
      steps.push({ label: part, path: acc });
    }
    steps.forEach((st, i) => {
      if (i) nav.append(el("span", "", "/"));
      if (i === steps.length - 1) nav.append(el("span", "here", st.label));
      else nav.append(button("", st.label, () => folder(st.path)));
    });
    return nav;
  }

  async function folder(path) {
    const c = cur;
    const mine = ++c.gen;
    c.body.replaceChildren(el("p", "none", "Loading..."));
    let f;
    try {
      f = await api("GET", c.chat.base + "/folder" + (path ? "?path=" + encodeURIComponent(path) : ""));
    } catch (e) {
      if (c !== cur || mine !== c.gen) return;
      const items = [el("p", "err", e.message)];
      if (path) items.push(button("", "Back to the chat's folder", () => folder("")));
      c.body.replaceChildren(...items);
      return;
    }
    if (c !== cur || mine !== c.gen) return;
    c.path = f.path;
    const list = el("div", "files-list");
    const head = el("div", "files-head");
    head.append(crumbs(f));
    if (f.path !== f.dir) head.append(button("link", "Chat's folder", () => folder("")));
    list.append(head);
    const ul = el("ul", "files-entries");
    for (const e of f.entries) {
      const li = el("li");
      const full = f.path.replace(/\/$/, "") + "/" + e.name;
      const b = button("entry " + e.kind, "", () => (e.kind === "dir" ? folder(full) : preview(full, e.size)));
      b.append(el("span", "ename", e.name + (e.kind === "dir" ? "/" : "")),
        el("span", "esize", e.link ? "Link" : e.kind === "file" ? hubFmt.bytes(e.size) : ""));
      b.disabled = e.kind === "other";
      li.append(b);
      ul.append(li);
    }
    if (!f.entries.length) ul.append(el("li", "none", "Empty folder."));
    list.append(ul);
    if (f.truncated) list.append(el("p", "mem-hint", "Only the first 1000 entries are listed."));
    const pane = el("div", "files-preview");
    pane.append(el("p", "none", "Pick a file to see it here."));
    const split = el("div", "files-split");
    split.append(list, pane);
    c.pane = pane;
    c.split = split;
    c.body.replaceChildren(split);
  }

  function download(c, blob, name) {
    const a = el("a", "mem-button", "Download");
    const u = URL.createObjectURL(new Blob([blob], { type: "application/octet-stream" }));
    c.urls.push(u);
    a.href = u;
    a.download = name;
    a.rel = "noopener";
    return a;
  }

  // preview shows a file: text with colors, images, and a download for the rest.
  async function preview(path, size) {
    const c = cur;
    if (!c.pane) return;
    const mine = ++c.gen;
    const name = baseName(path);
    const pane = c.pane;
    c.split.classList.add("showing");
    const head = el("div", "files-phead");
    head.append(button("files-close", "Back to list", () => { c.split.classList.remove("showing"); }), el("span", "pname", name));
    pane.replaceChildren(head, el("p", "none", "Loading..."));
    if (size > 32 * 1024 * 1024) {
      pane.replaceChildren(head, el("p", "none", "This file is too large to open here."));
      return;
    }
    let blob;
    try {
      const r = await fetch(c.chat.base + "/file?path=" + encodeURIComponent(path), { credentials: "same-origin" });
      if (!r.ok) {
        let msg = r.statusText;
        try { msg = (await r.json()).error || msg; } catch (_) { /* not JSON */ }
        throw new Error(msg);
      }
      blob = await r.blob();
    } catch (e) {
      if (c !== cur || mine !== c.gen) return;
      pane.replaceChildren(head, el("p", "err", "Could not open " + name + ": " + e.message));
      return;
    }
    if (c !== cur || mine !== c.gen) return;
    head.append(download(c, blob, name));
    const x = ext(name);
    if (images[x]) {
      const u = URL.createObjectURL(new Blob([blob], { type: images[x] }));
      c.urls.push(u);
      const img = el("img", "files-img");
      img.alt = name;
      img.src = u;
      pane.replaceChildren(head, img);
      return;
    }
    if (x === "pdf") {
      pane.replaceChildren(head, el("p", "none", "PDF files open with Download."));
      return;
    }
    if (blob.size > maxText) {
      pane.replaceChildren(head, el("p", "none", "This file is too large to show. Download it to read it."));
      return;
    }
    const buf = new Uint8Array(await blob.arrayBuffer());
    if (c !== cur || mine !== c.gen) return;
    let text;
    try {
      if (buf.subarray(0, 8000).includes(0)) throw new Error("binary");
      text = new TextDecoder("utf-8", { fatal: true }).decode(buf);
    } catch (_) {
      pane.replaceChildren(head, el("p", "none", "This is not a text file. Download it to open it."));
      return;
    }
    const lang = langs[x] || (/^(Makefile|Dockerfile)$/.test(name) ? (name === "Makefile" ? "makefile" : "bash") : "");
    pane.replaceChildren(head, window.hubMarkdown.codeBlock(lang, text));
  }

  async function changes() {
    const c = cur;
    const mine = ++c.gen;
    c.pane = null;
    c.body.replaceChildren(el("p", "none", "Loading..."));
    const [w, d] = await Promise.allSettled([api("GET", c.chat.base + "/changes"), api("GET", c.chat.base + "/diff")]);
    if (c !== cur || mine !== c.gen) return;
    const box = el("div", "files-changes");
    box.append(el("h2", "", "Files Talaria wrote in this chat"));
    box.append(el("p", "mem-hint", "From its write_file and patch calls. A file may have changed since."));
    if (w.status === "rejected") box.append(el("p", "err", w.reason.message));
    else if (!w.value.files.length) box.append(el("p", "none", "None."));
    else {
      const ul = el("ul", "files-entries");
      for (const f of w.value.files) {
        const li = el("li");
        const b = button("entry file", "", () => openFile(f.abs));
        b.append(el("span", "ename", f.path), el("span", "esize", (f.count > 1 ? f.count + " times, " : "") + hubFmt.when(f.at)));
        b.disabled = !f.abs;
        li.append(b);
        ul.append(li);
      }
      box.append(ul);
      if (w.value.truncated) box.append(el("p", "mem-hint", "Only the latest 500 files are listed."));
    }
    box.append(el("h2", "", "Git changes in this folder"));
    if (d.status === "rejected") box.append(el("p", "err", d.reason.message));
    else if (d.value.refused) box.append(el("p", "none", "This folder's git repository is outside the folders Talaria may read on this computer, so its changes are not shown."));
    else if (!d.value.git) box.append(el("p", "none", "This folder is not in a git repository."));
    else {
      const g = d.value;
      box.append(el("p", "mem-hint", "Changes in " + g.dir + " since the last commit, with files git does not track yet."));
      if (!g.files.length && !g.untracked.length) box.append(el("p", "none", "No changes."));
      const ul = el("ul", "files-entries");
      const status = { added: "Added", modified: "Changed", deleted: "Deleted", unmerged: "Conflict" };
      for (const f of g.files) {
        const li = el("li");
        const b = button("entry file", "", () => openFile(g.dir.replace(/\/$/, "") + "/" + f.path));
        b.append(el("span", "ename", f.path), el("span", "esize", status[f.status] || f.status));
        b.disabled = f.status === "deleted";
        li.append(b);
        ul.append(li);
      }
      for (const p of g.untracked) {
        const li = el("li");
        const b = button("entry file", "", () => openFile(g.dir.replace(/\/$/, "") + "/" + p));
        b.append(el("span", "ename", p), el("span", "esize", "New, not tracked"));
        li.append(b);
        ul.append(li);
      }
      box.append(ul);
      if (g.hidden) box.append(el("p", "mem-hint", g.hidden + " protected file" + (g.hidden === 1 ? " is" : "s are") + " left out, such as .env files and keys."));
      if (g.diff) box.append(window.hubMarkdown.codeBlock("diff", g.diff));
      if (g.truncated) box.append(el("p", "mem-hint", "The diff is cut short. It shows at most 1 MB and 500 files."));
    }
    c.body.replaceChildren(box);
  }

  // openFile shows a file of the Changes tab in the folder view.
  async function openFile(abs) {
    if (!abs || !cur) return;
    const dir = abs.replace(/\/[^/]*$/, "") || "/";
    cur.tab = "folder";
    drawTabs();
    await folder(dir);
    if (cur && cur.pane) preview(abs, 0);
  }

  // state and restore keep the viewer's tab and folder across a reload.
  const state = () => (cur && !cur.box.hidden ? { tab: cur.tab, path: cur.path } : null);
  function restore(st) {
    if (!cur || !st) return;
    cur.tab = st.tab === "changes" ? "changes" : "folder";
    drawTabs();
    if (cur.tab === "folder") folder(st.path || ""); else changes();
  }

  return { open, hide, close, state, restore, current: () => (cur ? cur.chat : null) };
};
