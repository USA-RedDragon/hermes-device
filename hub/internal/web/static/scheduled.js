"use strict";
// HubScheduled builds the Scheduled page: the Hermes cron jobs of the
// user's backends, their next runs and recent runs, and a form that makes
// or changes a job. Hermes reads every schedule; the page never parses
// one. Text from Hermes is only ever set with textContent.
window.HubScheduled = (ctx) => {
  const { api, el } = ctx;
  const root = document.getElementById("scheduled");
  const nav = document.getElementById("schednav");
  const count = document.getElementById("schedcount");
  const week = 7 * 24 * 3600 * 1000;
  const results = [["hub", "This hub only"], ["general", "Telegram, General topic"], ["topic", "Telegram, its own topic"]];
  const bad = "Talaria can't read that schedule. Try \"every monday 9am\" or cron like \"0 9 * * 1\".";

  let data = { zone: "", jobs: [], errors: {} };
  let backends = [];
  let shown = false;
  let loaded = false;
  let filter = "all";
  const open = new Set();
  let confirmDel = "";
  let editing = "";
  let form = null;
  let options = {};
  let timer = 0;
  let fast = 0;

  const key = (j) => j.backend_id + "/" + j.id;
  const byKey = (k) => data.jobs.find((j) => key(j) === k);

  function toast(text) {
    const x = document.getElementById("toast");
    x.textContent = text;
    x.hidden = false;
    clearTimeout(toast.h);
    toast.h = setTimeout(() => { x.hidden = true; }, 2200);
  }

  function badge() {
    nav.hidden = false;
    count.textContent = loaded ? String(data.jobs.length) : "";
    nav.classList.toggle("on", shown);
  }

  async function refresh() {
    try {
      data = await api("GET", "/api/scheduled");
      loaded = true;
    } catch (_) {
      return;
    }
    badge();
    if (shown && !editing) draw();
    schedule();
  }

  // schedule polls faster while a job runs, or just after Run now.
  function schedule() {
    clearTimeout(timer);
    if (!shown) return;
    const busy = fast > Date.now() || data.jobs.some((j) => j.state === "run");
    timer = setTimeout(() => { if (!document.hidden) refresh(); else schedule(); }, busy ? 4000 : 30000);
  }

  async function show() {
    shown = true;
    root.hidden = false;
    badge();
    if (!backends.length) {
      try { backends = await api("GET", "/api/backends"); } catch (_) { backends = []; }
    }
    if (!shown) return;
    draw();
    await refresh();
  }

  function hide() {
    if (form && form.draft) discard(form);
    shown = false;
    editing = "";
    form = null;
    root.hidden = true;
    clearTimeout(timer);
    nav.classList.remove("on");
  }

  function path(b, p) { return "/api/scheduled/" + encodeURIComponent(b) + p; }
  function jobPath(j, p) { return path(j.backend_id, "/jobs/" + encodeURIComponent(j.id) + (p || "")); }

  function replace(j) {
    const i = data.jobs.findIndex((x) => key(x) === key(j));
    if (i >= 0) data.jobs[i] = j; else data.jobs.unshift(j);
  }

  function draw() {
    const scroll = root.querySelector(".sched-scroll");
    const top = scroll ? scroll.scrollTop : 0;
    const act = document.activeElement;
    const fid = act && root.contains(act) && act.id ? act.id : "";
    const sel = fid && typeof act.selectionStart === "number" ? [act.selectionStart, act.selectionEnd] : null;
    root.replaceChildren();
    const s = el("div", "sched-scroll");
    const w = el("div", "sched-wrap");
    const head = el("div", "sched-head");
    const h1 = el("h1", "", "Scheduled");
    const nb = el("button", "primary", "New job");
    nb.type = "button";
    nb.onclick = () => startNew();
    head.append(h1, el("span", "tz", data.zone ? "Times in " + data.zone : ""), nb);
    w.append(head);
    for (const [b, msg] of Object.entries(data.errors || {})) w.append(el("p", "sched-err", msg || "Could not read the jobs of " + b));
    const f = el("div", "filters");
    f.setAttribute("role", "group");
    f.setAttribute("aria-label", "Filter jobs");
    for (const [id, label] of [["all", "All"], ["attn", "Needs attention"], ["active", "Active"], ["paused", "Paused"]]) {
      const c = el("button", "chip", label);
      c.type = "button";
      c.setAttribute("aria-pressed", String(filter === id));
      c.onclick = () => { filter = id; draw(); };
      f.append(c);
    }
    w.append(f, timeline());
    const list = el("div", "sched-list");
    if (editing === "new") list.append(newBox());
    const shownJobs = data.jobs.filter((j) => filter === "all" || (filter === "attn" && (j.state === "bad" || j.state === "warn")) ||
      (filter === "active" && j.state !== "paused") || (filter === "paused" && j.state === "paused"));
    for (const j of shownJobs) list.append(row(j));
    if (!shownJobs.length && editing !== "new") {
      list.append(el("div", "empty", !loaded ? "Loading..." : data.jobs.length ? "No jobs match this filter." : "No scheduled jobs yet. Ask in a chat, or make one with New job."));
    }
    w.append(list);
    s.append(w);
    root.append(s);
    s.scrollTop = top;
    const again = fid && document.getElementById(fid);
    if (again) {
      again.focus({ preventScroll: true });
      if (sel) try { again.setSelectionRange(sel[0], sel[1]); } catch (_) { /* not a text field */ }
    }
  }

  function many() { return new Set(data.jobs.map((j) => j.backend_id)).size > 1 || backends.length > 1; }

  function timeline() {
    const sec = el("section", "upnext");
    const h = el("h2", "", "Next 7 days");
    h.id = "upnext-h";
    sec.setAttribute("aria-labelledby", "upnext-h");
    const t = el("div", "timeline");
    const now = Date.now();
    const up = data.jobs.filter((j) => j.state !== "paused" && j.next_at && Date.parse(j.next_at) - now < week)
      .sort((a, b) => Date.parse(a.next_at) - Date.parse(b.next_at));
    for (const j of up) {
      const r = el("div", "tl");
      r.append(el("span", "when", j.next), el("span", "", j.name + (many() ? " · " + j.backend : "")));
      t.append(r);
    }
    if (!up.length) t.append(el("div", "empty", loaded ? "Nothing scheduled in the next 7 days." : ""));
    sec.append(h, t);
    return sec;
  }

  function pillClass(j) {
    if (j.state === "bad") return "bad";
    if (j.state === "warn") return "warn";
    if (j.state === "run") return "run";
    return "paused";
  }

  function row(j) {
    const k = key(j);
    const isOpen = open.has(k);
    const box = el("div", "job" + (isOpen ? " open" : ""));
    const h = el("div", "job-head");
    h.tabIndex = 0;
    h.setAttribute("role", "button");
    h.setAttribute("aria-expanded", String(isOpen));
    h.append(el("span", "st " + j.state), el("span", "name", j.name));
    const nx = el("span", "next");
    if (j.state === "paused") nx.append(j.flag === "Done" ? "Done" : "Paused");
    else if (j.state === "run") nx.append("Running now");
    else if (j.next) nx.append(el("b", "", j.next_rel), " · " + j.next);
    h.append(nx);
    const m = el("div", "meta");
    m.append(el("span", "", j.schedule), el("span", "", j.runs_in));
    if (many()) m.append(el("span", "", j.backend));
    if (j.flag && j.state !== "ok") m.append(el("span", "pill " + pillClass(j), j.flag));
    if (j.last) m.append(el("span", "", "Last: " + j.last));
    h.append(m);
    const flip = () => {
      if (open.has(k)) open.delete(k); else { open.add(k); loadRuns(j); }
      if (editing === k) closeForm();
      confirmDel = "";
      draw();
    };
    h.onclick = flip;
    h.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } };
    box.append(h);
    if (isOpen) box.append(editing === k ? editBox(j) : body(j));
    return box;
  }

  async function loadRuns(j) {
    try {
      const runs = await api("GET", jobPath(j, "/runs"));
      const cur = byKey(key(j));
      if (cur) cur.runs = runs;
      if (shown && !editing) draw();
    } catch (_) { /* the list's runs stay */ }
  }

  function body(j) {
    const b = el("div", "job-body");
    const kv = el("dl", "kv");
    const add = (k, v) => kv.append(el("dt", "", k), el("dd", "", v));
    add("Schedule", j.schedule + (j.expr ? "  (" + j.expr + ")" : ""));
    add("Runs in", j.runs_in);
    add("Model", j.model_label);
    if (j.skills && j.skills.length) add("Skills", j.skills.join(", "));
    add("Results go to", j.results_text);
    b.append(kv);
    if (j.prompt) b.append(el("div", "prompt", j.prompt));
    if (j.locked) b.append(el("p", "hint", "This job runs a script or other code on the Hermes server. Change it there; here you can pause, stop or delete it."));
    const runs = el("div", "runs");
    runs.append(el("h3", "", "Recent runs"));
    if (!j.runs || !j.runs.length) runs.append(el("div", "empty", "Hasn't run yet."));
    else {
      const w = el("div", "tablewrap");
      const t = el("table");
      for (const r of j.runs) {
        const tr = el("tr");
        tr.append(el("td", "", r.started), el("td", "", r.running ? "" : r.duration), el("td", "res" + (r.failed ? " bad" : ""), r.result));
        const td = el("td");
        if (r.session_id) {
          const a = el("a", "", "Open chat");
          a.href = ctx.chatHash(j.backend_id, r.session_id);
          td.append(a);
        }
        tr.append(td);
        t.append(tr);
      }
      w.append(t);
      runs.append(w);
    }
    b.append(runs);
    const a = el("div", "actions");
    const run = el("button", "", j.state === "run" ? "Cancel run" : "Run now");
    run.type = "button";
    run.disabled = j.locked && j.state !== "run";
    run.onclick = () => (j.state === "run" ? cancelRun(j, run) : runNow(j, run));
    const pause = el("button", "", j.state === "paused" && j.flag !== "Done" ? "Resume" : "Pause");
    pause.type = "button";
    pause.disabled = (j.locked && pause.textContent === "Resume") || j.flag === "Done";
    pause.onclick = () => toggle(j, pause);
    const ed = el("button", "", "Edit");
    ed.type = "button";
    ed.disabled = j.locked;
    ed.onclick = () => startEdit(j);
    a.append(run, pause, ed, el("span", "grow"));
    if (confirmDel === key(j)) {
      const c = el("div", "confirm");
      c.append(el("span", "", "Delete \"" + j.name + "\"? Its past run chats stay."));
      const n = el("button", "", "Keep");
      n.type = "button";
      n.onclick = () => { confirmDel = ""; draw(); };
      const y = el("button", "del solid", "Delete");
      y.type = "button";
      y.onclick = () => remove(j, y);
      c.append(n, y);
      a.append(c);
    } else {
      const d = el("button", "del", "Delete");
      d.type = "button";
      d.onclick = () => { confirmDel = key(j); draw(); };
      a.append(d);
    }
    b.append(a);
    return b;
  }

  async function act(btn, fn) {
    btn.disabled = true;
    try {
      return await fn();
    } catch (e) {
      toast(e.message);
      btn.disabled = false;
      return null;
    }
  }

  async function runNow(j, btn) {
    const ok = await act(btn, () => api("POST", jobPath(j, "/run")));
    if (!ok) return;
    j.state = "run";
    fast = Date.now() + 60000;
    toast("Started. The result goes to " + j.results_text);
    draw();
    schedule();
  }

  async function cancelRun(j, btn) {
    const r = await act(btn, () => api("POST", jobPath(j, "/cancel")));
    if (!r) return;
    toast(r.cancelled ? "Run cancelled; schedule unchanged" : "Nothing was running");
    fast = Date.now() + 20000;
    refresh();
  }

  async function toggle(j, btn) {
    const resume = j.state === "paused";
    const r = await act(btn, () => api("POST", jobPath(j, resume ? "/resume" : "/pause")));
    if (!r) return;
    replace(r);
    toast(resume ? "Resumed" : "Paused");
    draw();
  }

  async function remove(j, btn) {
    const r = await act(btn, () => api("DELETE", jobPath(j)));
    if (!r) return;
    data.jobs = data.jobs.filter((x) => key(x) !== key(j));
    open.delete(key(j));
    confirmDel = "";
    badge();
    toast("Deleted");
    draw();
  }

  // The form. form holds its values, the backend, the preview draft and
  // the job it changes ("" for a new one).
  function startNew() {
    const b = ctx.defaultBackend() && backends.some((x) => x.id === ctx.defaultBackend()) ? ctx.defaultBackend() : (backends[0] || {}).id;
    if (!b) { toast("You have no Hermes to schedule on"); return; }
    if (form && form.draft) discard(form);
    editing = "new";
    form = { backend: b, job: null, name: "", schedule: "", prompt: "", model: "", results: "general", draft: "", preview: null, saving: false };
    filter = "all";
    confirmDel = "";
    loadOptions(b);
    draw();
    focus("f-name-new");
  }

  function startEdit(j) {
    if (form && form.draft) discard(form);
    editing = key(j);
    form = { backend: j.backend_id, job: j, name: j.name, schedule: j.schedule, prompt: j.prompt,
      model: j.model && j.model.model ? j.model.provider + "\n" + j.model.model : "", results: j.results, draft: "", preview: null, saving: false };
    confirmDel = "";
    loadOptions(j.backend_id);
    draw();
    focus("f-name-" + j.id);
  }

  function focus(id) { setTimeout(() => { const x = document.getElementById(id); if (x) x.focus(); }, 0); }

  function closeForm() {
    if (form && form.draft) discard(form);
    editing = "";
    form = null;
  }

  function discard(f) {
    const d = f.draft;
    f.draft = "";
    api("DELETE", path(f.backend, "/drafts/" + encodeURIComponent(d))).catch(() => {});
  }

  async function loadOptions(b) {
    if (options[b]) return;
    try {
      options[b] = await api("GET", path(b, "/options"));
      if (editing && form && form.backend === b) draw();
    } catch (_) { options[b] = { providers: [], default_model: "", runs_in: "" }; }
  }

  function newBox() {
    const box = el("div", "job open");
    box.append(editBox(null));
    return box;
  }

  function editBox(j) {
    const f = form;
    const id = j ? j.id : "new";
    const fe = el("form", "form job-body");
    const lab = (t, input) => { const l = el("label", "", t); l.append(input); return l; };
    if (!j && backends.length > 1) {
      const sel = el("select");
      sel.id = "f-backend-new";
      for (const b of backends) {
        const o = el("option", "", b.name || b.id);
        o.value = b.id;
        o.selected = b.id === f.backend;
        sel.append(o);
      }
      sel.onchange = () => {
        if (f.draft) discard(f);
        f.backend = sel.value;
        f.preview = null;
        loadOptions(f.backend);
        draw();
      };
      fe.append(lab("Hermes", sel));
    }
    const name = el("input");
    name.id = "f-name-" + id;
    name.value = f.name;
    name.maxLength = 200;
    name.oninput = () => { f.name = name.value; };
    const sched = el("input");
    sched.id = "f-sched-" + id;
    sched.value = f.schedule;
    sched.maxLength = 200;
    sched.placeholder = "every monday 9am, weekdays at 8:30am, in 2h, 0 9 * * *";
    sched.autocomplete = "off";
    const prev = el("div", "preview");
    prev.setAttribute("aria-live", "polite");
    drawPreview(prev, j);
    let t = 0;
    sched.oninput = () => {
      f.schedule = sched.value;
      clearTimeout(t);
      t = setTimeout(() => preview(prev, j), 600);
    };
    const opt = options[f.backend] || {};
    const where = el("select");
    where.id = "f-where-" + id;
    where.disabled = true;
    where.title = "A job gets a computer's Firefox from a chat: ask Talaria to use your browser for the job.";
    where.append(el("option", "", j ? j.runs_in : opt.runs_in || "Sandbox"));
    const model = el("select");
    model.id = "f-model-" + id;
    const follow = el("option", "", "Follows the chat model" + (opt.default_model ? " (" + opt.default_model + ")" : ""));
    follow.value = "";
    model.append(follow);
    let found = !f.model;
    for (const p of opt.providers || []) {
      for (const m of p.models) {
        const o = el("option", "", m.id + " · " + p.name + " (pinned)");
        o.value = p.id + "\n" + m.id;
        if (o.value === f.model) { o.selected = true; found = true; }
        model.append(o);
      }
    }
    if (!found && j) {
      const o = el("option", "", j.model_label);
      o.value = f.model;
      o.selected = true;
      model.append(o);
    }
    model.onchange = () => { f.model = model.value; };
    const deliver = el("select");
    deliver.id = "f-deliver-" + id;
    const choices = results.slice();
    if (j && !results.some(([v]) => v === j.results)) choices.unshift([j.results, j.results_text]);
    for (const [v, label] of choices) {
      const o = el("option", "", label);
      o.value = v;
      o.selected = v === f.results;
      deliver.append(o);
    }
    deliver.onchange = () => { f.results = deliver.value; };
    const prompt = el("textarea");
    prompt.id = "f-prompt-" + id;
    prompt.value = f.prompt;
    prompt.maxLength = 20000;
    prompt.placeholder = "What the agent should do each time. Write it so it makes sense on its own.";
    prompt.oninput = () => { f.prompt = prompt.value; };
    const r1 = el("div", "row2");
    r1.append(lab("Runs in", where), lab("Model", model));
    fe.append(lab("Name", name), lab("When", sched), prev, r1, lab("Results go to", deliver), lab("Instructions", prompt));
    const a = el("div", "actions");
    const cancel = el("button", "", "Cancel");
    cancel.type = "button";
    cancel.onclick = () => { closeForm(); draw(); };
    const save = el("button", "primary", j ? "Save" : "Schedule it");
    save.type = "submit";
    save.disabled = f.saving;
    a.append(el("span", "grow"), cancel, save);
    fe.append(a);
    fe.onsubmit = (e) => { e.preventDefault(); submit(j, save); };
    return fe;
  }

  function drawPreview(prev, j) {
    const p = form.preview;
    prev.className = "preview";
    if (!p) {
      if (j && j.schedule === form.schedule) prev.textContent = "Next: " + (j.next || "after you resume it");
      else prev.textContent = "";
      return;
    }
    if (!p.ok) {
      prev.className = "preview bad";
      prev.textContent = form.schedule.trim() ? bad : "";
      return;
    }
    const n = p.next || [];
    if (!n.length) prev.textContent = "Talaria reads this as " + p.display + (p.expr ? " (" + p.expr + ")" : "") + ". The next time shows after you save.";
    else prev.textContent = "Next: " + n[0] + (n[1] ? ", then " + n[1] : "") + (p.expr && p.expr !== p.display ? "  (" + p.expr + ")" : "");
  }

  async function preview(prev, j) {
    const f = form;
    if (!f) return;
    const text = f.schedule.trim();
    if (!text || (j && text === j.schedule)) { f.preview = null; drawPreview(prev, j); return; }
    try {
      const p = await api("POST", path(f.backend, "/preview"), { schedule: text, draft: f.draft });
      if (form !== f) { if (p.draft && p.draft !== f.draft) api("DELETE", path(f.backend, "/drafts/" + encodeURIComponent(p.draft))).catch(() => {}); return; }
      f.draft = p.draft || f.draft;
      if (f.schedule.trim() !== text) return;
      f.preview = p;
    } catch (e) {
      f.preview = { ok: false };
    }
    drawPreview(prev, j);
  }

  async function submit(j, btn) {
    const f = form;
    if (!f.name.trim() || !f.schedule.trim() || !f.prompt.trim()) {
      toast("A job needs a name, a time and instructions");
      return;
    }
    const [provider, model] = f.model ? f.model.split("\n") : ["", ""];
    const body = { draft: f.draft };
    if (!j || f.name !== j.name) body.name = f.name;
    if (!j || f.schedule.trim() !== j.schedule) body.schedule = f.schedule;
    if (!j || f.prompt !== j.prompt) body.prompt = f.prompt;
    if (!j || provider !== ((j.model && j.model.provider) || "") || model !== ((j.model && j.model.model) || "")) {
      body.model = { provider, model };
    }
    if (!j || f.results !== j.results) {
      if (results.some(([v]) => v === f.results)) body.results = f.results;
    }
    f.saving = true;
    btn.disabled = true;
    try {
      const out = j ? await api("PUT", jobPath(j), body) : await api("POST", path(f.backend, "/jobs"), body);
      f.draft = "";
      replace(out);
      open.add(key(out));
      editing = "";
      form = null;
      badge();
      toast(j ? "Saved" : "Scheduled");
      draw();
    } catch (e) {
      f.saving = false;
      btn.disabled = false;
      toast(e.message);
    }
  }

  return { show, hide, refresh };
};
