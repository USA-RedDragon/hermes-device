"use strict";
// HubToolCard shows one tool call of a chat as a card with the look of the
// notice cards (notice.js): a line icon, a one-line title in plain words, a
// status line that leads with the result, then the time and how long it
// took, and the raw call and output behind the expansion.
// derive(c) is the pure part, run against internal/web/testdata/toolcards.json
// by a Go test: it turns a call (name, args, result) into the icon, title,
// status and the labeled blocks. memory() keeps what earlier calls of the
// chat taught (a background command by its session id, a subagent by its
// id) so that a later call can name it. create(ctx, c) builds the card;
// its update(c) draws the call again, as it goes from running to done.
// c: name, args, result (raw), res (parsed result or null), out (output
// text), diff (inline diff text), done, failed, error, startedAt and endedAt
// (Unix ms, 0 when not known), duration (s), computer, mem.
// ctx: el, highlight(code) (HTML or null), diff(text) (node), toast(text).
window.HubToolCard = (() => {
  const titleLen = 120;
  const errorLen = 80;
  const outLen = 20000;

  const isObj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
  const str = (v) => (typeof v === "string" ? v : v == null ? "" : typeof v === "number" || typeof v === "boolean" ? String(v) : "");
  const cut = (s, n) => {
    const r = Array.from(s);
    return r.length <= n ? s : r.slice(0, n).join("").trim() + "…";
  };
  const squash = (s) => str(s).split(/\s+/).filter(Boolean).join(" ");
  const short = (s) => cut(squash(s), titleLen);
  const first = (s) => {
    for (const l of str(s).split("\n")) if (l.trim()) return l.trim();
    return "";
  };
  const plural = (n, one, many) => n + " " + (n === 1 ? one : many || one + "s");
  const base = (p) => str(p).replace(/\/+$/, "").split("/").pop() || str(p);
  const lines = (s) => (s ? s.replace(/\n+$/, "").split("\n").length : 0);
  const stripAnsi = (s) => str(s).replace(/\x1b\[[0-9;]*[A-Za-z]/g, "");

  function host(u) {
    try {
      return new URL(u).host || u;
    } catch (_) {
      return str(u).replace(/^[a-z]+:\/\//i, "").split("/")[0];
    }
  }

  function memory() {
    const procs = new Map();
    const subs = new Map();
    const find = (m, id) => {
      if (!id) return "";
      if (m.has(id)) return m.get(id);
      const hits = [...m.keys()].filter((k) => k.startsWith(id) || k.replace(/^proc_/, "").startsWith(id));
      return hits.length === 1 ? m.get(hits[0]) : "";
    };
    return {
      proc: (id) => find(procs, id),
      sub: (id) => find(subs, id),
      learn(name, args, res) {
        if (!isObj(res)) return;
        if (isObj(args) && typeof res.session_id === "string" && typeof res.command === "string") procs.set(res.session_id, res.command);
        if (name === "terminal" && isObj(args) && typeof res.session_id === "string" && args.command) procs.set(res.session_id, str(args.command));
        if (name === "process_manage" && Array.isArray(res.processes)) {
          for (const p of res.processes) if (isObj(p) && p.session_id && p.command) procs.set(str(p.session_id), str(p.command));
        }
        if (name === "delegate_task") {
          if (Array.isArray(res.subagent_ids) && Array.isArray(res.goals)) {
            res.subagent_ids.forEach((id, i) => {
              if (id && res.goals[i]) subs.set(str(id), str(res.goals[i]));
            });
          }
          if (Array.isArray(res.subagents)) {
            for (const s of res.subagents) if (isObj(s) && s.subagent_id && s.goal) subs.set(str(s.subagent_id), str(s.goal));
          }
        }
      },
    };
  }

  // wrapped lists the calls that a tool_call of Hermes carries; take removes
  // the first of them named name and returns it.
  function wrapped(name, args) {
    if (name !== "tool_call" || !isObj(args) || !Array.isArray(args.calls)) return null;
    return args.calls.filter((x) => isObj(x) && typeof x.name === "string").map((x) => ({ name: x.name, args: x.arguments }));
  }

  // calls lists the cards a live tool_call draws: one for each call it
  // carries, keyed by the wrapper's id and the call's place, with the id
  // that tells the fold the calls ran together (none when there is one).
  // It is null for any other call.
  function calls(id, name, args) {
    const list = wrapped(name, args);
    if (!list || !list.length) return null;
    return list.map((x, i) => ({ key: id + ":" + i, name: x.name, args: x.args, batch: list.length > 1 ? id : "" }));
  }

  // split divides the one result Hermes ends a live tool_call with into a
  // result for each of its n calls, when it is a list of that length (or an
  // object with a results list of that length). Else it is null.
  function split(result, n) {
    let r = result;
    if (typeof r === "string" && (r[0] === "[" || r[0] === "{")) {
      try {
        r = JSON.parse(r);
      } catch (_) {
        return null;
      }
    }
    const list = Array.isArray(r) ? r : isObj(r) && Array.isArray(r.results) ? r.results : null;
    return list && list.length === n ? list : null;
  }

  function take(list, name) {
    const i = list.findIndex((x) => x.name === name);
    return i < 0 ? null : list.splice(i, 1)[0];
  }

  function counts(diff) {
    let add = 0;
    let del = 0;
    for (const l of stripAnsi(diff).split("\n")) {
      if (l.startsWith("+") && !l.startsWith("+++")) add++;
      else if (l.startsWith("-") && !l.startsWith("---")) del++;
    }
    return "+" + add + " -" + del;
  }

  const verb = (c, past, now) => (c.done ? past : now);

  function failure(c) {
    if (!c.failed) return "";
    const res = c.res;
    let e = isObj(res) && typeof res.error === "string" ? res.error : str(c.error);
    if (!e && isObj(res) && typeof res.message === "string") e = res.message;
    if (!e) e = first(c.out);
    return "Failed: " + (cut(squash(e), errorLen) || "the call did not work");
  }

  const exitCode = (res) => (isObj(res) && typeof res.exit_code === "number" ? res.exit_code : null);

  function pm(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const act = str(a.action) || "list";
    const id = str(a.session_id);
    const cmd = short(str(res.command) || (c.mem && c.mem.proc(id)) || id);
    const out = { icon: "activity", blocks: [] };
    const exited = res.status === "exited" || res.status === "already_exited" || (typeof res.exit_code === "number" && res.status !== "killed");
    const exit = exitCode(res);
    const exitText = exit === null ? "Exited" : "Exited " + exit;
    switch (act) {
      case "kill":
        out.icon = "stop";
        out.title = verb(c, "Stopped", "Stopping") + " background command " + cmd;
        out.status = res.status === "already_exited" ? exitText : "Stopped";
        break;
      case "wait": {
        out.icon = "clock";
        out.title = verb(c, "Waited for", "Waiting for") + " background command " + cmd;
        if (exited) out.status = exitText;
        else if (res.status === "interrupted") out.status = "Interrupted";
        else {
          // The timeout asked for is not how long it waited; the card's own
          // duration is. A wait that has not finished is not a success.
          out.status = "Still running" + (typeof c.duration === "number" && c.duration >= 1 ? " after waiting " + span(c.duration) : "");
          out.idle = true;
        }
        break;
      }
      case "poll":
        out.title = verb(c, "Checked", "Checking") + " background command " + cmd;
        out.status = exited ? exitText : "Still running";
        break;
      case "log": {
        out.icon = "list";
        out.title = verb(c, "Read the output of", "Reading the output of") + " background command " + cmd;
        const n = typeof res.total_lines === "number" ? res.total_lines : lines(str(res.output));
        out.status = plural(n, "line");
        break;
      }
      case "write":
      case "submit":
        out.icon = "send";
        out.title = verb(c, "Sent input to", "Sending input to") + " background command " + cmd;
        out.status = "Sent";
        break;
      case "close":
        out.title = verb(c, "Closed the input of", "Closing the input of") + " background command " + cmd;
        out.status = "Closed";
        break;
      case "handoff":
        out.icon = "send";
        out.title = verb(c, "Handed off", "Handing off") + " background command " + cmd;
        out.status = "Handed off";
        break;
      default: {
        out.icon = "list";
        out.title = verb(c, "Listed", "Listing") + " background commands";
        const list = Array.isArray(res.processes) ? res.processes : null;
        out.status = list ? plural(list.length, "command") : "Done";
      }
    }
    if (res.status === "not_found") out.status = "No such command";
    out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function delegate(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const act = str(a.action) || "spawn";
    const out = { icon: "bot", blocks: [] };
    const id = str(a.subagent_id) || str(res.subagent_id);
    const goalOf = () => short((c.mem && c.mem.sub(id)) || id) || "a subagent";
    if (act === "list") {
      out.icon = "list";
      out.title = verb(c, "Listed", "Listing") + " subagents";
      out.status = Array.isArray(res.subagents) ? plural(res.subagents.length, "subagent") + " running" : "Done";
    } else if (act === "steer") {
      out.icon = "compass";
      out.title = verb(c, "Message to subagent: ", "Sending a message to subagent: ") + goalOf();
      out.status = res.status === "queued" ? "Delivered at its next step" : str(res.status) ? squash(res.status) : "Delivered at its next step";
      if (a.message) out.blocks.push({ label: "Message", text: str(a.message), pre: true });
    } else if (act === "stop") {
      out.icon = "stop";
      out.title = verb(c, "Stopped", "Stopping") + " subagent: " + goalOf();
      out.status = "Stopped";
    } else {
      const tasks = Array.isArray(a.tasks) ? a.tasks.filter(isObj) : [];
      const goals = tasks.length ? tasks.map((t) => str(t.goal)) : [str(a.goal)];
      out.title = tasks.length > 1 ? verb(c, "Started", "Starting") + " " + tasks.length + " subagents" : verb(c, "Started", "Starting") + " subagent: " + (short(goals[0]) || "a task");
      out.status = Array.isArray(res.results) ? plural(res.results.length, "result") : "Running in the background";
      out.blocks.push({ label: goals.length > 1 ? "Goals" : "Goal", text: goals.filter(Boolean).join("\n\n"), pre: true });
      if (a.context) out.blocks.push({ label: "Context", text: str(a.context), pre: true });
    }
    if (c.failed) out.status = failure(c);
    if (act === "list" || act === "stop") out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out && !out.blocks.some((b) => b.label === "Output")) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function edit(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    let path = str(a.path) || str(a.file_path);
    if (!path && typeof a.patch === "string") {
      const m = /^\*\*\* (?:Update|Add|Delete) File: (.+)$/m.exec(a.patch);
      if (m) path = m[1].trim();
    }
    const wrote = c.name === "write_file";
    const out = { icon: wrote ? "filenew" : "edit", blocks: [] };
    out.title = (wrote ? verb(c, "Wrote", "Writing") : verb(c, "Edited", "Editing")) + " " + (path || "a file");
    const diff = c.diff || str(res.diff);
    if (diff) out.status = counts(diff);
    else if (wrote) out.status = "+" + lines(str(a.content)) + " -0";
    else if (typeof a.old_string === "string" || typeof a.new_string === "string") out.status = "+" + lines(str(a.new_string)) + " -" + lines(str(a.old_string));
    else out.status = "Done";
    if (diff) out.blocks.push({ label: "Diff", text: diff, kind: "diff" });
    else if (wrote && a.content) out.blocks.push({ label: "Content", text: str(a.content), pre: true });
    else if (typeof a.old_string === "string") {
      out.blocks.push({ label: "Diff", text: stripAnsi(["--- " + path, "+++ " + path].concat(str(a.old_string).split("\n").map((l) => "-" + l), str(a.new_string).split("\n").map((l) => "+" + l)).join("\n")), kind: "diff" });
    } else if (typeof a.patch === "string") out.blocks.push({ label: "Patch", text: a.patch, pre: true });
    if (c.out && !diff) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function search(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const out = { icon: "search", blocks: [] };
    const where = str(a.path);
    out.title = verb(c, "Searched", "Searching") + " files for " + (short(str(a.pattern)) || "files") + (where ? " in " + where : "");
    const list = Array.isArray(res.matches) ? res.matches : Array.isArray(res.files) ? res.files : null;
    if (list) {
      const paths = new Set(list.map((m) => (isObj(m) ? str(m.path || m.file) : str(m))).filter(Boolean));
      const n = typeof res.total_count === "number" ? res.total_count : list.length;
      out.status = Array.isArray(res.matches) ? n + (n === 1 ? " match" : " matches") + " in " + plural(paths.size, "file") : plural(n, "file");
    } else if (typeof res.total_count === "number") {
      out.status = plural(res.total_count, "match", "matches");
    } else {
      out.status = "Done";
    }
    out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Matches", text: c.out, pre: true });
    return out;
  }

  function read(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const out = { icon: "file", blocks: [] };
    const path = str(a.path) || str(a.file_path);
    out.title = verb(c, "Read", "Reading") + " " + (base(path) || "a file");
    const n = typeof res.content === "string" ? lines(res.content) : lines(c.out);
    out.status = plural(n, "line");
    out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: typeof res.content === "string" ? res.content : c.out, pre: true });
    return out;
  }

  function web(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const out = { icon: "globe", blocks: [] };
    if (c.name === "web_search") {
      out.title = verb(c, "Searched", "Searching") + " the web for " + (short(str(a.query)) || "a query");
      const list = isObj(res.data) && Array.isArray(res.data.web) ? res.data.web : Array.isArray(res.results) ? res.results : Array.isArray(res.web) ? res.web : null;
      out.status = list ? plural(list.length, "result") : "Done";
    } else {
      const urls = Array.isArray(a.urls) ? a.urls.map(str).filter(Boolean) : a.url ? [str(a.url)] : [];
      const h = urls.length ? host(urls[0]) : "a page";
      out.title = verb(c, "Read", "Reading") + " " + h + (urls.length > 1 ? " and " + (urls.length - 1) + " more" : "");
      const list = Array.isArray(res.results) ? res.results : null;
      out.status = plural(list ? list.length : urls.length || 1, "page");
    }
    out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function send(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const to = str(a.to);
    const out = { icon: "mail", blocks: [] };
    const label = str(res.to) || to;
    out.title = to === "parent" ? "Message to parent" : /^sa-/.test(to) ? "Message to subagent " + short(label) : "Message to chat " + short(label);
    out.status = str(res.delivery) ? upperFirst(squash(res.delivery)) : "Sent";
    if (a.text) out.blocks.push({ label: "Message", text: str(a.text), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  const upperFirst = (s) => (s ? s.charAt(0).toUpperCase() + s.slice(1) : s);

  function terminal(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const cmd = str(a.command);
    const bg = !!a.background || res.status === "yielded_to_background";
    const out = { icon: "terminal", blocks: [] };
    out.title = (bg ? verb(c, "Started in the background: ", "Starting in the background: ") : verb(c, "Ran ", "Running ")) + (short(cmd) || "a command");
    const exit = exitCode(res);
    if (bg && !c.failed) out.status = "Running in the background";
    else if (exit !== null && exit !== 0) out.status = "Exit " + exit;
    else if (exit === 0) out.status = "Exit 0";
    else out.status = "Done";
    if (exit !== null && !bg && c.computer) out.status += " on " + c.computer;
    if (cmd) out.blocks.push({ label: "Command", text: cmd, kind: "command" });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  const browserWords = {
    browser_navigate: ["globe", "Opened", "Opening", (a) => host(str(a.url))],
    browser_click: ["cursor", "Clicked", "Clicking", (a) => str(a.ref)],
    browser_type: ["cursor", "Typed into", "Typing into", (a) => str(a.ref)],
    browser_press: ["cursor", "Pressed", "Pressing", (a) => str(a.key)],
    browser_scroll: ["eye", "Scrolled the page", "Scrolling the page", () => ""],
    browser_back: ["eye", "Went back a page", "Going back a page", () => ""],
    browser_snapshot: ["eye", "Read the page", "Reading the page", () => ""],
    browser_screenshot: ["camera", "Took a screenshot", "Taking a screenshot", () => ""],
    browser_vision: ["eye", "Looked at the page", "Looking at the page", () => ""],
    browser_hover: ["cursor", "Hovered over", "Hovering over", (a) => str(a.ref)],
    browser_tabs: ["tabs", "Used the browser tabs", "Using the browser tabs", () => ""],
    browser_eval: ["globe", "Ran a script in the page", "Running a script in the page", () => ""],
  };

  function browser(c) {
    const a = isObj(c.args) ? c.args : {};
    const w = browserWords[c.name];
    const out = { icon: w ? w[0] : "globe", blocks: [] };
    if (w) out.title = (c.done ? w[1] : w[2]) + (w[3](a) ? " " + short(w[3](a)) : "");
    else out.title = verb(c, "Used", "Using") + " the browser: " + c.name.replace(/^browser_/, "").replace(/_/g, " ");
    out.status = "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  // Hermes asks before it saves some notes. A result that says so, or a
  // call the page marked pending, is waiting for the user.
  const pendingWords = new Set(["pending", "pending_approval", "awaiting_approval", "approval_required", "needs_approval", "waiting_for_approval"]);
  function waiting(c) {
    if (c.pending) return true;
    const res = isObj(c.res) ? c.res : {};
    return pendingWords.has(str(res.status)) || res.pending_approval === true || res.staged === true || res.approval_pending === true || res.requires_approval === true;
  }

  function noteBlocks(ops) {
    const blocks = [];
    for (const o of ops) {
      const act = str(o.action);
      if (act === "add") blocks.push({ label: "Added note", text: str(o.content), pre: true });
      else if (act === "remove") blocks.push({ label: "Removed note", text: str(o.old_text) || str(o.content), pre: true });
      else if (act === "replace" || act === "patch") {
        blocks.push({ label: "Edited note", text: str(o.content), pre: true });
        if (o.old_text) blocks.push({ label: act === "patch" ? "Text that changed" : "Note it replaced", text: str(o.old_text), pre: true });
      }
    }
    return blocks;
  }

  const noteWord = (n) => (n === 1 ? "a project note" : n + " project notes");

  function projectMemory(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    let ops = Array.isArray(a.operations) ? a.operations.filter(isObj) : [];
    if (!ops.length && ["add", "patch", "replace", "remove"].includes(str(a.action))) ops = [a];
    const out = { icon: "database", blocks: [] };
    const name = short(str(res.project) || str(res.project_name) || str(res.name) || base(str(res.folder) || str(res.path) || str(res.project_path) || str(res.root)) || str(c.project));
    const count = (...k) => ops.filter((o) => k.includes(str(o.action))).length;
    const added = count("add");
    const edited = count("replace", "patch");
    const removed = count("remove");
    const kinds = [added, edited, removed].filter(Boolean).length;
    if (!ops.length) {
      out.title = verb(c, "Read", "Reading") + " the notes of " + (name || "this project");
      const list = Array.isArray(res.entries) ? res.entries : Array.isArray(res.notes) ? res.notes : null;
      out.status = list ? plural(list.length, "note") : "Done";
    } else {
      let head;
      if (kinds > 1) head = verb(c, "Changed ", "Changing ") + noteWord(ops.length) + (name ? " in " + name : "");
      else if (added) head = verb(c, "Added ", "Adding ") + noteWord(added) + (name ? " to " + name : "");
      else if (edited) head = verb(c, "Edited ", "Editing ") + noteWord(edited) + (name ? " in " + name : "");
      else head = verb(c, "Removed ", "Removing ") + noteWord(removed) + (name ? " from " + name : "");
      out.title = head;
      out.status = "Saved";
      out.blocks.push(...noteBlocks(ops));
    }
    if (waiting(c)) {
      out.status = "Waiting for approval";
      out.waiting = true;
    }
    if (c.out && (waiting(c) || c.failed || !ops.length)) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function memoryTool(c) {
    const a = isObj(c.args) ? c.args : {};
    const act = str(a.action);
    const user = str(a.target) === "user";
    const what = user ? "your profile" : "memory";
    const out = { icon: "database", blocks: [] };
    if (act === "add") out.title = verb(c, "Saved a note to ", "Saving a note to ") + what;
    else if (act === "remove") out.title = verb(c, "Removed a note from ", "Removing a note from ") + what;
    else if (act === "replace") out.title = verb(c, "Updated a note in ", "Updating a note in ") + what;
    else out.title = verb(c, "Used ", "Using ") + what;
    out.status = act === "add" || act === "remove" || act === "replace" ? "Saved" : "Done";
    out.blocks.push(...noteBlocks(act ? [a] : []));
    if (waiting(c)) {
      out.status = "Waiting for approval";
      out.waiting = true;
    }
    if (!out.blocks.length && Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out && (waiting(c) || c.failed || !out.blocks.length)) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function sessionSearch(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const out = { icon: "search", blocks: [] };
    const q = short(str(a.query));
    out.title = q ? verb(c, "Searched", "Searching") + " past chats for " + q : verb(c, "Looked through", "Looking through") + " recent chats";
    const list = Array.isArray(res.results) ? res.results : Array.isArray(res.sessions) ? res.sessions : Array.isArray(res.matches) ? res.matches : null;
    out.status = list ? plural(list.length, "session") : "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  const secretKey = /pass|secret|token|code|cvc|card|number|otp|key/i;
  function scrub(v) {
    if (Array.isArray(v)) return v.map(scrub);
    if (!isObj(v)) return v;
    const o = {};
    for (const [k, x] of Object.entries(v)) o[k] = secretKey.test(k) && typeof x !== "boolean" ? "(hidden)" : scrub(x);
    return o;
  }

  function vault(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const out = { icon: "key", blocks: [] };
    const where = host(str(a.url) || str(a.host) || str(a.domain) || str(res.host) || str(res.url));
    const looking = !str(a.action) || str(a.action) === "list";
    out.title = (looking ? verb(c, "Looked for", "Looking for") : verb(c, "Used", "Using")) + " a saved login" + (where ? " on " + where : "");
    const list = Array.isArray(res.logins) ? res.logins : Array.isArray(res.entries) ? res.entries : Array.isArray(res.items) ? res.items : null;
    out.status = list ? plural(list.length, "login") : "Done";
    // Never show what the vault returned: it may hold a secret.
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(scrub(a), null, 2), pre: true });
    return out;
  }

  function mcpCall(c) {
    const a = isObj(c.args) ? c.args : {};
    const out = { icon: "plug", blocks: [] };
    const tool = [str(a.server), str(a.tool)].filter(Boolean).join(".") || str(a.name);
    if (c.name === "mcp_device_call") out.title = verb(c, "Called ", "Calling ") + (short(tool) || "a tool") + " on your computer";
    else if (c.name === "mcp_device_tools") out.title = verb(c, "Looked up", "Looking up") + " the tools of " + (short(str(a.server)) || "a server") + " on your computer";
    else out.title = verb(c, "Looked up", "Looking up") + " the servers on your computer";
    out.status = "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function toolLookup(c) {
    const a = isObj(c.args) ? c.args : {};
    const res = isObj(c.res) ? c.res : {};
    const out = { icon: "search", blocks: [] };
    out.title = verb(c, "Looked up", "Looking up") + " a tool";
    const list = Array.isArray(res.tools) ? res.tools : Array.isArray(res.matches) ? res.matches : Array.isArray(res.results) ? res.results : null;
    out.status = list ? plural(list.length, "match", "matches") : "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function wrapper(c) {
    const a = isObj(c.args) ? c.args : {};
    const list = wrapped(c.name, a);
    const out = { icon: "tool", blocks: [] };
    out.title = list && list.length ? verb(c, "Ran ", "Running ") + plural(list.length, "tool") + " in one call" : verb(c, "Used", "Using") + " a deferred tool";
    out.status = "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function skill(c) {
    const a = isObj(c.args) ? c.args : {};
    const out = { icon: "book", blocks: [] };
    const name = str(a.name) || (Array.isArray(a.operations) && isObj(a.operations[0]) ? str(a.operations[0].name) : "");
    if (c.name === "skills_list") out.title = verb(c, "Listed", "Listing") + " skills";
    else if (c.name === "skill_view") out.title = verb(c, "Read", "Reading") + " skill" + (name ? " " + name : "");
    else out.title = verb(c, "Edited", "Editing") + " skill" + (name ? " " + name : "");
    out.status = "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  function generic(c) {
    const a = isObj(c.args) ? c.args : {};
    const out = { icon: "tool", blocks: [] };
    let hint = "";
    for (const v of Object.values(a)) {
      const s = typeof v === "string" ? v : typeof v === "number" ? String(v) : Array.isArray(v) ? v.filter((x) => typeof x === "string").join(", ") : "";
      if (s) {
        hint = short(s);
        break;
      }
    }
    out.title = (c.name || "tool") + (hint ? ": " + cut(hint, 60) : "");
    out.status = "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  const handlers = {
    terminal, process_manage: pm, delegate_task: delegate, agent_send: send, search_files: search,
    read_file: read, write_file: edit, patch: edit, web_search: web, web_extract: web,
    memory: memoryTool, project_memory: projectMemory, skills_list: skill, skill_view: skill, skill_manage: skill,
    session_search: sessionSearch, browser_vault: vault, mcp_device_call: mcpCall, mcp_device_servers: mcpCall, mcp_device_tools: mcpCall,
    tool_search: toolLookup, tool_describe: toolLookup, tool_call: wrapper,
  };

  // span writes a length of time: 45 s, 3 min, 1 h 5 min.
  function span(s) {
    s = Math.round(s);
    if (s < 60) return s + " s";
    if (s < 3600) return Math.round(s / 60) + " min";
    const m = Math.round((s % 3600) / 60);
    return Math.floor(s / 3600) + " h" + (m ? " " + m + " min" : "");
  }

  function todo(c) {
    const a = isObj(c.args) ? c.args : {};
    const out = { icon: "checkcircle", blocks: [] };
    out.title = verb(c, "Updated", "Updating") + " the to-do list";
    out.status = Array.isArray(a.todos) ? plural(a.todos.length, "item") : "Done";
    if (Object.keys(a).length) out.blocks.push({ label: "Call", text: JSON.stringify(a, null, 2), pre: true });
    if (c.out) out.blocks.push({ label: "Output", text: c.out, pre: true });
    return out;
  }

  // derive gives the icon, title, status, state ("run", "ok" or "bad") and
  // blocks of a call. A running call has no status.
  function derive(c) {
    const name = str(c.name) || "tool";
    c = Object.assign({}, c, { name });
    let d;
    if (handlers[name]) d = handlers[name](c);
    else if (name.startsWith("browser_")) d = browser(c);
    else if (name === "todo" || name === "todo_list") d = todo(c);
    else d = generic(c);
    d.state = !c.done ? "run" : c.failed ? "bad" : "ok";
    const code = exitCode(isObj(c.res) ? c.res : null);
    if (c.failed && !(name === "terminal" && code !== null && code !== 0)) d.status = failure(c);
    if (!c.done) d.status = d.waiting ? "Waiting for approval" : "";
    d.title = plain(d.title);
    d.status = plain(d.status);
    d.blocks = d.blocks.filter((b) => b.text);
    for (const b of d.blocks) if (b.text.length > outLen) b.text = b.text.slice(0, outLen) + "\n...";
    return d;
  }

  // plain drops the emoji and variation selectors that Hermes puts in its
  // labels and progress text. Message text of the user and the assistant never
  // goes through it.
  const emojiRe = /[\p{Extended_Pictographic}\uFE0E\uFE0F\u200D\u20E3\u{E0020}-\u{E007F}]/gu;
  function plain(s) {
    return String(s === undefined || s === null ? "" : s).replace(emojiRe, "").replace(/ {2,}/g, " ").trim();
  }

  // batch reads the progress text of a batched call, "[set 5] terminal,
  // terminal, process_manage", into its size and its distinct tool names.
  function batch(text) {
    const m = /^\[set\s+(\d+)\]\s*(.*)$/i.exec(plain(text));
    if (!m) return null;
    const names = [];
    for (const n of m[2].split(",")) {
      const t = n.trim().split(/\s+/)[0];
      if (t && !names.includes(t)) names.push(t);
    }
    return { n: +m[1], names };
  }

  // statusLine turns what Hermes says a subagent is doing now (the tool name
  // and a short preview, which is a string and not the call's arguments)
  // into one plain sentence and an icon. A background command is named from
  // earlier calls through mem. A tool it does not know is "Using <name>",
  // never the raw text.
  const pmWords = {
    wait: ["clock", "Waiting on"], poll: ["activity", "Checking"], log: ["list", "Reading the output of"], kill: ["stop", "Stopping"],
    write: ["send", "Sending input to"], submit: ["send", "Sending input to"], close: ["activity", "Closing the input of"], handoff: ["send", "Handing off"],
  };
  const lineWords = {
    todo: ["checkcircle", "Updating the to-do list"], todo_list: ["checkcircle", "Updating the to-do list"],
    memory: ["database", "Saving to memory"], project_memory: ["database", "Updating the project notes"],
    delegate_task: ["bot", "Starting a subagent"], agent_send: ["mail", "Sending a message"],
    skills_list: ["book", "Looking at skills"], skill_view: ["book", "Reading a skill"], skill_manage: ["book", "Editing a skill"],
    tool_search: ["search", "Looking up a tool"], tool_describe: ["search", "Looking up a tool"],
    mcp_device_servers: ["plug", "Looking up the servers on your computer"], mcp_device_tools: ["plug", "Looking up tools on your computer"],
  };

  function statusLine(name, preview, mem) {
    const l = statusLine0(plain(name), plain(preview), mem);
    return { icon: l.icon, text: plain(l.text) };
  }

  function statusLine0(name, preview, mem) {
    name = str(name).trim();
    const b = batch(preview);
    if (b && (!name || name === "tool_call" || name === "tool")) name = "tool_call";
    const p = squash(preview);
    const arg = (p.split(" ")[0] || "");
    const tail = (t) => (t ? " " + short(t) : "");
    if (!name) return { icon: "tool", text: "Working" };
    if (name === "process_manage") {
      const w = p.split(" ");
      const act = pmWords[w[0]] ? w[0] : w[0] === "list" || !w[0] ? "list" : "";
      const id = /^(proc_)?[0-9a-f]{4,}$/i.test(w[1] || "") ? w[1] : /^proc_/.test(w[0] || "") ? w[0] : "";
      const cmd = id && mem ? mem.proc(id) : "";
      if (act === "list" || !act) return { icon: "list", text: "Listing background commands" };
      const [icon, word] = pmWords[act];
      return { icon, text: word + (cmd ? " background command " + short(cmd) : " a background command") };
    }
    if (name === "terminal") return { icon: "terminal", text: p ? "Running " + short(p) : "Running a command" };
    if (name === "read_file") return { icon: "file", text: "Reading " + (base(arg) || "a file") };
    if (name === "write_file" || name === "patch") return { icon: name === "write_file" ? "filenew" : "edit", text: (name === "write_file" ? "Writing " : "Editing ") + (base(arg) || "a file") };
    if (name === "search_files") return { icon: "search", text: "Searching files" + (p ? " for " + short(p) : "") };
    if (name === "web_search") return { icon: "globe", text: "Searching the web" + (p ? " for " + short(p) : "") };
    if (name === "web_extract") return { icon: "globe", text: "Reading " + (arg ? host(arg) : "a web page") };
    if (name === "session_search") return { icon: "search", text: "Searching past chats" + (p ? " for " + short(p) : "") };
    if (name === "browser_vault") return { icon: "key", text: /^(fill|use|login)\b/.test(p) ? "Using a saved login" : "Looking for a saved login" };
    if (name === "mcp_device_call") return { icon: "plug", text: "Calling " + (arg ? short(arg) : "a tool") + " on your computer" };
    if (name === "tool_call") {
      const m = /(\d+)\s+calls?/.exec(p);
      const n = b ? b.n : m ? +m[1] : 1;
      const list = b && b.names.length ? ": " + b.names.join(", ") : "";
      return { icon: "tool", text: n !== 1 ? "Running " + n + " tools" + list : "Running a tool" };
    }
    if (lineWords[name]) return { icon: lineWords[name][0], text: lineWords[name][1] };
    if (browserWords[name]) {
      const w = browserWords[name];
      return { icon: w[0], text: w[2] + tail(w[3]({ url: arg, ref: arg, key: arg })) };
    }
    if (name.startsWith("browser_")) return { icon: "globe", text: "Using the browser" };
    return { icon: "tool", text: "Using " + toolLabel(name) };
  }

  // toolLabel is a tool's name in plain words: get_thing is "get thing".
  function toolLabel(name) {
    return plain(name).replace(/[_-]+/g, " ").trim() || "a tool";
  }

  function parse(r) {
    if (isObj(r)) return r;
    if (typeof r !== "string" || r[0] !== "{") return null;
    try {
      return JSON.parse(r);
    } catch (_) {
      return null;
    }
  }

  // outcome reads a finished call's result p (result, is_error, error,
  // result_text): the parsed result, whether it failed, and the output to
  // show (a command's output with its error and exit code).
  function outcome(name, p) {
    const res = parse(p.result);
    const exit = res && typeof res.exit_code === "number" ? res.exit_code : null;
    const failed = !!(p.is_error || p.error || (res && res.error) || (exit !== null && exit !== 0));
    let out = p.result_text;
    if (!out && res && name === "terminal" && "output" in res) {
      out = String(res.output || "");
      if (res.error) out += (out ? "\n" : "") + res.error;
      if (exit !== null && exit !== 0) out += (out ? "\n" : "") + "(exit code " + exit + ")";
    }
    if (!out && p.result !== undefined && p.result !== null) out = typeof p.result === "string" ? p.result : JSON.stringify(p.result, null, 2);
    return { res, failed, out: out || "" };
  }

  function create(ctx, c0) {
    const { el } = ctx;
    const T = window.HubTimes;
    const box = el("div", "tool notice");
    const head = el("button", "notice-head");
    head.type = "button";
    head.setAttribute("aria-expanded", "false");
    const title = el("span", "notice-title");
    const meta = el("div", "notice-meta");
    const wrap = el("div", "notice-body-wrap");
    wrap.hidden = true;
    const blocks = el("div", "notice-body");
    const extra = el("div", "tool-extra");
    const live = el("div", "tool-live");
    wrap.append(blocks, extra);
    const chev = window.HubIcons.icon("chev");
    chev.classList.add("notice-chev");
    head.onclick = () => {
      const open = head.getAttribute("aria-expanded") !== "true";
      head.setAttribute("aria-expanded", open ? "true" : "false");
      wrap.hidden = !open;
    };
    box.append(head, live, wrap);
    const view = { node: box, body: extra, live, derived: null };

    function copyButton(text) {
      const b = window.HubIcons.button("copy", "Copy", "ghost ib tool-copy");
      b.onclick = async () => {
        try {
          await navigator.clipboard.writeText(text);
          ctx.toast("Copied");
        } catch (_) {
          ctx.toast("The browser did not allow copying");
        }
      };
      return b;
    }

    function drawBlock(b) {
      const sec = el("div", "notice-sec tool-sec");
      sec.append(el("b", "", b.label));
      let body;
      if (b.kind === "diff") {
        body = ctx.diff(b.text);
        body.classList.add("tool-box");
      } else {
        body = el("pre", "tool-box" + (b.kind === "command" ? " command" : ""));
        const html = b.kind === "command" && ctx.highlight ? ctx.highlight(b.text) : null;
        if (html !== null && html !== undefined) {
          body.innerHTML = html;
          body.classList.add("hljs");
        } else if (window.HubLinkify) {
          window.HubLinkify.append(body, b.text);
        } else {
          body.textContent = b.text;
        }
      }
      sec.append(body, copyButton(b.text));
      return sec;
    }

    function drawMeta(d, c) {
      meta.replaceChildren();
      if (d.state === "run") {
        const lead = el("span", d.waiting ? "notice-wait" : "notice-run", d.status || "Running");
        meta.append(lead);
        if (T && T.valid(c.startedAt)) {
          meta.append(" · started ", T.stamp(el("span"), c.startedAt), " · ", T.ticking(el("span"), c.startedAt));
        }
        return;
      }
      const lead = d.status ? el("span", "notice-" + (d.waiting ? "wait" : d.idle ? "still" : d.state), d.status) : null;
      if (T) {
        T.finished(meta, c.endedAt, c.duration, lead || undefined);
      } else if (lead) {
        meta.append(lead);
      }
    }

    view.update = (c) => {
      const d = derive(c);
      view.derived = d;
      box.dataset.state = d.state;
      box.dataset.tool = c.name || "tool";
      head.replaceChildren(window.HubIcons.icon(d.icon), title, chev, meta);
      title.textContent = d.title;
      title.title = d.title;
      drawMeta(d, c);
      blocks.replaceChildren(...d.blocks.map(drawBlock));
      return d;
    };
    view.update(c0);
    return view;
  }

  return { derive, memory, create, wrapped, take, calls, split, parse, outcome, statusLine, plain, batch };
})();
