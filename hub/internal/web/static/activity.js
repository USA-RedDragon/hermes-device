"use strict";
// HubActivity is the Activity sidebar: a chat's subagents (delegate_task)
// and its background commands and /bg prompts, with a live tail of each,
// and the token counts in the chat's top bar. Subagents come from the
// session's subagent.* events and from GET .../activity, which lists the
// running ones (subagent.list) and the background commands (process.list).
// /bg prompts come from background.start (sent by the hub) and
// background.complete. ctx gives it the app's api, el, banner, chat (the
// open Chat or null), showText (a dialog with a long text), words, and the
// chat's toast, highlight, diff and page (the card of a published page)
// for the tool cards of a subagent's steps.
// An open subagent row shows its steps as a small chat (minichat.js), read
// from GET .../subagents/{id}/events after the last seq it holds.
window.HubActivity = (ctx) => {
  const { api, el } = ctx;
  const $ = (id) => document.getElementById(id);
  const live = new Set(["run", "wait", "queued", "hold"]);
  const stateWord = { run: "Running", queued: "Starting", ok: "Done", bad: "Failed", hold: "Waiting", wait: "Needs you", stopped: "Stopped" };
  const tailMax = 200;
  const narrow = () => matchMedia("(max-width: 900px)").matches;

  // S is the activity of the open chat. It is cleared when another chat
  // opens; a fresh snapshot of the same chat keeps it, since the events
  // before a snapshot are not sent again.
  let S = fresh(null);
  function fresh(chat) {
    return { chat, subs: new Map(), bgs: new Map(), open: new Set(), tab: "sub", paused: false, turn: null,
      usage: null, running: false, error: "", failingSince: 0, loaded: false, polled: 0, timer: 0, tick: 0,
      approvals: new Map(), asked: new Set(), minis: new Map(), raw: new Set(), nodes: new Map(), heads: new Map(), sumKey: "", fed: 0 };
  }

  // k writes a token count: 812, 1.3k, 41k, 1.2M.
  function k(n) {
    if (typeof n !== "number" || !isFinite(n)) return "?";
    if (n >= 1e6) return +(n / 1e6).toFixed(1) + "M";
    if (n >= 1e4) return Math.round(n / 1000) + "k";
    if (n >= 1000) return +(n / 1000).toFixed(1) + "k";
    return String(Math.round(n));
  }

  // fmt writes an elapsed time in seconds: 41s, 1m 36s, 2h 5m.
  function fmt(s) {
    s = Math.max(0, Math.floor(s));
    if (s < 60) return s + "s";
    if (s < 3600) return Math.floor(s / 60) + "m " + String(s % 60).padStart(2, "0") + "s";
    return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
  }

  function toast(text) {
    const x = $("toast");
    if (!x) return;
    x.textContent = text;
    x.hidden = false;
    clearTimeout(toast.h);
    toast.h = setTimeout(() => { x.hidden = true; }, 2200);
  }

  function push(row, kind, text) {
    text = String(text || "").trim();
    if (!text) return;
    const last = row.tail[row.tail.length - 1];
    if (last && last[0] === kind && last[1] === text) return;
    row.tail.push([kind, text]);
    if (row.tail.length > tailMax) row.tail.splice(0, row.tail.length - tailMax);
  }

  const firstLine = (s) => String(s || "").split("\n").map((l) => l.trim()).find(Boolean) || "";

  // subState maps a subagent status (tools/delegate_tool.py) to a row state.
  function subState(status) {
    switch (status) {
      case "queued": case "spawn_requested": return "queued";
      case "running": case "": case undefined: case null: return "run";
      case "completed": case "complete": case "success": return "ok";
      case "interrupted": case "cancelled": return "stopped";
      // Ended its turn waiting on a background job; Hermes resumes it when the job exits (patch 0092).
      case "waiting": return "hold";
      default: return "bad";
    }
  }

  // holdLine is the line of a subagent waiting on its background job: the
  // first command, cut short, and how many more there are.
  function holdLine(cmds) {
    const list = (cmds || []).map(firstLine).filter(Boolean);
    if (!list.length) return "Waiting on a background job";
    const c = list[0].length > 70 ? list[0].slice(0, 69).trimEnd() + "\u2026" : list[0];
    return "Waiting on " + c + (list.length > 1 ? " (+" + (list.length - 1) + " more)" : "");
  }

  function sub(id, p) {
    let r = S.subs.get(id);
    if (!r) {
      r = { id, kind: "sub", goal: "", model: "", state: "queued", startedAt: Date.now(), endedAt: 0, tools: 0,
        tin: null, tout: null, now: "", tool: null, missed: 0, tail: [], result: "", parentId: "", delegation: "", child: "", steer: true };
      S.subs.set(id, r);
    }
    if (p) {
      if (p.goal) r.goal = firstLine(p.goal);
      if (p.model) r.model = p.model;
      if (p.parent_id) r.parentId = p.parent_id;
      if (p.delegation_id) r.delegation = p.delegation_id;
      if (p.child_session_id) r.child = p.child_session_id;
      if (typeof p.tool_count === "number") r.tools = Math.max(r.tools, p.tool_count);
    }
    return r;
  }

  // event takes one session event. It returns true when the sidebar
  // should be drawn again.
  function event(kind, p) {
    p = p || {};
    if (kind.startsWith("subagent.")) { S.fed = Date.now(); return subEvent(kind, p); }
    switch (kind) {
      case "background.start": {
        if (!p.task_id) return false;
        const r = bg(p.task_id, "prompt");
        r.goal = r.goal || firstLine(p.text);
        r.text = p.text || r.text || "";
        if (typeof p.started_at === "number") r.startedAt = p.started_at * 1000;
        if (r.state !== "ok" && r.state !== "bad") r.state = "run";
        r.now = r.now || "Working in the background";
        return true;
      }
      case "background.complete": {
        if (!p.task_id) return false;
        const r = bg(p.task_id, "prompt");
        const text = String(p.text || "");
        const failed = /^error: /.test(text);
        r.state = failed ? "bad" : "ok";
        r.endedAt = Date.now();
        r.result = text;
        r.now = failed ? firstLine(text) : firstLine(text) || "Finished";
        if (!r.goal) r.goal = "Background prompt";
        tokensLater(r);
        return true;
      }
      case "snapshot":
        S.running = !!p.running;
        S.usage = (p.info && p.info.usage) || null;
        return true;
      case "message.start": S.running = true; return true;
      case "message.complete":
        S.running = false;
        if (p.hub_turn) S.turn = p.hub_turn;
        if (p.usage) S.usage = p.usage;
        return true;
      case "session.usage":
        if (p.hub_turn) S.turn = p.hub_turn;
        if (p.usage) S.usage = p.usage;
        return true;
      case "approval.request": return approval(p);
      case "approval.settled": case "clarify.settled": return settle(p.prompt_id);
      case "prompt.lost": {
        let any = false;
        for (const id of p.prompt_ids || []) any = settle(id) || any;
        return any;
      }
      case "approval.cancelled": {
        let any = false;
        for (const id of p.request_ids || []) any = settle(id) || any;
        return any;
      }
      case "tool.complete": return toolDone(p);
    }
    return false;
  }

  function subEvent(kind, p) {
    if (!p.subagent_id) return false;
    const r = sub(p.subagent_id, p);
    const text = String(p.text || "").trim();
    switch (kind) {
      case "subagent.spawn_requested":
        if (!live.has(r.state)) return true;
        r.state = "queued";
        r.now = r.now || "Waiting to start";
        break;
      case "subagent.start":
        if (live.has(r.state)) r.state = r.state === "wait" ? "wait" : "run";
        r.now = r.now || "Started";
        break;
      case "subagent.thinking":
        if (text) { push(r, "think", text); r.now = text; r.tool = null; }
        if (r.state === "queued") r.state = "run";
        break;
      case "subagent.tool": {
        const P = window.HubToolCard.plain;
        const line = P((P(p.tool_name) || "tool") + "  " + (p.tool_preview || text));
        push(r, "t", line);
        r.now = line;
        r.tool = { name: P(p.tool_name) || "tool", preview: P(p.tool_preview || text || ""), at: Date.now() };
        r.lastTool = p.tool_name || "";
        r.lastPreview = p.tool_preview || text;
        if (typeof p.tool_count !== "number") r.tools++;
        if (r.state === "queued") r.state = "run";
        break;
      }
      case "subagent.progress":
        if (text) {
          // A batched call arrives as plain progress text: shown as a sentence, like a tool.
          const clean = window.HubToolCard.plain(text);
          push(r, "k", clean);
          r.now = clean;
          r.tool = window.HubToolCard.batch(clean) ? { name: "tool_call", preview: clean, at: Date.now() } : null;
        }
        break;
      case "subagent.complete":
        // A subagent that waits on a job has not completed: Hermes resumes it.
        if (subState(p.status) === "hold") {
          if (live.has(r.state)) { r.state = "hold"; r.tool = null; }
          break;
        }
        r.state = subState(p.status || "completed");
        r.endedAt = Date.now();
        if (typeof p.duration_seconds === "number") r.startedAt = Math.min(r.startedAt, r.endedAt - p.duration_seconds * 1000);
        if (typeof p.input_tokens === "number") r.tin = p.input_tokens;
        if (typeof p.output_tokens === "number") r.tout = p.output_tokens;
        r.result = String(p.summary || p.text || "");
        for (const o of p.output_tail || []) push(r, o.is_error ? "e" : "t", (o.tool || "") + "  " + firstLine(o.preview));
        r.now = firstLine(r.result) || stateWord[r.state];
        r.tool = null;
        r.byMiss = false;
        r.final = true;
        for (const [id, a] of S.approvals) if (a.sub === r.id) S.approvals.delete(id);
        break;
    }
    return true;
  }

  function bg(id, kind) {
    let r = S.bgs.get(id);
    if (!r) {
      r = { id, kind, goal: "", state: "run", startedAt: Date.now(), endedAt: 0, now: "", tail: [], result: "",
        tin: null, tout: null, output: "", exit: null };
      S.bgs.set(id, r);
    }
    return r;
  }

  // toolDone adds a background command the moment the terminal tool
  // reports it started, before the next poll lists it.
  function toolDone(p) {
    if (p.name !== "terminal") return false;
    const res = parse(p.result);
    if (!res || typeof res.session_id !== "string" || !/^proc_/.test(res.session_id)) return false;
    const r = bg(res.session_id, "command");
    if (!r.goal && p.args && typeof p.args.command === "string") r.goal = firstLine(p.args.command);
    r.now = r.now || "Started";
    soon();
    return true;
  }

  function parse(v) {
    if (v && typeof v === "object") return v;
    if (typeof v !== "string" || v[0] !== "{") return null;
    try { return JSON.parse(v); } catch (_) { return null; }
  }

  // approval puts an approval in the row of the subagent that asked for
  // it. Hermes does not say which subagent asks, so the hub picks the one
  // whose last tool call names the command, or the only one running; with
  // neither, the approval shows in the chat only.
  function approval(p) {
    if (p.kind === "clarify" || !p.prompt_id) return false;
    const running = [...S.subs.values()].filter((r) => r.state === "run" || r.state === "wait");
    if (!running.length) return false;
    const cmd = String(p.command || "").trim();
    const head = cmd.slice(0, 60);
    let pick = head ? running.filter((r) => r.lastPreview && (r.lastPreview.includes(head) || head.includes(r.lastPreview.slice(0, 60)))) : [];
    if (pick.length !== 1) pick = running.length === 1 ? running : [];
    if (pick.length !== 1) return false;
    const r = pick[0];
    S.approvals.set(p.prompt_id, { sub: r.id, p });
    r.state = "wait";
    r.now = "Waiting for your approval" + (cmd ? ": " + firstLine(cmd) : "");
    push(r, "e", "Needs approval: " + (firstLine(cmd) || p.description || "a command"));
    S.open.add(r.id);
    return true;
  }

  function settle(id) {
    const a = S.approvals.get(id);
    if (!a) return false;
    S.approvals.delete(id);
    const r = S.subs.get(a.sub);
    if (r && r.state === "wait" && ![...S.approvals.values()].some((x) => x.sub === r.id)) r.state = "run";
    return true;
  }

  // merge takes a GET .../activity answer.
  function merge(st) {
    S.paused = !!st.paused;
    const seen = new Set();
    for (const s of st.subagents || []) {
      seen.add(s.id);
      const r = sub(s.id, { goal: s.goal, model: s.model, parent_id: s.parent_id, child_session_id: s.child_session_id,
        tool_count: s.tool_count });
      // A start never moves later: a restart may hand the subagent to a Hermes that lists its own start.
      if (s.started_at) r.startedAt = Math.min(r.startedAt, s.started_at * 1000);
      r.steer = s.accepting_steer !== false;
      r.missed = 0;
      // The status Hermes lists is the truth, a subagent.complete event too:
      // a subagent is finished when one of them says so, and not because it
      // is missing from one answer.
      const want = subState(s.status);
      if (want === "hold") {
        // Waiting is alive: it counts as running, shows what it waits on, and no miss or event ends it.
        r.byMiss = false;
        r.endedAt = 0;
        r.final = false;
        r.state = "hold";
        r.tool = null;
        r.now = holdLine(s.waiting_on);
        continue;
      }
      if (r.state === "hold" && (want === "run" || want === "queued")) {
        // It woke up: the job ended and Hermes resumed it.
        r.state = want;
        r.now = "";
      } else if (r.byMiss && (want === "run" || want === "queued")) {
        r.byMiss = false;
        r.endedAt = 0;
        r.state = want;
      } else if (live.has(r.state)) {
        if (want === "run" || want === "queued") {
          if (r.state !== "wait") r.state = want;
        } else {
          r.state = want;
          r.endedAt = r.endedAt || Date.now();
          r.tool = null;
          r.now = r.now || stateWord[want];
        }
      }
      // The list names the last tool and what it was given. A page that missed the event has only this.
      const lastPreview = window.HubToolCard.plain(s.last_preview || "");
      if (!r.now && !r.tool && s.last_tool) r.tool = { name: s.last_tool, preview: lastPreview, at: 0 };
      else if (r.tool && !r.tool.preview && lastPreview && r.tool.name === s.last_tool) r.tool.preview = lastPreview;
    }
    // One answer without a subagent proves nothing. After two, verify() asks
    // Hermes whether it still runs before the row says it ended.
    for (const r of S.subs.values()) {
      if (!seen.has(r.id) && live.has(r.state) && Date.now() - r.startedAt > 5000) r.missed = (r.missed || 0) + 1;
    }
    const procs = new Set();
    for (const p of st.processes || []) {
      procs.add(p.id);
      const r = bg(p.id, "command");
      r.goal = firstLine(p.command) || r.goal;
      r.command = p.command;
      if (p.started_at) r.startedAt = p.started_at * 1000;
      r.output = p.output || "";
      const lines = r.output.split("\n").filter((l) => l.trim()).slice(-tailMax);
      r.tail = lines.map((l) => ["", l]);
      if (p.running) {
        r.state = "run";
        r.now = lines.length ? lines[lines.length - 1] : "Running";
      } else {
        r.exit = typeof p.exit_code === "number" ? p.exit_code : null;
        r.endedAt = p.exited_at ? p.exited_at * 1000 : (r.endedAt || Date.now());
        r.state = p.reason === "killed" ? "stopped" : r.exit === 0 ? "ok" : "bad";
        const how = p.reason === "killed" ? "Stopped" : r.exit === null ? "Ended" : "Exit " + r.exit;
        r.now = how + (lines.length ? " · " + lines[lines.length - 1] : "");
        if (r.state !== "ok") r.tail.push(["e", how]);
      }
    }
    for (const r of S.bgs.values()) {
      if (r.kind === "command" && !procs.has(r.id) && r.state === "run" && Date.now() - r.startedAt > 10000) {
        r.state = "ok";
        r.endedAt = r.endedAt || Date.now();
      }
    }
  }

  async function poll() {
    const chat = S.chat;
    if (!chat) return;
    clearTimeout(S.timer);
    S.timer = 0;
    if (!document.hidden) {
      try {
        const st = await api("GET", chat.base + "/activity");
        if (chat !== S.chat) return;
        S.error = "";
        S.failingSince = 0;
        S.loaded = true;
        merge(st);
      } catch (e) {
        if (chat !== S.chat) return;
        // A backend that restarts answers again within seconds: keep showing the last good list
        // through a short gap, and say it failed only when it lasts.
        S.failingSince = S.failingSince || Date.now();
        if (!S.loaded || Date.now() - S.failingSince >= ACTIVITY_GAP_MS) S.error = e.message;
      }
      S.polled = Date.now();
      await verify();
      await refreshMinis();
      if (chat !== S.chat) return;
      draw();
    }
    schedule();
  }

  const pollBackoff = window.HubBackoff({ base: 3000, max: 30000 });
  // How long a failing poll keeps the last good activity on screen before it shows the error.
  const ACTIVITY_GAP_MS = 60000;

  function schedule() {
    if (!S.chat) return;
    clearTimeout(S.timer);
    const busy = count() > 0 || S.running;
    let ms = busy || isOpen() ? 3000 : 20000;
    // The event feed carries subagent starts, steps and ends: while it has
    // spoken lately and no background command runs (a command's output
    // comes only from the list), the list is read less often.
    if (ms === 3000 && S.fed && Date.now() - S.fed < 15000 && ![...S.bgs.values()].some((r) => r.kind === "command" && live.has(r.state))) ms = 10000;
    if (S.error) ms = Math.max(ms, pollBackoff.next());
    else pollBackoff.reset();
    S.timer = setTimeout(poll, ms);
  }

  function soon() {
    if (!S.chat) return;
    clearTimeout(S.timer);
    S.timer = setTimeout(poll, 400);
  }

  // verify asks the hub, which asks Hermes, about each running subagent the
  // list no longer shows. Only an answer that comes from Hermes and says it
  // does not run ends the row; no answer, or "runs", keeps it as it is.
  async function verify() {
    const chat = S.chat;
    const gone = [...S.subs.values()].filter((r) => live.has(r.state) && r.missed >= 2 && !r.verifying && Date.now() - (r.checkedAt || 0) > 8000);
    await Promise.all(gone.map(async (r) => {
      r.verifying = true;
      r.checkedAt = Date.now();
      try {
        const got = await api("GET", chat.base + "/subagents/" + encodeURIComponent(r.id) + "/events?after=999999999999999");
        if (chat !== S.chat) return;
        if (got.running) r.missed = 0;
        else if (got.live && live.has(r.state)) {
          r.state = "ok";
          r.byMiss = true;
          r.endedAt = r.endedAt || Date.now();
          r.tool = null;
          r.now = r.now || "Finished";
        }
      } catch (_) { /* no answer: the row stays as it is */ } finally {
        r.verifying = false;
      }
    }));
  }

  // mini is the small chat of a subagent's steps, kept across draws so it
  // keeps its place and what the reader opened.
  function mini(r) {
    let m = S.minis.get(r.id);
    if (!m) {
      m = { view: window.HubMiniChat.create({ el, toast, highlight: ctx.highlight, diff: ctx.diff, page: ctx.page }), seq: 0, epoch: null,
        busy: false, done: false, top: 0, end: true };
      S.minis.set(r.id, m);
      m.view.empty("Loading the steps…");
    }
    m.view.set(miniInfo(r));
    return m;
  }

  function miniInfo(r) {
    if (live.has(r.state)) return { model: r.model };
    return { model: r.model, end: { state: r.state, durationS: r.endedAt && r.startedAt ? (r.endedAt - r.startedAt) / 1000 : 0,
      result: r.result, tin: r.tin, tout: r.tout, tokens: r.tin !== null || r.tout !== null ? k(r.tin) + " in · " + k(r.tout) + " out" : "",
      endedAt: r.endedAt } };
  }

  // readSteps reads the steps of subagent r after the last one its small
  // chat holds, until the hub has no more. A transcript Hermes rewrote
  // (a new epoch) is read again from the start.
  async function readSteps(r) {
    const chat = S.chat;
    const m = mini(r);
    if (m.busy) return;
    m.busy = true;
    try {
      for (let i = 0; i < 20; i++) {
        const got = await api("GET", chat.base + "/subagents/" + encodeURIComponent(r.id) + "/events?after=" + m.seq);
        if (chat !== S.chat) return;
        if (m.epoch !== null && got.epoch !== m.epoch && m.seq > 0) {
          m.view.reset();
          m.seq = 0;
          m.epoch = got.epoch;
          continue;
        }
        m.epoch = got.epoch;
        const evs = got.events || [];
        if (evs.length) {
          m.view.clearEmpty();
          m.view.add(evs);
          m.seq = evs[evs.length - 1].seq;
        }
        if (!got.more) break;
      }
      if (!m.view.events().length) {
        m.view.clearEmpty();
        m.view.empty(live.has(r.state) ? "No steps yet." : "The agent kept no steps of this subagent.");
      }
      m.done = !live.has(r.state);
    } catch (e) {
      if (chat === S.chat && !m.view.events().length) {
        m.view.clearEmpty();
        m.view.empty("Could not load the steps: " + e.message);
      }
    } finally {
      m.busy = false;
    }
  }

  // refreshMinis reads the new steps of each open subagent: a running one
  // on every poll, an ended one until it has its last steps.
  async function refreshMinis() {
    const want = [...S.open].map((id) => S.subs.get(id)).filter((r) => r && (live.has(r.state) || !(S.minis.get(r.id) || {}).done));
    await Promise.all(want.map(readSteps));
  }

  // tokensLater reads a finished /bg prompt's tokens from Hermes's records,
  // which have them a moment after it ends.
  function tokensLater(r, tries) {
    const chat = S.chat;
    if (!chat || r.tin !== null) return;
    tries = tries || 0;
    setTimeout(async () => {
      if (chat !== S.chat) return;
      try {
        const got = await api("GET", chat.base + "/background-tokens?task=" + encodeURIComponent(r.id));
        const t = got && got[r.id];
        if (t) {
          r.tin = (t.input || 0) + (t.cache_read || 0) + (t.cache_write || 0);
          r.tout = t.output || 0;
          draw();
          return;
        }
      } catch (_) { /* tried again below */ }
      if (tries < 2) tokensLater(r, tries + 1);
    }, 1500 * (tries + 1));
  }

  const isOpen = () => !$("app").classList.contains("act-closed");

  function count() {
    let n = 0;
    for (const r of S.subs.values()) if (live.has(r.state)) n++;
    for (const r of S.bgs.values()) if (live.has(r.state)) n++;
    return n;
  }

  function setOpen(open, remember) {
    $("app").classList.toggle("act-closed", !open);
    $("actbtn").setAttribute("aria-expanded", String(open));
    if (remember) {
      try { localStorage.setItem("hub.activity", open ? "open" : "closed"); } catch (_) { /* storage off */ }
    }
    if (open) { draw(); soon(); }
  }

  function wantOpen() {
    if (narrow()) return false;
    try { return localStorage.getItem("hub.activity") !== "closed"; } catch (_) { return true; }
  }

  // show starts following chat (a Chat, or null for no chat).
  function show(chat) {
    clearTimeout(S.timer);
    clearInterval(S.tick);
    S = fresh(chat);
    $("actbtn").hidden = !chat;
    $("tok").hidden = true;
    if (!chat) {
      $("app").classList.add("act-closed", "act-none");
      return;
    }
    $("app").classList.remove("act-none");
    setOpen(wantOpen(), false);
    S.tick = setInterval(() => { if (!document.hidden && count()) tickRows(); }, 1000);
    draw();
    soon();
  }

  // reveal opens the sidebar on a tab, with the rows of ids open.
  function reveal(tab, ids) {
    S.tab = tab;
    for (const id of ids) S.open.add(id);
    setOpen(true, false);
    draw();
    const first = ids.find((id) => document.querySelector('[data-act="' + CSS.escape(id) + '"]'));
    if (first) document.querySelector('[data-act="' + CSS.escape(first) + '"]').scrollIntoView({ block: "nearest" });
  }

  // fromTool opens the sidebar on the work a chat step started: the
  // subagents of a delegate_task, or a background command.
  function fromTool(name, args, result) {
    const res = parse(result) || {};
    if (name === "terminal") {
      if (typeof res.session_id === "string" && /^proc_/.test(res.session_id)) {
        bg(res.session_id, "command");
        reveal("bg", [res.session_id]);
        soon();
        return true;
      }
      return false;
    }
    if (name !== "delegate_task") return false;
    let ids = [...S.subs.values()].filter((r) => res.delegation_id && r.delegation === res.delegation_id).map((r) => r.id);
    if (!ids.length) ids = [...S.subs.values()].filter((r) => !r.parentId).map((r) => r.id).slice(-((args && Array.isArray(args.tasks) && args.tasks.length) || 1));
    reveal("sub", ids);
    return true;
  }

  // steps says whether a chat step opens the sidebar.
  function opens(name, args) {
    return name === "delegate_task" || (name === "terminal" && !!(args && args.background));
  }

  async function act(fn, ok, bad) {
    try {
      await fn();
      if (ok) toast(ok);
      soon();
    } catch (e) {
      toast((bad || "Could not do that") + ": " + e.message);
    }
  }

  // ago writes how long ago a step began: 8 s, 3 min, 2 h.
  function ago(s) {
    s = Math.floor(s);
    if (s < 1) return "";
    if (s < 60) return s + " s";
    if (s < 3600) return Math.round(s / 60) + " min";
    return Math.floor(s / 3600) + " h";
  }

  // nowLine is the line under a running subagent's row. A tool it runs is a
  // plain sentence with an icon and how long it has gone on; the line is a
  // button that opens the raw text Hermes sent, with a copy button. Anything
  // else is the subagent's own words.
  function nowLine(r) {
    if (!(r.kind === "sub" && r.tool && live.has(r.state) && r.state !== "wait" && r.state !== "hold")) return [el("div", "now", r.now || "")];
    const m = S.minis.get(r.id);
    const line = window.HubToolCard.statusLine(r.tool.name, r.tool.preview, m ? m.view.memory() : null);
    const raw = r.tool.preview; // what the tool was given (the command), never its name
    const open = S.raw.has(r.id);
    // With only the tool's name there is no raw text worth showing: no box to open.
    if (!r.tool.preview) {
      const d = el("div", "now2");
      d.append(window.HubIcons.icon(line.icon), el("span", "lbl", line.text));
      const w = r.tool.at ? ago((Date.now() - r.tool.at) / 1000) : "";
      if (w) d.append(el("span", "ago", w));
      return [d];
    }
    const b = el("button", "now2");
    b.type = "button";
    b.setAttribute("aria-expanded", String(open));
    b.title = "Show the raw text";
    const when = r.tool.at ? ago((Date.now() - r.tool.at) / 1000) : "";
    b.append(window.HubIcons.icon(line.icon), el("span", "lbl", line.text));
    if (when) b.append(el("span", "ago", when));
    const chev = window.HubIcons.icon("chev");
    chev.classList.add("chev");
    b.append(chev);
    const stop = (e) => e.stopPropagation();
    b.onclick = (e) => {
      e.stopPropagation();
      if (S.raw.has(r.id)) S.raw.delete(r.id); else S.raw.add(r.id);
      draw();
    };
    b.onkeydown = stop;
    const out = [b];
    if (open) {
      const box = el("div", "rawbox");
      const code = el("code", "", raw);
      const cp = window.HubIcons.button("copy", "Copy", "ghost ib");
      cp.onclick = async (e) => {
        e.stopPropagation();
        try {
          await navigator.clipboard.writeText(raw);
          toast("Copied");
        } catch (_) {
          toast("The browser did not allow copying");
        }
      };
      cp.onkeydown = stop;
      box.onclick = stop;
      box.append(code, cp);
      out.push(box);
    }
    return out;
  }

  // rowKey says what a closed row shows apart from its clock: a row whose
  // key is the same as when it was drawn keeps its node. Open rows are drawn
  // again, since their body holds forms and a small chat.
  function rowKey(r, child) {
    let line = "";
    if (r.kind === "sub" && r.tool && live.has(r.state) && r.state !== "wait" && r.state !== "hold") {
      const m = S.minis.get(r.id);
      const l = window.HubToolCard.statusLine(r.tool.name, r.tool.preview, m ? m.view.memory() : null);
      line = l.icon + "|" + l.text + "|" + (r.tool.at && ago((Date.now() - r.tool.at) / 1000) ? 1 : 0) + "|" + S.raw.has(r.id) + "|" + r.tool.name + " " + r.tool.preview;
    }
    return JSON.stringify([child, r.id, r.kind, r.state, r.goal, r.startedAt, r.endedAt, r.model, r.tools, r.tin, r.tout, r.now, line]);
  }

  // rowNode gives the node for row r: the one drawn before when nothing it
  // shows changed, else a new one.
  function rowNode(r, child) {
    if (S.open.has(r.id)) { S.nodes.delete(r.id); return row(r, child); }
    const key = rowKey(r, child);
    const old = S.nodes.get(r.id);
    if (old && old.r === r && old.key === key) return old.node;
    const node = row(r, child);
    S.nodes.set(r.id, { r, key, node });
    return node;
  }

  // tickRows moves the clocks of the running rows that are on show, and
  // nothing else: the second's tick does not build a row.
  function tickRows() {
    if (isOpen() && clocks()) draw();
  }
  function clocks() {
    let redraw = false;
    for (const [id, c] of S.nodes) {
      const r = c.r;
      if (r.endedAt || !c.node.isConnected) continue;
      const t = c.node.querySelector(".time");
      if (t) t.textContent = window.HubTimes.counter(Date.now() - r.startedAt);
      const a = c.node.querySelector(".now2 .ago");
      if (r.tool && r.tool.at && a) a.textContent = ago((Date.now() - r.tool.at) / 1000);
      else if (r.kind === "sub" && c.key !== rowKey(r, JSON.parse(c.key)[0])) redraw = true;
    }
    return redraw;
  }

  function row(r, child) {
    const box = el("div", "row" + (S.open.has(r.id) ? " open" : "") + (child ? " child" : ""));
    box.dataset.act = r.id;
    const head = el("div", "row-head");
    head.tabIndex = 0;
    head.setAttribute("role", "button");
    head.setAttribute("aria-expanded", String(S.open.has(r.id)));
    const end = r.endedAt || Date.now();
    const running = !r.endedAt;
    head.append(el("span", "state " + r.state), el("span", "goal", r.goal || (r.kind === "sub" ? "Subagent" : "Command")),
      el("span", "time", running ? window.HubTimes.counter(end - r.startedAt) : fmt((end - r.startedAt) / 1000)));
    const meta = el("div", "meta");
    meta.append(el("span", "", stateWord[r.state] || "Done"));
    const when = el("span");
    if (running) when.append("started ", window.HubTimes.stamp(el("span"), r.startedAt));
    else window.HubTimes.stamp(when, r.endedAt);
    meta.append(when);
    if (r.kind === "sub") {
      if (r.model) meta.append(el("span", "", r.model));
      meta.append(el("span", "", r.tools + (r.tools === 1 ? " tool" : " tools")));
    } else {
      meta.append(el("span", "", r.kind === "prompt" ? "Prompt" : "Command"));
    }
    if (r.tin !== null || r.tout !== null) meta.append(el("span", "", k(r.tin) + " in · " + k(r.tout) + " out"));
    head.append(meta);
    head.append(...nowLine(r));
    const flip = () => {
      if (S.open.has(r.id)) S.open.delete(r.id); else S.open.add(r.id);
      draw();
      if (S.open.has(r.id) && r.kind === "sub") soon();
    };
    head.onclick = flip;
    head.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } };
    box.append(head);
    if (S.open.has(r.id)) box.append(body(r));
    return box;
  }

  function body(r) {
    const chat = S.chat;
    const b = el("div", "row-body");
    for (const [id, a] of S.approvals) if (a.sub === r.id) b.append(approvalBox(id, a.p));
    if (r.result && !live.has(r.state) && r.kind !== "sub") b.append(el("div", "result", r.result));
    if (r.kind === "sub") b.append(mini(r).view.node);
    const lines = r.kind === "sub" ? [] : r.tail;
    if (lines.length) {
      const tail = el("div", "tail");
      for (const [kind, text] of lines) {
        tail.append(el("div", "line" + (kind ? " " + kind : ""), kind === "think" ? "… " + text : kind === "t" ? "▸ " + text : text));
      }
      b.append(tail);
      requestAnimationFrame(() => { tail.scrollTop = tail.scrollHeight; });
    } else if (r.kind === "prompt" && live.has(r.state)) {
      b.append(el("div", "result muted", "The reply comes when it is done."));
    }
    const running = live.has(r.state);
    if (r.kind === "sub" && running && r.steer) {
      const form = el("form", "steer");
      const inp = el("input");
      inp.placeholder = "Message this subagent";
      inp.setAttribute("aria-label", "Message this subagent");
      inp.maxLength = 16384;
      const send = el("button", "", "Send");
      send.type = "submit";
      form.append(inp, send);
      form.onsubmit = (e) => {
        e.preventDefault();
        const text = inp.value.trim();
        if (!text) return;
        send.disabled = true;
        act(async () => {
          await api("POST", chat.base + "/subagents/" + encodeURIComponent(r.id) + "/steer", { text });
          push(r, "k", "You: " + text);
          inp.value = "";
          await readSteps(r);
          draw();
        }, "Sent to " + (r.goal || "the subagent") + ". It reads it at its next step.", "Could not send").finally(() => { send.disabled = false; });
      };
      b.append(form);
    }
    const acts = el("div", "acts");
    if (r.kind === "sub" && running) {
      const p = el("button", "", S.paused ? "Resume" : "Pause");
      p.type = "button";
      p.title = S.paused ? "Let Talaria start new subagents again" : "Stop Talaria from starting new subagents on this backend. Running ones go on.";
      p.onclick = () => act(async () => {
        const got = await api("POST", chat.base + "/subagents-paused", { paused: !S.paused });
        S.paused = !!(got && got.paused);
        draw();
      }, S.paused ? "New subagents can start again" : "No new subagents start until you resume. Running ones go on.", "Could not change it");
      acts.append(p);
    }
    if (r.kind === "command" && (r.output || r.tail.length)) {
      const o = el("button", "", "Full output");
      o.type = "button";
      o.onclick = () => ctx.showText(r.command || r.goal, r.output || r.tail.map((l) => l[1]).join("\n"));
      acts.append(o);
    }
    if (r.kind === "sub" && !running) {
      const o = el("button", "", "Open full transcript");
      o.type = "button";
      o.onclick = () => fullSteps(r);
      acts.append(o);
    }
    if (r.kind === "prompt" && !running) {
      const o = el("button", "", "Open chat");
      o.type = "button";
      o.title = "Open the full transcript of this background prompt";
      o.onclick = () => { location.hash = "#/s/" + encodeURIComponent(chat.backend) + "/" + encodeURIComponent(r.id); };
      acts.append(o);
    }
    if (running && r.kind !== "prompt") {
      const stop = el("button", "danger-text", "Stop");
      stop.type = "button";
      stop.onclick = () => {
        stop.disabled = true;
        const path = r.kind === "sub" ? "/subagents/" : "/processes/";
        act(() => api("POST", chat.base + path + encodeURIComponent(r.id) + "/stop"), "Stopped", "Could not stop it")
          .finally(() => { stop.disabled = false; });
      };
      acts.append(stop);
    }
    if (acts.children.length) b.append(acts);
    return b;
  }

  // fullSteps shows every step of an ended subagent in a dialog, without
  // the height of the row.
  function fullSteps(r) {
    if (document.querySelector("dialog.steps")) return;
    const m = S.minis.get(r.id);
    const d = el("dialog", "picker steps");
    d.append(el("h2", "", r.goal || "Subagent"));
    const v = window.HubMiniChat.create({ el, toast, highlight: ctx.highlight, diff: ctx.diff, page: ctx.page, full: true });
    const body = el("div", "pbody");
    body.append(v.node);
    const foot = el("div", "pfoot");
    const shut = el("button", "primary", "Close");
    shut.type = "button";
    shut.onclick = () => d.close();
    foot.append(shut);
    d.append(body, foot);
    d.addEventListener("close", () => d.remove());
    document.body.append(d);
    d.showModal();
    v.set(miniInfo(r));
    v.add(m ? m.view.events() : []);
  }

  function approvalBox(id, p) {
    const chat = S.chat;
    const c = el("div", "confirm");
    const what = firstLine(p.command) || p.description || "this";
    c.append(el("span", "", "Allow " + what + "?"));
    const names = { once: "Once", session: "This chat", always: "Always", deny: "Deny" };
    const choices = Array.isArray(p.choices) && p.choices.length ? p.choices : ["once", "deny"];
    const btns = [];
    for (const ch of choices) {
      const btn = el("button", ch === "once" ? "primary" : ch === "deny" ? "danger-text" : "", names[ch] || ch);
      btn.type = "button";
      btn.onclick = () => {
        for (const x of btns) x.disabled = true;
        act(async () => {
          await api("POST", chat.base + "/prompts/" + encodeURIComponent(id) + "/answer", { choice: ch });
          settle(id);
          draw();
        }, ch === "deny" ? "Denied" : "Allowed", "Could not answer").finally(() => { for (const x of btns) x.disabled = false; });
      };
      btns.push(btn);
      c.append(btn);
    }
    return c;
  }

  function list(rows) {
    const byParent = new Map();
    const ids = new Set(rows.map((r) => r.id));
    for (const r of rows) {
      const p = r.parentId && ids.has(r.parentId) ? r.parentId : "";
      if (!byParent.has(p)) byParent.set(p, []);
      byParent.get(p).push(r);
    }
    for (const l of byParent.values()) l.sort((a, b) => a.startedAt - b.startedAt);
    return byParent;
  }

  function section(body, title, rows, tree) {
    if (!rows.length) return;
    let h = S.heads.get(title);
    if (!h) { h = el("div", "sec", title); S.heads.set(title, h); }
    body.push(h);
    const add = (r, depth) => {
      body.push(rowNode(r, depth > 0));
      for (const c of (tree && tree.get(r.id)) || []) add(c, depth + 1);
    };
    const top = tree ? rows.filter((r) => !r.parentId || !rows.some((x) => x.id === r.parentId)) : rows;
    for (const r of top.sort((a, b) => a.startedAt - b.startedAt)) add(r, 0);
  }

  function totals(rows) {
    let i = 0, o = 0, known = false;
    for (const r of rows) {
      if (typeof r.tin === "number") { i += r.tin; known = true; }
      if (typeof r.tout === "number") { o += r.tout; known = true; }
    }
    return { i, o, known };
  }

  // tokenText says what the subagents used. Tokens come when a subagent
  // finishes, so with none counted yet it says so and does not claim 0.
  function tokenText(t, running, finished) {
    const state = running || finished ? " · " + running + " running, " + finished + " finished" : "";
    if (!t.known) return [el("b", "", "tokens appear when each finishes"), state];
    return [el("b", "", k(t.i)), " in · ", el("b", "", k(t.o)), " out", running ? " so far" + state : ""];
  }

  // draw paints the top bar's count and tokens, and the sidebar.
  function draw() {
    if (!S.chat) return;
    drawTokens();
    if (ctx.drawn) ctx.drawn();
    const subs = [...S.subs.values()], bgs = [...S.bgs.values()];
    const liveSub = subs.filter((r) => live.has(r.state)).length, liveBg = bgs.filter((r) => live.has(r.state)).length;
    const n = liveSub + liveBg;
    const c = $("actcount");
    c.textContent = String(n);
    c.hidden = n === 0;
    $("actbtn").title = n ? n + " running beside this chat" : "Subagents and background work of this chat";
    if (!isOpen()) return;
    $("tab-sub").setAttribute("aria-selected", String(S.tab === "sub"));
    $("tab-bg").setAttribute("aria-selected", String(S.tab === "bg"));
    $("c-sub").textContent = liveSub ? liveSub + " running" : "";
    $("c-bg").textContent = liveBg ? liveBg + " running" : "";
    const sum = $("actsum");
    const sumKey = JSON.stringify([S.tab, liveSub, liveBg, subs.length, S.paused, S.error, totals(subs), totals(bgs.filter((r) => r.kind === "prompt"))]);
    if (sumKey !== S.sumKey) {
    S.sumKey = sumKey;
    sum.replaceChildren();
    if (S.tab === "sub") {
      const t = totals(subs);
      sum.append("Subagents in this chat: ", ...tokenText(t, liveSub, subs.length - liveSub));
      if (S.paused) sum.append(el("div", "warn", "New subagents are paused on this backend."));
    } else {
      const t = totals(bgs.filter((r) => r.kind === "prompt"));
      sum.append("Background prompts: ", el("b", "", k(t.i)), " in · ", el("b", "", k(t.o)), " out · commands use no tokens");
    }
    if (S.error) sum.append(el("div", "bad", "Could not load activity: " + S.error));
    }
    const body = $("actbody");
    const top = body.scrollTop;
    // A small chat loses its place when its row is drawn again: keep it.
    for (const m of S.minis.values()) {
      const n = m.view.node;
      if (n.isConnected) { m.top = n.scrollTop; m.end = window.HubMiniChat.near(n); }
    }
    const focus = document.activeElement && body.contains(document.activeElement) && document.activeElement.tagName === "INPUT"
      ? { row: document.activeElement.closest("[data-act]"), value: document.activeElement.value, pos: document.activeElement.selectionStart }
      : null;
    const out = [];
    const rows = S.tab === "sub" ? subs : bgs;
    const now = rows.filter((r) => live.has(r.state)), done = rows.filter((r) => !live.has(r.state));
    const tree = S.tab === "sub" ? list(rows) : null;
    if (tree) {
      // A child goes with its parent's section, so the tree stays whole.
      const inNow = new Set(now.map((r) => r.id));
      const rootOf = (r) => { let x = r; const seen = new Set(); while (x.parentId && S.subs.has(x.parentId) && !seen.has(x.id)) { seen.add(x.id); x = S.subs.get(x.parentId); } return x; };
      const nowRoots = rows.filter((r) => inNow.has(rootOf(r).id) || [...inNow].some((id) => rootOf(S.subs.get(id)).id === rootOf(r).id));
      const doneRows = rows.filter((r) => !nowRoots.includes(r));
      section(out, "Now", nowRoots, tree);
      section(out, "Finished", doneRows, tree);
    } else {
      section(out, "Now", now, null);
      section(out, "Finished", done, null);
    }
    if (!rows.length) {
      const text = S.tab === "sub" ? "No subagents in this chat yet." : "Nothing running in the background. Start a prompt with /bg in the message box.";
      let e = S.heads.get(text);
      if (!e) { e = el("div", "empty", text); S.heads.set(text, e); }
      out.push(e);
    }
    // Put the nodes in place, moving only what is not where it belongs, so
    // a row that did not change is not touched.
    let at = body.firstChild;
    for (const n of out) {
      if (n === at) at = at.nextSibling;
      else body.insertBefore(n, at);
    }
    while (at) { const nx = at.nextSibling; body.removeChild(at); at = nx; }
    for (const id of [...S.nodes.keys()]) if (!out.includes(S.nodes.get(id).node)) S.nodes.delete(id);
    clocks();
    for (const m of S.minis.values()) {
      const n = m.view.node;
      if (n.isConnected) n.scrollTop = m.end ? n.scrollHeight : m.top;
    }
    body.scrollTop = top;
    if (focus && focus.row) {
      const again = body.querySelector('[data-act="' + CSS.escape(focus.row.dataset.act) + '"] input');
      if (again) { again.value = focus.value; again.focus(); try { again.setSelectionRange(focus.pos, focus.pos); } catch (_) { /* not text */ } }
    }
  }

  // drawTokens shows this turn's tokens and the chat's totals. On a phone
  // the parts in .long are hidden.
  function drawTokens() {
    const box = $("tok");
    const u = S.usage || (S.chat && S.chat.info && S.chat.info.usage) || null;
    const chatIn = u ? (u.prompt || u.input || 0) : 0;
    const chatOut = u ? (u.output || 0) : 0;
    if (!S.turn && !chatIn && !chatOut) { box.hidden = true; return; }
    box.hidden = false;
    const long = (text) => el("span", "long", text);
    const line = $("tokturn");
    line.replaceChildren();
    if (S.turn) {
      if (typeof S.turn.input === "number") line.append(el("b", "", k(S.turn.input)), " in · ");
      line.append(el("b", "", k(S.turn.output)), " out", long(S.running ? " this turn" : " last turn"));
    } else {
      line.append(el("b", "", k(chatIn)), " in · ", el("b", "", k(chatOut)), " out", long(" in this chat"));
    }
    const sub = $("toksub");
    sub.replaceChildren();
    const parts = [];
    if (S.turn && (chatIn || chatOut)) parts.push(["chat " + k(chatIn) + " in · " + k(chatOut) + " out", false]);
    if (u && typeof u.cache_hit_pct === "number") parts.push([u.cache_hit_pct + "% cached", true]);
    if (u && typeof u.avg_tps === "number" && u.avg_tps > 0) parts.push([Math.round(u.avg_tps) + " tok/s", false]);
    // A separator goes with the part after it, inside .long for a long
    // part, so a phone never shows one at the start.
    let any = false, short = false;
    for (const [text, isLong] of parts) {
      if (isLong) sub.append(long((any ? " · " : "") + text));
      else {
        if (short) sub.append(" · ");
        else if (any) sub.append(long(" · "));
        sub.append(text);
        short = true;
      }
      any = true;
    }
    sub.hidden = !parts.length;
  }

  function init() {
    $("actbtn").onclick = () => setOpen(!isOpen(), true);
    $("actclose").onclick = () => setOpen(false, true);
    $("tab-sub").onclick = () => { S.tab = "sub"; draw(); };
    $("tab-bg").onclick = () => { S.tab = "bg"; draw(); };
    document.addEventListener("visibilitychange", () => { if (!document.hidden && S.chat) soon(); });
    show(null);
  }

  // background starts a /bg prompt from the message box.
  async function background(text) {
    const chat = S.chat;
    if (!chat) return;
    const got = await api("POST", chat.base + "/background", { text });
    if (chat !== S.chat || !got || !got.task_id) return;
    event("background.start", { task_id: got.task_id, text, started_at: Date.now() / 1000 });
    reveal("bg", [got.task_id]);
  }

  init();
  // view and restore keep the sidebar as it was across a reload.
  const view = () => ({ open: isOpen(), tab: S.tab, rows: [...S.open], top: $("actbody").scrollTop });
  function restore(v) {
    if (!v || !S.chat) return;
    S.tab = v.tab === "bg" ? "bg" : "sub";
    for (const id of v.rows || []) S.open.add(id);
    setOpen(!!v.open, false);
    draw();
    $("actbody").scrollTop = v.top || 0;
  }

  // liveModels are the models of the chat's running subagents.
  const liveModels = () => S.chat ? [...S.subs.values()].filter((r) => live.has(r.state)).map((r) => r.model) : [];

  return { view, restore, show, event: (kind, p) => { if (S.chat && event(kind, p)) draw(); }, opens, fromTool, background, liveModels,
    running: (on) => { S.running = !!on; draw(); }, usage: (u) => { if (u) { S.usage = u; draw(); } } };
};
