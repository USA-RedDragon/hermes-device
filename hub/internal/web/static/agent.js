"use strict";
// HubAgent is the agent's own settings in the hub: Settings > Agent,
// Skills and tools, Diagnostics, Backup, the admin part of Settings >
// Telegram, the admin-only update banner and the quick panel in a chat.
// Everything goes through the hub's /api/agent/<backend>/... and
// /api/admin/... routes; the hub checks who may change what. The pure
// helpers (doctorSummary, logTone, modeLabel, bannerText, updateSteps,
// liveLine, zones, splitSkills) run against internal/web/testdata/agent/run.js.
window.HubAgent = (() => {
  const modes = [["manual", "Ask every time"], ["smart", "Ask only when a command looks risky"], ["off", "Never ask"]];
  const shortModes = { manual: "Ask every time", smart: "Ask when risky", off: "Never ask" };
  const thresholds = [0.4, 0.5, 0.7, 0.85];
  const lockText = "Set in the deployment config.";

  function modeLabel(m, short) {
    return (short ? shortModes : Object.fromEntries(modes))[m] || "";
  }

  // Warnings about things most installs never set up: a provider login that
  // was never made, a tool whose key or program is absent, a file the
  // deployment manages itself. They are counted, not listed.
  const optionalLine = [
    /\(not logged in\)/i, /\(not configured\)/i, /\(missing [A-Z][A-Z0-9_]*\)/, /system dependency not met/i,
    /generation unavailable/i, /^No [A-Z][A-Z0-9_]*_(TOKEN|KEY)\b/, /^No API key found/i, /not initialized/i, /\.env file missing/i, /config\.yaml not found/i,
  ];

  // quiet rewrites a line of the report in Talaria's words: no command to
  // type on the server and no product name of the agent's maker.
  function quiet(line) {
    return String(line)
      .replace(/\s*\((?:run|fix|try):[^)]*\)/gi, "")
      .replace(/\bHermes(?: Agent)?\b/g, "the agent")
      .replace(/\bhermes\b/g, "agent");
  }

  // doctorSummary reads the health check's output: how many checks passed,
  // the lines to look at (failures, and warnings about things the install
  // uses) and how many warnings were about optional things that are not set
  // up.
  function doctorSummary(lines) {
    let passed = 0;
    let optional = 0;
    const look = [];
    for (const raw of lines || []) {
      const l = String(raw).trim();
      if (/^(✓|✔|\[ok\]|OK\b)/i.test(l)) passed++;
      else if (/^(✗|✘|⚠|!|\[warn|\[fail|\[error|WARN|FAIL|ERROR)/i.test(l)) {
        const text = l.replace(/^(✗|✘|⚠|!)\s*/, "");
        if (optionalLine.some((re) => re.test(text))) optional++;
        else look.push(quiet(text).replace(/\b[a-z]+(?:_[a-z]+)+\b/g, (w) => w.replace(/_/g, " ")));
      }
    }
    return { passed, look, optional };
  }

  // reportText is the full report for the details box: the agent's own
  // banner and its closing list of commands to type are left out, and so
  // are the warnings about optional things (the summary counts them).
  function reportText(lines) {
    const blocks = [];
    let cur = null;
    let skipping = false;
    for (const raw of lines || []) {
      const l = String(raw);
      if (/^[─━-]{8,}/.test(l.trim())) break;
      if (/^\s*[┌│└]/.test(l) || !l.trim()) continue;
      if (/^\s*◆/.test(l)) {
        cur = { head: quiet(l).trim(), body: [] };
        blocks.push(cur);
        skipping = false;
        continue;
      }
      if (/^\s*→/.test(l)) {
        if (!skipping && cur && !/^\s*→\s*Run /.test(l)) cur.body.push(quiet(l).trimEnd());
        continue;
      }
      const text = l.trim().replace(/^(✗|✘|⚠|!)\s*/, "");
      skipping = /^\s*(✗|✘|⚠|!)/.test(l) && optionalLine.some((re) => re.test(text));
      if (!skipping && cur) cur.body.push(quiet(l).trimEnd());
    }
    return blocks.filter((b) => b.body.length).map((b) => b.head + "\n" + b.body.join("\n")).join("\n\n");
  }

  // optionalLineText says how many optional things are not set up.
  function optionalText(n) {
    return n + " optional " + (n === 1 ? "item is" : "items are") + " not set up. That is fine unless you want them.";
  }

  // logTone is the color of one agent log line: w for warnings, e for errors.
  function logTone(line) {
    if (/\b(ERROR|CRITICAL|Traceback)\b/.test(line)) return "e";
    if (/\bWARN(ING)?\b/.test(line)) return "w";
    return "";
  }

  // liveLine says when a change reaches chats.
  function liveLine(what, on, applied) {
    const verb = on === undefined ? "changed" : on ? "on" : "off";
    return applied ? what + " is " + verb + ". Open chats use it from their next message."
      : what + " is " + verb + ". New chats use it.";
  }

  // bannerText is the update banner's line for an admin, or "" when nothing
  // is behind (and always "" for a member: they never get the status).
  function bannerText(me, st) {
    if (!me || !me.admin || !st) return "";
    if (st.latest) return "Talaria " + st.latest.version + " is out. This server runs " + st.current + ".";
    if ((st.agent || []).length) return "A new agent release is out (" + st.agent[0].latest + "). This server runs " + st.agent[0].running + ".";
    if ((st.computers || []).length) {
      const c = st.computers;
      return c.length === 1 ? c[0].name + " runs talaria " + c[0].version + ", and " + c[0].latest + " is out."
        : c.length + " computers run an older talaria. " + c[0].latest + " is out.";
    }
    return "";
  }

  // updateSteps are the steps to update an install of the given type.
  function updateSteps(install) {
    return ({
      compose: ["On the server, in the folder with compose.yaml:",
        "curl -fsSL https://github.com/USA-RedDragon/talaria/archive/refs/heads/main.tar.gz \\\n  | tar -xz --strip-components=3 talaria-main/deploy/compose\ndocker compose up -d --wait",
        "Your .env stays."],
      helm: ["Upgrade the release with the new chart:", "helm upgrade <release> oci://ghcr.io/usa-reddragon/charts/talaria --reuse-values", "Your values stay."],
      aio: ["Pull the new image and start the container again:", "docker pull ghcr.io/usa-reddragon/talaria-aio:latest\ndocker stop talaria && docker rm talaria\n# then run it again with the same docker run command", "Your data volume stays."],
      unraid: ["In Unraid, open Docker, click Talaria and choose Apply Update.", "", "Your settings stay."],
      truenas: ["In TrueNAS, open Apps, pick Talaria and choose Update (or redeploy the custom app with the new image).", "", "Your settings stay."],
    })[install] || ["Pull the new images and start Talaria again the way you installed it.", "", ""];
  }

  // zones lists time zones with the browser's own first.
  function zones(browser, all) {
    const list = (all || []).slice();
    if (!list.includes("UTC")) list.unshift("UTC");
    if (browser) {
      const i = list.indexOf(browser);
      if (i >= 0) list.splice(i, 1);
      list.unshift(browser);
    }
    return list;
  }

  // splitSkills puts skills in the order the lists show: on first, then by name.
  function splitSkills(skills, filter) {
    const f = String(filter || "").toLowerCase();
    return (skills || []).filter((s) => !f || s.name.toLowerCase().includes(f) || String(s.description || "").toLowerCase().includes(f))
      .sort((a, b) => (b.enabled - a.enabled) || (a.label || a.name).localeCompare(b.label || b.name));
  }

  const origin = { bundled: "Came with Talaria", hub: "Added from the catalog", agent: "Made by the agent", external: "From a shared folder" };

  function create(ctx) {
    const { api, el } = ctx;
    const I = window.HubIcons;
    const enc = encodeURIComponent;

    function btn(cls, text, fn, icon) {
      const b = el("button", cls, "");
      b.type = "button";
      if (icon) b.append(I.icon(icon));
      b.append(text);
      if (fn) b.onclick = fn;
      return b;
    }

    function say(tone, title, lines, acts) {
      const box = el("div", "say " + tone);
      box.setAttribute("role", tone === "good" ? "status" : "alert");
      box.append(I.icon(tone === "good" ? "checkcircle" : tone === "lock" ? "lock" : "alert"));
      const body = el("div", "");
      const p = el("p", "");
      if (title) p.append(el("b", "", title));
      body.append(p);
      for (const l of lines || []) if (l) body.append(typeof l === "string" ? el("p", "", l) : l);
      if (acts && acts.length) {
        const row = el("div", "row");
        row.append(...acts);
        body.append(row);
      }
      box.append(body);
      return box;
    }

    function toggle(on, disabled, label) {
      const t = el("input", "toggle");
      t.type = "checkbox";
      t.setAttribute("role", "switch");
      t.checked = !!on;
      t.disabled = !!disabled;
      if (label) t.setAttribute("aria-label", label);
      return t;
    }

    // switchRow is a labeled switch; fn(on) saves and answers a line to show.
    function switchRow(title, small, on, opts) {
      const o = opts || {};
      const row = el("label", "switch");
      const g = el("span", "grow");
      g.append(el("b", "", title));
      if (small) g.append(el("small", "", small));
      if (o.dot) row.append(el("span", "dot " + o.dot));
      const t = toggle(on, o.disabled, title);
      row.append(g);
      if (o.extra) row.append(o.extra);
      row.append(t);
      return { row, t };
    }

    function seg(choices, cur, label, fn, disabled) {
      const box = el("div", "seg");
      box.setAttribute("role", "group");
      box.setAttribute("aria-label", label);
      for (const [v, text] of choices) {
        const b = btn("", text, null);
        b.setAttribute("aria-pressed", String(v === cur));
        b.disabled = !!disabled;
        b.onclick = () => {
          box.querySelectorAll("button").forEach((x) => x.setAttribute("aria-pressed", String(x === b)));
          fn(v);
        };
        box.append(b);
      }
      return box;
    }

    function waitLine(text) {
      const w = el("span", "wait");
      w.append(el("span", "spin"), text);
      return w;
    }

    function shared() {
      const s = el("span", "shared");
      s.append(I.icon("users"), "Applies to everyone on this server");
      return s;
    }

    function lockLine() {
      const p = el("p", "lockline");
      p.append(I.icon("lock"), lockText);
      return p;
    }

    const backends = () => (ctx.me().agent_admin_backends || []);
    let chosen = "";
    function backend() {
      const bs = backends();
      if (!bs.includes(chosen)) chosen = bs.includes(ctx.me().default_backend) ? ctx.me().default_backend : bs[0] || "";
      return chosen;
    }
    const base = (b) => "/api/agent/" + enc(b || backend());

    function head(p, title, hint, withBackend, redraw) {
      p.replaceChildren(el("h2", "", title));
      if (hint) p.append(el("p", "hint", hint));
      if (withBackend && backends().length > 1) {
        const sel = el("select", "f");
        sel.setAttribute("aria-label", "Agent server");
        for (const b of backends()) {
          const o = el("option", "", b);
          o.value = b;
          o.selected = b === backend();
          sel.append(o);
        }
        sel.onchange = () => { chosen = sel.value; redraw(); };
        p.append(sel);
      }
    }

    // ---- Settings > Agent ----
    async function agentPane(p) {
      head(p, "Agent", "How the agent behaves for every chat on this server.", true, () => agentPane(p));
      p.append(shared());
      const wait = waitLine("Loading");
      p.append(wait);
      let st;
      try {
        st = await api("GET", base() + "/settings");
      } catch (e) {
        wait.replaceWith(say("bad", "Could not load the agent's settings.", [e.message]));
        return;
      }
      wait.remove();
      const locked = (k) => (st.locked || []).includes(k);
      const note = (box, el2) => { box.querySelector(".say")?.remove(); if (el2) box.append(el2); };

      // Personality
      const ps = el("div", "field");
      ps.append(el("span", "", "Personality"));
      const ta = el("textarea", "f");
      ta.value = st.soul || "";
      ta.rows = 6;
      ta.setAttribute("aria-label", "Personality");
      ta.disabled = locked("soul");
      ps.append(ta, el("small", "", "What the agent is told about itself in every chat."));
      const pacts = el("div", "acts");
      const saveSoul = async (content) => {
        note(ps);
        try {
          const r = await api("PUT", base() + "/soul", { content });
          ta.value = content || st.soul_default || "";
          note(ps, say("good", liveLine("The personality", undefined, r.applied_live)));
        } catch (e) { note(ps, say("bad", "Not saved.", [e.message])); }
      };
      pacts.append(btn("primary", "Save", () => saveSoul(ta.value)), btn("ghost", "Reset to Talaria's default", () => saveSoul("")));
      if (locked("soul")) ps.append(lockLine()); else ps.append(pacts);
      p.append(ps);

      // Before risky commands
      const ap = el("div", "field");
      ap.append(el("span", "", "Before risky commands"));
      ap.append(seg(modes, st.approvals_mode, "Before risky commands", async (v) => {
        note(ap);
        try {
          await api("PUT", base() + "/settings", { approvals_mode: v });
          note(ap, say("good", "Saved. It applies to the next command in every chat."));
        } catch (e) { note(ap, say("bad", "Not saved.", [e.message])); }
      }, locked("approvals.mode")));
      ap.append(el("small", "", "Risky means things like deleting files or changing the system. You answer in the chat or in a notification."));
      if (locked("approvals.mode")) ap.append(lockLine());
      p.append(ap);

      // Long chats
      const lc = el("div", "field");
      lc.append(el("span", "", "Long chats"));
      const sw = switchRow("Summarize older messages when a chat gets long", "Keeps long chats working. The agent writes a short summary and drops the details it summarized.",
        st.compression_enabled, { disabled: locked("compression.enabled") });
      const th = el("div", "field");
      th.append(el("span", "", "Start when the chat fills this much of what the model can read at once"));
      th.append(seg(thresholds.map((t) => [t, Math.round(t * 100) + "%"]), st.compression_threshold, "When to summarize", async (v) => {
        note(lc);
        try { await api("PUT", base() + "/settings", { compression_threshold: v }); note(lc, say("good", "Saved. New chats use it.")); } catch (e) { note(lc, say("bad", "Not saved.", [e.message])); }
      }, locked("compression.threshold")));
      th.hidden = !st.compression_enabled;
      sw.t.onchange = async () => {
        note(lc);
        th.hidden = !sw.t.checked;
        try { await api("PUT", base() + "/settings", { compression_enabled: sw.t.checked }); note(lc, say("good", "Saved. New chats use it.")); } catch (e) { sw.t.checked = !sw.t.checked; note(lc, say("bad", "Not saved.", [e.message])); }
      };
      lc.append(sw.row, th);
      if (locked("compression.enabled") || locked("compression.threshold")) lc.append(lockLine());
      p.append(lc);

      // Time zone
      const tz = el("div", "field");
      tz.append(el("span", "", "Time zone"));
      const browser = Intl.DateTimeFormat().resolvedOptions().timeZone;
      const all = typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : [];
      const sel = el("select", "f");
      sel.setAttribute("aria-label", "Time zone");
      const list = zones(browser, all);
      if (st.timezone && !list.includes(st.timezone)) list.unshift(st.timezone);
      for (const z of list) {
        const o = el("option", "", z === browser ? z + " (this browser)" : z);
        o.value = z;
        o.selected = z === (st.timezone || browser);
        sel.append(o);
      }
      sel.disabled = locked("timezone");
      const row = el("div", "row2");
      row.append(sel);
      if (!locked("timezone")) row.append(btn("primary", "Save", async () => {
        note(tz);
        try { await api("PUT", base() + "/settings", { timezone: sel.value }); note(tz, say("good", "Saved. Scheduled jobs and the agent's clock use it.")); } catch (e) { note(tz, say("bad", "Not saved.", [e.message])); }
      }));
      tz.append(row, el("small", "", st.timezone ? "For scheduled jobs and the agent's clock." : "For scheduled jobs and the agent's clock. Not picked yet: the server's own zone applies."));
      if (locked("timezone")) tz.append(lockLine());
      p.append(tz);
    }

    // ---- Settings > Skills and tools ----
    let toolsTab = "skills";
    async function toolsPane(p) {
      head(p, "Skills and tools", "", true, () => toolsPane(p));
      p.append(seg([["skills", "Skills"], ["tools", "Tools"], ["connections", "Connections"]], toolsTab, "Part", (v) => { toolsTab = v; toolsPane(p); }));
      const box = el("div", "tpart");
      p.append(box);
      if (toolsTab === "skills") return skillsPart(box);
      if (toolsTab === "tools") return toolsetsPart(box);
      return connectionsPart(box);
    }

    function result(box, el2) {
      box.querySelector(":scope > .say")?.remove();
      if (el2) box.append(el2);
    }

    async function skillsPart(box) {
      box.append(el("p", "hint", "Skills are instructions the agent loads when a task needs them. Off: the agent does not see it."));
      const add = btn("", "Add a skill", () => { add.hidden = true; box.insertBefore(addSkill(box), list); }, "plus");
      const filter = el("input");
      filter.type = "search";
      filter.placeholder = "Find a skill";
      filter.setAttribute("aria-label", "Find a skill");
      const acts = el("div", "acts");
      acts.append(add, filter);
      const list = el("div", "lst");
      box.append(acts, list);
      let skills;
      try {
        skills = await api("GET", base() + "/skills");
      } catch (e) {
        list.replaceWith(say("bad", "Could not load the skills.", [e.message]));
        return;
      }
      const draw = () => {
        list.replaceChildren();
        for (const s of splitSkills(skills, filter.value)) {
          const r = el("div", "");
          const sn = s.label || s.name;
          const tx = el("span", "tx");
          tx.append(el("b", "", sn), el("span", "", (s.description || "") + (origin[s.provenance] ? " · " + origin[s.provenance] : "")));
          const t = toggle(s.enabled, false, sn);
          t.onchange = async () => {
            t.disabled = true;
            try {
              const a = await api("PUT", base() + "/skills/" + enc(s.name), { enabled: t.checked });
              s.enabled = t.checked;
              result(box, say("good", liveLine(sn, t.checked, a.applied_live)));
            } catch (e) { t.checked = !t.checked; result(box, say("bad", sn + " was not changed.", [e.message])); }
            t.disabled = false;
          };
          r.append(tx);
          if (s.provenance === "hub") r.append(btn("ghost ib", "", async () => {
            result(box, say("warn", "Removing " + sn + "...", []));
            try {
              const a = await api("DELETE", base() + "/skills/" + enc(s.name));
              if (!a.ok) throw new Error((a.lines || []).slice(-1)[0] || "The agent could not remove it.");
              skills = skills.filter((x) => x !== s);
              draw();
              result(box, say("good", sn + " is removed." + (a.applied_live ? " Open chats lose it from their next message." : "")));
            } catch (e) { result(box, say("bad", sn + " was not removed.", [e.message])); }
          }, "trash"));
          r.lastChild.setAttribute?.("aria-label", "Remove " + sn);
          r.append(t);
          list.append(r);
        }
        if (!list.children.length) list.append(el("p", "none", filter.value ? "No skill matches." : "No skills yet."));
      };
      filter.oninput = draw;
      draw();
    }

    function addSkill(box) {
      const f = el("form", "say");
      const body = el("div", "");
      const q = el("input");
      q.type = "search";
      q.placeholder = "What should the skill do? For example: weather";
      q.setAttribute("aria-label", "Search the skill catalog");
      const row = el("div", "row2");
      row.append(q, btn("primary", "Search", null));
      row.lastChild.type = "submit";
      const out = el("div", "lst");
      const head = el("p");
      head.append(el("b", "", "Add a skill"));
      body.append(head, row, out);
      f.append(body);
      f.onsubmit = async (e) => {
        e.preventDefault();
        out.replaceChildren(waitLine("Searching"));
        try {
          const r = await api("GET", base() + "/catalog?q=" + enc(q.value));
          out.replaceChildren();
          for (const m of r.results || []) {
            const d = el("div", "");
            const tx = el("span", "tx");
            tx.append(el("b", "", m.name), el("span", "", (m.description || "") + " · " + (m.trust_level === "builtin" ? "Official catalog" : m.trust_level === "trusted" ? "Trusted source" : "Community")));
            d.append(tx, btn("", "Check", () => check(d, m)));
            out.append(d);
          }
          if (!out.children.length) out.append(el("p", "none", "Nothing found."));
        } catch (err) { out.replaceChildren(say("bad", "The search failed.", [err.message])); }
      };
      async function check(d, m) {
        d.querySelector(".say")?.remove();
        const w = waitLine("Checking");
        d.append(w);
        try {
          const r = await api("GET", base() + "/catalog/check?identifier=" + enc(m.identifier));
          const s = r.scan || {};
          w.remove();
          if (s.policy === "block") {
            d.append(say("bad", "Not added: the check found a problem.", [s.policy_reason || s.summary || ""]));
            return;
          }
          const addBtn = btn("primary", "Add", async () => {
            addBtn.disabled = true;
            addBtn.replaceWith(waitLine("Adding"));
            try {
              const a = await api("POST", base() + "/catalog/install", { identifier: m.identifier });
              if (!a.ok) throw new Error((a.lines || []).slice(-1)[0] || "The agent could not add it.");
              toolsTab = "skills";
              const pane = box.closest(".set-pane");
              await toolsPane(pane);
              pane.append(say("good", m.name + " is added." + (a.applied_live ? " Open chats can use it from their next message." : " New chats can use it.")));
            } catch (err) { d.append(say("bad", m.name + " was not added.", [err.message])); }
          });
          d.append(say(s.policy === "allow" ? "good" : "warn", s.policy === "allow" ? "Checked: nothing risky found." : "Checked: look at this before you add it.",
            [s.summary || "", s.policy === "ask" ? s.policy_reason || "" : ""], [addBtn]));
        } catch (err) { w.replaceWith(say("bad", "Could not check it.", [err.message])); }
      }
      return f;
    }

    async function toolsetsPart(box) {
      box.append(el("p", "hint", "Groups of things the agent can do. Off: no chat can use them."));
      const list = el("div", "lst");
      box.append(list);
      let tools;
      try { tools = await api("GET", base() + "/tools"); } catch (e) { list.replaceWith(say("bad", "Could not load the tools.", [e.message])); return; }
      for (const t of tools) {
        const r = el("div", "");
        const tx = el("span", "tx");
        tx.append(el("b", "", t.label || t.name), el("span", "", t.always ? "Always on: runs on your computers or the sandbox" : !t.configured && t.enabled ? "Needs a key" : t.description || ""));
        const tg = toggle(t.enabled, t.always, t.label || t.name);
        tg.onchange = async () => {
          tg.disabled = true;
          try {
            const a = await api("PUT", base() + "/tools/" + enc(t.name), { enabled: tg.checked });
            result(box, say("good", liveLine(t.label || t.name, tg.checked, a.applied_live)));
          } catch (e) { tg.checked = !tg.checked; result(box, say("bad", (t.label || t.name) + " was not changed.", [e.message])); }
          tg.disabled = false;
        };
        r.append(tx);
        if (!t.always && !t.configured) r.append(btn("", "Add key", () => keyForm(r, t)));
        r.append(tg);
        list.append(r);
      }
    }

    async function keyForm(row, t) {
      row.querySelector("form")?.remove();
      const f = el("form", "say");
      const body = el("div", "");
      f.append(body);
      row.append(f);
      let cfg;
      try { cfg = await api("GET", base() + "/tools/" + enc(t.name) + "/keys"); } catch (e) { body.append(el("p", "err", e.message)); return; }
      const inputs = {};
      for (const pr of cfg.providers || []) {
        for (const v of pr.env_vars || []) {
          if (inputs[v.key]) continue;
          const i = el("input");
          i.type = "password";
          i.autocomplete = "off";
          i.placeholder = v.is_set ? "Saved. Enter a new one to replace it." : "";
          inputs[v.key] = i;
          const lab = el("label", "field");
          lab.append(el("span", "", pr.name + ": " + (v.prompt || v.key)), i);
          if (v.url) {
            const a = el("a", "", "Get a key");
            a.href = v.url;
            a.target = "_blank";
            a.rel = "noopener";
            lab.append(a);
          }
          body.append(lab);
        }
      }
      if (!Object.keys(inputs).length) { body.append(el("p", "", "This tool takes no key here.")); return; }
      const save = btn("primary", "Save", null);
      save.type = "submit";
      body.append(save);
      f.onsubmit = async (e) => {
        e.preventDefault();
        const env = {};
        for (const [k, i] of Object.entries(inputs)) if (i.value.trim()) env[k] = i.value.trim();
        try {
          const a = await api("PUT", base() + "/tools/" + enc(t.name) + "/keys", { env });
          for (const i of Object.values(inputs)) i.value = "";
          f.replaceWith(say("good", "Key saved." + (a.applied_live ? " Open chats can use it from their next message." : "")));
        } catch (err) { body.append(el("p", "err", err.message)); }
      };
    }

    async function connectionsPart(box) {
      box.append(el("p", "hint", "Other services the agent can use, such as a calendar or a database. Each adds its own tools."));
      const list = el("div", "lst");
      const acts = el("div", "acts");
      const add = btn("", "Add a connection", () => { add.hidden = true; acts.after(addConnection(box)); }, "plus");
      acts.append(add);
      box.append(list, acts);
      let conns;
      try { conns = await api("GET", base() + "/connections"); } catch (e) { list.replaceWith(say("bad", "Could not load the connections.", [e.message])); return; }
      if (!conns.length) { list.replaceWith(el("p", "none", "No connections yet.")); return; }
      for (const c of conns) {
        const r = el("div", "");
        const dot = el("span", "dot " + (c.enabled ? "" : "off"));
        const tx = el("span", "tx");
        const sub = el("span", "", c.enabled ? (c.url || c.command || "") : "Off");
        const cn = c.label || c.name;
        tx.append(el("b", "", cn), sub);
        const tg = toggle(c.enabled, c.plugin, cn);
        tg.onchange = async () => {
          tg.disabled = true;
          result(box, say("warn", (tg.checked ? "Connecting " : "Disconnecting ") + cn + "...", []));
          try {
            const a = await api("PUT", base() + "/connections/" + enc(c.name), { enabled: tg.checked });
            dot.className = "dot " + (tg.checked ? "" : "off");
            result(box, say("good", liveLine(cn, tg.checked, a.applied_live)));
          } catch (e) { tg.checked = !tg.checked; result(box, say("bad", cn + " was not changed.", [e.message])); }
          tg.disabled = false;
        };
        r.append(dot, tx, btn("", "Test", async () => {
          sub.textContent = "Testing...";
          try {
            const a = await api("POST", base() + "/connections/" + enc(c.name) + "/test");
            sub.textContent = a.ok ? "Connected, " + (a.tools || []).length + " tools" : a.error || "Could not connect.";
          } catch (e) { sub.textContent = e.message; }
        }));
        if (!c.plugin) r.append(btn("ghost ib", "", async () => {
          try {
            const a = await api("DELETE", base() + "/connections/" + enc(c.name));
            r.remove();
            result(box, say("good", cn + " is removed." + (a.applied_live ? " Open chats lose its tools from their next message." : "")));
          } catch (e) { result(box, say("bad", cn + " was not removed.", [e.message])); }
        }, "trash"));
        r.lastChild.setAttribute("aria-label", "Remove " + cn);
        r.append(tg);
        list.append(r);
      }
    }

    function addConnection(box) {
      const f = el("form", "say");
      const body = el("div", "");
      const name = el("input");
      name.placeholder = "home-calendar";
      const url = el("input");
      url.placeholder = "https://calendar.example.com/mcp";
      const tok = el("input");
      tok.type = "password";
      tok.autocomplete = "off";
      const lab = (t, i, s) => { const l = el("label", "field"); l.append(el("span", "", t), i); if (s) l.append(el("small", "", s)); return l; };
      const save = btn("primary", "Add", null);
      save.type = "submit";
      const head = el("p");
      head.append(el("b", "", "Add a connection"));
      body.append(head, lab("Name", name), lab("Address", url), lab("Token (optional)", tok, "Only if the service gave you one. Kept by the agent on the server."), save);
      f.append(body);
      f.onsubmit = async (e) => {
        e.preventDefault();
        save.disabled = true;
        body.querySelector(".say")?.remove();
        try {
          const a = await api("POST", base() + "/connections", { name: name.value.trim(), url: url.value.trim(), token: tok.value });
          tok.value = "";
          const pane = box.closest(".set-pane");
          await toolsPane(pane);
          pane.append(say("good", name.value.trim() + " is added." + (a.applied_live ? " Open chats can use it from their next message." : "")));
        } catch (err) { body.append(say("bad", "Could not add it.", [err.message])); save.disabled = false; }
      };
      return f;
    }

    // ---- Settings > Diagnostics ----
    let logTimer = 0;
    let logLevel = "";
    async function diagnosticsPane(p) {
      clearInterval(logTimer);
      head(p, "Diagnostics", "Whether each part of Talaria is working. Only admins see this.", true, () => diagnosticsPane(p));
      const parts = el("div", "lst");
      parts.append(waitLine("Checking"));
      p.append(parts);
      let info = null;
      try {
        info = await api("GET", base() + "/diagnostics");
        parts.replaceChildren();
        for (const x of info.parts || []) {
          const r = el("div", "");
          const tx = el("span", "tx");
          tx.append(el("b", "", x.name), el("span", "", x.text));
          r.append(el("span", "dot " + (x.status === "ok" ? "" : x.status)), tx);
          parts.append(r);
        }
      } catch (e) { parts.replaceChildren(say("bad", "Could not check.", [e.message])); }
      const hc = el("div", "");
      const acts = el("div", "acts");
      const run = btn("", "Run a health check", async () => {
        run.disabled = true;
        hc.replaceChildren(waitLine("Running the health check. It takes up to a minute."));
        try {
          const r = await api("POST", base() + "/healthcheck");
          const s = doctorSummary(r.lines);
          const lines = s.look.slice(0, 8);
          const pre = el("pre", "logbox");
          pre.textContent = reportText(r.lines);
          const det = el("details", "adv");
          det.append(el("summary", "", "Full report"), pre);
          hc.replaceChildren(say(s.look.length ? "warn" : "good", "Health check: " + s.passed + " passed" + (s.look.length ? ", " + s.look.length + " to look at" : ""), [...lines, s.optional ? el("small", "muted", optionalText(s.optional)) : null, det]));
        } catch (e) { hc.replaceChildren(say("bad", "The health check did not finish.", [e.message])); }
        run.disabled = false;
      }, "pulse");
      const copy = btn("", "Copy support info", async () => {
        const text = ["Talaria support info", ...((info && info.parts) || []).map((x) => x.name + ": " + x.text), "", ...logBox.textContent.split("\n").slice(-60)].join("\n");
        try { await navigator.clipboard.writeText(text); copy.lastChild.textContent = "Copied"; } catch (_) { copy.lastChild.textContent = "Could not copy"; }
        setTimeout(() => { copy.lastChild.textContent = "Copy support info"; }, 2000);
      }, "copy");
      acts.append(run, copy);
      p.append(acts, hc);
      const lh = el("div", "field");
      lh.append(el("span", "", "Agent log, newest last"));
      const logBox = el("pre", "logbox");
      logBox.setAttribute("aria-live", "off");
      // note shows a message in the box instead of lines.
      const note = (text, bad) => {
        logBox.className = "logbox note" + (bad ? " bad" : "");
        logBox.replaceChildren(...(text.nodeType ? [text] : [document.createTextNode(text)]));
      };
      const noLines = () => logLevel === "ERROR" ? "No errors in the log." : logLevel === "WARNING" ? "No warnings in the log." : "The agent has written no log lines yet.";
      // load reads the log. quiet keeps the lines shown while the refresh runs.
      const load = async (quiet) => {
        if (!quiet) note(waitLine("Loading the log"));
        try {
          const r = await api("GET", base() + "/log?lines=200&level=" + enc(logLevel));
          if (!r.lines || !r.lines.length) { note(noLines()); return; }
          const atEnd = !logBox.scrollTop || logBox.scrollTop + logBox.clientHeight >= logBox.scrollHeight - 4 || logBox.classList.contains("note");
          logBox.className = "logbox";
          logBox.replaceChildren(...r.lines.map((l) => el("span", logTone(l), l.replace(/\n$/, "") + "\n")));
          if (atEnd) logBox.scrollTop = logBox.scrollHeight;
        } catch (e) { note("Could not read the log: " + e.message, true); }
      };
      lh.append(seg([["", "Everything"], ["WARNING", "Warnings"], ["ERROR", "Errors"]], logLevel, "Log lines", (v) => { logLevel = v; load(false); }), logBox,
        el("small", "", "The last 200 lines. It updates every 10 seconds while open."));
      p.append(lh);
      await load(false);
      logTimer = setInterval(() => { if (p.isConnected && !p.hidden && !document.hidden) load(true); else if (!p.isConnected || p.hidden) clearInterval(logTimer); }, 10000);
    }

    // ---- Settings > Backup ----
    async function backupPane(p) {
      head(p, "Backup", "One file with what you need to set this server up again.");
      const f = el("div", "field");
      f.append(el("span", "", "The file holds"));
      const ul = el("ul", "plain");
      for (const t of ["Accounts, paired computers, Telegram links and saved prompts (the hub's data)", "The hub's keys and the Telegram token",
        "Chats, memory, skills, agent settings and model keys (the agent's data)"]) ul.append(el("li", "", t));
      f.append(ul, el("small", "", "Not in it: files on your computers and the sandbox's work folder."));
      p.append(f, say("warn", "The file holds keys. Keep it somewhere private.", []));
      const out = el("div", "");
      const go = btn("primary", "Download backup", async () => {
        go.disabled = true;
        out.replaceChildren(waitLine("Packing the agent's data. A large one takes a few minutes."));
        try {
          const r = await api("POST", "/api/admin/backup", { backend: backend() });
          out.replaceChildren(say("good", "Backup ready. The download starts now.", []));
          const a = document.createElement("a");
          a.href = "/api/admin/backup/" + enc(r.token);
          a.download = "";
          document.body.append(a);
          a.click();
          a.remove();
        } catch (e) { out.replaceChildren(say("bad", "Backup not finished: " + e.message, ["Nothing was downloaded. Try again in a moment."])); }
        go.disabled = false;
      }, "download");
      const acts = el("div", "acts");
      acts.append(go);
      const docs = el("a", "", "How to restore");
      docs.href = "https://usa-reddragon.github.io/talaria/deploy/upgrades.html#back-up";
      docs.target = "_blank";
      docs.rel = "noopener";
      acts.append(docs);
      p.append(acts, out, el("p", "hint", "To restore, start a new install, then follow the steps in the file's README.txt."));
    }

    // ---- Settings > Telegram, admin part ----
    async function telegramAdmin(p) {
      const box = el("div", "tgadmin");
      p.append(box);
      let st;
      try { st = await api("GET", "/api/admin/telegram"); } catch (e) { box.append(say("bad", "Could not read the bot's state.", [e.message])); return; }
      const redraw = () => { box.remove(); telegramAdmin(p); };
      if (st.running || st.install) {
        const r = el("div", "lst");
        const row = el("div", "");
        const tx = el("span", "tx");
        tx.append(el("b", "", st.bot || "The bot"), el("span", "", st.running ? "Running" : "Not running"));
        row.append(el("span", "dot " + (st.running ? "" : "bad")), tx);
        r.append(row);
        box.append(r);
        if (st.install) { box.append(lockLine()); return; }
        const acts = el("div", "acts");
        acts.append(btn("", "Change token", () => { box.replaceChildren(tokenForm(box, redraw)); }),
          btn("danger-text", "Stop", async () => { try { await api("DELETE", "/api/admin/telegram"); redraw(); ctx.refreshMe?.(); } catch (e) { box.append(say("bad", "Not stopped.", [e.message])); } }));
        box.append(acts);
        if (st.error) box.append(say("bad", st.error, []));
        return;
      }
      if (st.error) box.append(say("bad", st.error, []));
      box.append(tokenForm(box, redraw));
    }

    function tokenForm(box, done) {
      const f = el("form", "field");
      const steps = el("ol", "plain");
      for (const t of ["In Telegram, open @BotFather and send /newbot.", "Pick a name and a user name ending in bot.", "Copy the token it sends and paste it here."]) steps.append(el("li", "", t));
      const i = el("input");
      i.type = "password";
      i.autocomplete = "off";
      i.setAttribute("aria-label", "Bot token");
      const pw = el("div", "pw");
      const show = btn("", "Show", () => { i.type = i.type === "password" ? "text" : "password"; show.textContent = i.type === "password" ? "Show" : "Hide"; });
      pw.append(i, show);
      const out = el("div", "");
      const acts = el("div", "acts");
      const test = btn("", "Test", async () => {
        out.replaceChildren(waitLine("Asking Telegram"));
        try { const r = await api("POST", "/api/admin/telegram/test", { token: i.value }); out.replaceChildren(say("good", "Works: this is " + r.bot + ".", [])); } catch (e) { out.replaceChildren(say("bad", e.message, [])); }
      });
      const save = btn("primary", "Save and start", null);
      save.type = "submit";
      acts.append(test, save);
      f.append(el("span", "", "Make a bot (2 minutes)"), steps, el("span", "", "Bot token"), pw, el("small", "", "Kept by the hub on this server. Nobody can read it back here."), acts, out);
      f.onsubmit = async (e) => {
        e.preventDefault();
        save.disabled = true;
        out.replaceChildren(waitLine("Starting the bot"));
        try { await api("PUT", "/api/admin/telegram", { token: i.value }); i.value = ""; ctx.refreshMe?.(); done(); } catch (err) { out.replaceChildren(say("bad", err.message, [])); save.disabled = false; }
      };
      return f;
    }

    // ---- Update banner (admins only) ----
    async function updateBanner(box) {
      const me = ctx.me();
      box.hidden = true;
      if (!me.admin) return;
      let st;
      try { st = await api("GET", "/api/admin/updates"); } catch (_) { return; }
      const text = bannerText(me, st);
      const key = "talaria.update.dismissed";
      const id = text && (st.latest ? st.latest.version : (st.agent || [])[0]?.latest || (st.computers || [])[0]?.latest);
      if (!text || localStorage.getItem(key) === id) return;
      box.replaceChildren();
      const how = btn("", "How to update", () => updateDialog(st));
      const x = btn("close", "\u00d7", () => { localStorage.setItem(key, id); box.hidden = true; });
      x.setAttribute("aria-label", "Dismiss");
      box.append(el("span", "", text), how, x);
      box.hidden = false;
    }

    function updateDialog(st) {
      const d = document.getElementById("picker");
      d.replaceChildren();
      d.className = "picker connect";
      const h = el("h2", "", "Update Talaria");
      const body = el("div", "pbody");
      const rows = el("div", "lst");
      const line = (name, text) => { const r = el("div", ""); const tx = el("span", "tx"); tx.append(el("b", "", name), el("span", "", text)); r.append(tx); rows.append(r); };
      line("Hub", st.current + " now" + (st.latest ? ", " + st.latest.version + " available" : ", up to date"));
      for (const a of st.agent || []) line("Agent (" + a.backend + ")", a.running + " now, " + a.latest + " available");
      for (const c of st.computers || []) line(c.name, "runs talaria " + c.version + ". It updates itself when it next connects.");
      const [lead, cmd, after] = updateSteps(st.install);
      body.append(rows, el("p", "", lead));
      if (cmd) { const pre = el("pre", "cmd"); pre.textContent = cmd; body.append(pre); }
      if (after) body.append(el("p", "hint", after));
      body.append(el("p", "hint", "Running replies finish first."));
      if (st.latest && st.latest.url) { const a = el("a", "", "What changed"); a.href = st.latest.url; a.target = "_blank"; a.rel = "noopener"; body.append(a); }
      const foot = el("div", "pfoot");
      foot.append(btn("", "Close", () => d.close()));
      d.append(h, body, foot);
      d.showModal();
    }

    // ---- Quick panel in a chat ----
    // quick builds the panel for the chat view v ({ backend, session }) into
    // box; admin switches change the whole server, everyone can turn asking
    // off for their own chat.
    async function quick(box, v, onClose) {
      box.replaceChildren();
      const grab = el("span", "grab");
      const h = el("div", "qhead");
      const title = el("h2", "", "Tools, skills and safety");
      title.id = "qp-title";
      const close = btn("ghost ib", "", onClose, "close");
      close.setAttribute("aria-label", "Close");
      h.append(title, close);
      box.append(grab, h, waitLine("Loading"));
      let sum, yolo;
      try {
        [sum, yolo] = await Promise.all([api("GET", "/api/agent/" + enc(v.backend) + "/summary"),
          api("GET", "/api/sessions/" + enc(v.backend) + "/" + enc(v.session) + "/yolo").catch(() => ({ enabled: false }))]);
      } catch (e) {
        box.lastChild.replaceWith(say("bad", "Could not load them.", [e.message]));
        return;
      }
      box.lastChild.remove();
      const b = "/api/agent/" + enc(v.backend);
      const locked = (k) => (sum.locked || []).includes(k);
      const sec = (icon, name, count) => {
        const s = el("div", "qsec");
        const h3 = el("h3", "");
        if (icon) h3.append(I.icon(icon));
        h3.append(el("span", "grow", name));
        if (count) h3.append(el("span", "cnt", count));
        s.append(h3);
        box.append(s);
        return s;
      };
      const flash = (s, el2) => { s.querySelector(":scope > .say")?.remove(); if (el2) s.append(el2); };
      if (sum.admin) {
        const n = el("p", "qnote");
        n.append(I.icon("users"), "Applies to every chat on this server from its next message.");
        box.append(n);
      }
      // Safety
      const safe = sec("shield", "Before risky commands");
      if (sum.admin) {
        safe.append(seg([["manual", shortModes.manual], ["smart", shortModes.smart], ["off", shortModes.off]], sum.approvals_mode, "Before risky commands", async (m) => {
          flash(safe);
          try { await api("PUT", b + "/settings", { approvals_mode: m }); flash(safe, say("good", "Saved. Every chat uses it from its next command.")); } catch (e) { flash(safe, say("bad", "Not changed.", [e.message])); }
        }, locked("approvals.mode")));
        if (locked("approvals.mode")) safe.append(lockLine());
      } else {
        safe.append(el("p", "qnote", (modeLabel(sum.approvals_mode, false) || "The agent asks before risky commands") + ". An admin sets this for everyone."));
      }
      const ys = switchRow("Never ask in this chat", "Only this chat, until it ends." + (sum.admin ? " Others keep the setting above." : ""), yolo.enabled);
      ys.t.onchange = async () => {
        ys.t.disabled = true;
        flash(safe);
        try {
          const r = await api("PUT", "/api/sessions/" + enc(v.backend) + "/" + enc(v.session) + "/yolo", { enabled: ys.t.checked });
          ys.t.checked = r.enabled;
          if (r.enabled) flash(safe, say("warn", "This chat runs commands without asking. Other agents cannot send it messages while this is on.", []));
        } catch (e) { ys.t.checked = !ys.t.checked; flash(safe, say("bad", "Not changed.", [e.message])); }
        ys.t.disabled = false;
      };
      safe.append(ys.row);
      if (yolo.enabled) safe.append(say("warn", "This chat runs commands without asking. Other agents cannot send it messages while this is on.", []));

      if (!sum.admin) {
        const s = sec("", "On for every chat");
        const names = (xs, f) => xs.map(f).join(", ");
        s.append(el("p", "qnote", "Tools: " + (names(sum.tools, (t) => t.label || t.name) || "none") + "."));
        const sk = sum.skills.map((x) => x.label || x.name);
        s.append(el("p", "qnote", "Skills: " + (sk.length > 3 ? sk.slice(0, 3).join(", ") + " and " + (sk.length - 3) + " more" : sk.join(", ") || "none") + "."));
        s.append(el("p", "qnote", "Connections: " + (names(sum.connections, (c) => c.label || c.name) || "none") + "."));
        s.append(el("p", "qnote", "An admin of this server turns these on and off."));
        return;
      }
      const change = async (s, row, t, url, label, kind) => {
        t.disabled = true;
        const w = waitLine("Saving");
        t.before(w);
        flash(s);
        try {
          const r = await api("PUT", url, { enabled: t.checked });
          flash(s, say("good", liveLine(label, t.checked, r.applied_live)));
          if (kind === "conn") row.querySelector(".dot").className = "dot " + (t.checked ? "" : "off");
          count(s);
        } catch (e) { t.checked = !t.checked; flash(s, say("bad", label + " is still " + (t.checked ? "on" : "off") + ".", [e.message])); }
        w.remove();
        t.disabled = false;
      };
      const count = (s) => {
        const ts = [...s.querySelectorAll("input.toggle")];
        const c = s.querySelector(".cnt");
        if (c) c.textContent = ts.filter((x) => x.checked).length + " of " + ts.length + " on";
      };
      const limited = (s, rows, n, more) => {
        rows.forEach((r, i) => { r.hidden = i >= n; s.append(r); });
        if (rows.length > n) {
          const m = btn("link more", more(rows.length - n, rows.length), () => { rows.forEach((r) => { r.hidden = false; }); m.remove(); });
          s.append(m);
        }
      };
      // Tools
      const ts = sec("", "Tools", " ");
      limited(ts, sum.tools.map((t) => {
        const sw = switchRow(t.label || t.name, t.always ? "Always on: runs on your computers or the sandbox" : (!t.configured && t.enabled ? "Needs a key in Settings" : t.description || ""), t.enabled,
          { disabled: t.always || locked("tools." + t.name) });
        sw.t.onchange = () => change(ts, sw.row, sw.t, b + "/tools/" + enc(t.name), t.label || t.name);
        return sw.row;
      }), 6, (n) => "Show " + n + " more");
      count(ts);
      // Skills
      const ss = sec("", "Skills", " ");
      const find = el("input");
      find.type = "search";
      find.placeholder = "Find a skill";
      find.setAttribute("aria-label", "Find a skill");
      ss.append(find);
      const skillRows = splitSkills(sum.skills).map((k) => {
        const sw = switchRow(k.label || k.name, k.description || "", k.enabled, { disabled: locked("skills." + k.name) });
        sw.row.dataset.name = ((k.label || "") + " " + k.name + " " + (k.description || "")).toLowerCase();
        sw.t.onchange = () => change(ss, sw.row, sw.t, b + "/skills/" + enc(k.name), k.label || k.name);
        return sw.row;
      });
      limited(ss, skillRows, 5, (_, all) => "Show all " + all);
      find.oninput = () => {
        const q = find.value.toLowerCase();
        ss.querySelector(".more")?.remove();
        skillRows.forEach((r) => { r.hidden = !!q && !r.dataset.name.includes(q); });
      };
      count(ss);
      // Connections
      const cs = sec("", "Connections", sum.connections.length ? " " : "");
      if (!sum.connections.length) cs.append(el("p", "qnote", "None yet."));
      for (const c of sum.connections) {
        const sw = switchRow(c.label || c.name, c.enabled ? c.url || "On" : "Off", c.enabled, { dot: c.enabled ? "" : "off", disabled: c.plugin || locked("mcp_servers." + c.name) });
        sw.t.onchange = () => change(cs, sw.row, sw.t, b + "/connections/" + enc(c.name), c.label || c.name, "conn");
        cs.append(sw.row);
      }
      count(cs);
      const foot = el("div", "qfoot");
      foot.append(el("span", "grow", "Add or remove them in Settings."), btn("link", "Skills and tools", () => { onClose(); ctx.openSettings("tools"); }, "chevright"));
      box.append(foot);
    }

    return { agentPane, toolsPane, diagnosticsPane, backupPane, telegramAdmin, updateBanner, quick };
  }

  return { create, doctorSummary, reportText, optionalText, logTone, modeLabel, bannerText, updateSteps, liveLine, zones, splitSkills };
})();
