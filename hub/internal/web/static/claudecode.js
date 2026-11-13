"use strict";
// HubClaudeCode builds the Usage page's Claude Code panel: the tokens that
// Claude Code used on the user's computers and in Hermes, from the counts
// talaria sends, with totals, tokens per day stacked by group, and a table
// by account, computer, model or project. Cost is at API prices, an
// estimate. An account no computer names gets a "Name it" button, which
// stores the user's own name for it on the hub.
window.HubClaudeCode = (ctx) => {
  const { api, el } = ctx;
  const ns = "http://www.w3.org/2000/svg";
  const colors = ["var(--s1)", "var(--s2)", "var(--s3)", "var(--s4)"];
  const groups = [["account", "Account"], ["computer", "Computer"], ["model", "Model"], ["project", "Project"]];
  const heads = { account: "Account", computer: "Computer", model: "Model", project: "Project" };
  const store = "hub.claudecode";
  const S = { range: "7d", group: "account" };
  const ranges = ["today", "7d", "30d", "mtd", "90d"];
  try {
    const saved = JSON.parse(localStorage.getItem(store) || "{}");
    if (groups.some(([v]) => v === saved.group)) S.group = saved.group;
  } catch (_) { /* defaults */ }

  const k = (n) => n >= 1e9 ? (n / 1e9).toFixed(2) + "B" : n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : String(n);
  const usd = (n) => "$" + n.toFixed(2);
  const svgEl = (t, a) => { const e = document.createElementNS(ns, t); for (const x in a) e.setAttribute(x, a[x]); return e; };

  function toast(text) {
    const x = document.getElementById("toast");
    if (!x) return;
    x.textContent = text;
    x.hidden = false;
    clearTimeout(toast.h);
    toast.h = setTimeout(() => { x.hidden = true; }, 2000);
  }

  function seg(label, opts, key, redraw) {
    const g = el("div", "cc-seg");
    g.setAttribute("role", "group");
    g.setAttribute("aria-label", label);
    for (const [v, l] of opts) {
      const b = el("button", "", l);
      b.type = "button";
      b.setAttribute("aria-pressed", String(S[key] === v));
      b.onclick = () => {
        if (S[key] === v) return;
        S[key] = v;
        try { localStorage.setItem(store, JSON.stringify({ group: S.group })); } catch (_) { /* per-viewer only */ }
        redraw();
      };
      g.append(b);
    }
    return g;
  }

  // series are the chart's groups: all of them up to four, else the top
  // three and the rest as Other. color says each group's swatch.
  function series(d) {
    const gs = d.groups.filter((g) => g.total > 0);
    if (gs.length <= 4) return { list: gs, color: (i) => colors[i % 4] };
    const other = { label: "Other", key: "\u0000other", daily: d.days.map(() => 0), total: 0 };
    for (const g of gs.slice(3)) {
      g.daily.forEach((v, i) => { other.daily[i] += v; });
      other.total += g.total;
    }
    return { list: [...gs.slice(0, 3), other], color: (i) => colors[Math.min(i, 3)] };
  }

  const nameOf = (g) => g.label || (g.account_uuid ? "account " + g.account_uuid.slice(0, 8) : g.key);

  function chart(d, s) {
    const W = 640, H = 190, L = 40, B = 22, top = 8;
    const svg = svgEl("svg", { class: "cc-chart", viewBox: "0 0 " + W + " " + H, role: "img", "aria-label": "Tokens per day, stacked by group" });
    const n = d.days.length;
    const sums = d.days.map((_, i) => s.list.reduce((a, g) => a + g.daily[i], 0));
    const maxv = Math.max(...sums, 1);
    const step = Math.pow(10, Math.floor(Math.log10(maxv)));
    const ymax = Math.ceil(maxv / step) * step;
    for (let i = 0; i <= 4; i++) {
      const v = ymax * i / 4, y = H - B - (H - B - top) * i / 4;
      svg.append(svgEl("line", { x1: L, x2: W, y1: y, y2: y, class: "grid" }));
      const t = svgEl("text", { x: L - 6, y: y + 3, "text-anchor": "end" });
      t.textContent = k(v);
      svg.append(t);
    }
    const bw = (W - L) / n;
    for (let i = 0; i < n; i++) {
      let y = H - B;
      s.list.forEach((g, gi) => {
        const v = g.daily[i];
        if (!v) return;
        const h = (H - B - top) * v / ymax;
        y -= h;
        const r = svgEl("rect", { x: L + i * bw + bw * .15, y, width: bw * .7, height: Math.max(h, .5), fill: s.color(gi) });
        const tt = svgEl("title", {});
        tt.textContent = nameOf(g) + ": " + k(v);
        r.append(tt);
        svg.append(r);
      });
      if (n <= 11 || i % 5 === 0 || i === n - 1) {
        const [, m, day] = d.days[i].split("-").map(Number);
        const t = svgEl("text", { x: L + i * bw + bw / 2, y: H - 6, "text-anchor": "middle" });
        t.textContent = m + "/" + day;
        svg.append(t);
      }
    }
    return svg;
  }

  function nameControl(g, reload) {
    const box = el("span", "cc-name");
    const b = el("button", "cc-nameit", "Name it");
    b.type = "button";
    b.onclick = () => {
      const input = el("input", "cc-nameinput");
      input.type = "text";
      input.maxLength = 64;
      input.setAttribute("aria-label", "Name for account " + g.account_uuid.slice(0, 8));
      const save = el("button", "cc-nameit", "Save");
      save.type = "button";
      const done = async () => {
        const name = input.value.trim();
        if (!name) { box.replaceChildren(b); return; }
        save.disabled = true;
        try {
          await api("PUT", "/api/claude-code/accounts/" + encodeURIComponent(g.account_uuid) + "/name", { name });
          toast("Named");
          reload();
        } catch (e) {
          save.disabled = false;
          toast("Could not save the name: " + e.message);
        }
      };
      save.onclick = done;
      input.onkeydown = (e) => {
        if (e.key === "Enter") done();
        if (e.key === "Escape") box.replaceChildren(b);
      };
      box.replaceChildren(input, save);
      input.focus();
    };
    box.append(b);
    return box;
  }

  function draw(sec, body, d, reload) {
    const s = series(d);
    const T = d.total;
    const inp = T.input + T.cache_write + T.cache_read;
    const tiles = el("div", "cc-totals");
    const unpriced = d.unpriced_models || [];
    for (const [a, b, c] of [
      ["Tokens", k(T.total), d.since ? "since " + d.since + ", the first day with data" : d.days.length + (d.days.length === 1 ? " day" : " days")],
      ["Output", k(T.output), "written by Claude"],
      ["Cache hit", (inp ? Math.round(T.cache_read / inp * 100) : 0) + "%", k(T.cache_read) + " read from cache"],
      ["At API prices", usd(T.cost_usd), unpriced.length ? "estimate, without " + unpriced.length + (unpriced.length === 1 ? " model" : " models") : "estimate"],
    ]) {
      const t = el("div", "cc-tot");
      t.append(el("div", "k", a), el("div", "v", b), el("div", "s", c));
      tiles.append(t);
    }
    const parts = [tiles];
    if (!d.groups.length) {
      parts.push(el("p", "none", "No Claude Code use in this time."));
      body.replaceChildren(...parts);
      return;
    }
    const legend = el("div", "cc-legend");
    s.list.forEach((g, i) => {
      const sp = el("span");
      const sw = el("i");
      sw.style.background = s.color(i);
      sp.append(sw, nameOf(g));
      legend.append(sp);
    });
    const graph = el("div");
    graph.append(chart(d, s), legend);
    parts.push(graph);

    const wrap = el("div", "cc-tablewrap");
    const tb = el("table", "cc-table");
    const hr = el("tr");
    for (const h of [heads[d.group], "Input", "Output", "Cache read", "Cache write", "Total", "At API prices"]) hr.append(el("th", null, h));
    const thead = el("thead");
    thead.append(hr);
    const tbody = el("tbody");
    const shown = new Map(s.list.map((g, i) => [g.key, i]));
    d.groups.forEach((g) => {
      const tr = el("tr");
      const td = el("td");
      const sw = el("span", "sw");
      const ci = shown.has(g.key) ? shown.get(g.key) : s.list.length === 4 && s.list[3].key === "\u0000other" ? 3 : -1;
      sw.style.background = ci >= 0 ? s.color(ci) : "transparent";
      td.append(sw);
      if (d.group === "account" && g.account_uuid && !g.label) {
        td.append(el("span", "id", "account " + g.account_uuid.slice(0, 8)), nameControl(g, reload));
      } else {
        td.append(nameOf(g));
      }
      tr.append(td);
      for (const v of [g.input, g.output, g.cache_read, g.cache_write, g.total]) tr.append(el("td", null, k(v)));
      tr.append(el("td", null, usd(g.cost_usd)));
      tbody.append(tr);
    });
    const ft = el("tr", "total");
    ft.append(el("td", null, "Total"));
    for (const v of [T.input, T.output, T.cache_read, T.cache_write, T.total]) ft.append(el("td", null, k(v)));
    ft.append(el("td", null, usd(T.cost_usd)));
    tbody.append(ft);
    tb.append(thead, tbody);
    wrap.append(tb);
    parts.push(wrap);
    if (unpriced.length) parts.push(el("p", "mem-hint", "No API price for " + unpriced.join(", ") + "; their tokens count as $0."));
    body.replaceChildren(...parts);
  }

  // panel returns the panel's section for range, the Usage page's time
  // range, and loads it.
  function panel(range) {
    if (ranges.includes(range)) S.range = range;
    const sec = el("section", "usage-sec cc-panel");
    sec.setAttribute("aria-labelledby", "cc-h");
    const head = el("div", "cc-head");
    const h = el("h2", "", "Claude Code");
    h.id = "cc-h";
    head.append(h);
    const hint = el("p", "mem-hint", "From the Claude Code transcripts on your computers and on the server. Only token counts leave each computer, " +
      "never what was said. Cost is what the tokens would cost at API prices; a subscription does not bill this.");
    const controls = el("div", "cc-controls");
    const groupBox = el("span");
    controls.append("Group by ", groupBox);
    const body = el("div", "cc-body");
    body.append(el("p", "none", "Loading..."));
    sec.append(head, hint, controls, body);
    let gen = 0;
    const load = async () => {
      groupBox.replaceChildren(seg("Group by", groups, "group", load));
      const mine = ++gen;
      let d;
      try {
        d = await api("GET", "/api/claude-code/usage?range=" + encodeURIComponent(S.range) + "&group=" + encodeURIComponent(S.group));
      } catch (e) {
        if (mine === gen) body.replaceChildren(el("p", "err", "Could not load Claude Code usage: " + e.message));
        return;
      }
      if (mine === gen) draw(sec, body, d, load);
    };
    load();
    return sec;
  }

  return { panel };
};
