"use strict";
// HubMiniChat shows the steps of one subagent as a small chat inside its
// Activity row: its text as reply bubbles, its thinking folded, its tool
// calls as the chat's tool cards (toolcard.js) folded into the chat's groups
// (toolgroup.js), the messages the user sent it as small bubbles that say
// whether it read them, and a Done or Failed card at the end. Steps come
// from GET .../subagents/{id}/events (typed events with seq). It follows the
// newest step while the reader is at the bottom; scrolled up, it stays put
// and a pill counts the new steps.
// The pure parts (order, steerState, steps, near, pill, end) are run by a
// node test. create(ctx) builds a view; ctx: el, toast(text),
// highlight(code), diff(text), full (no height cap).
window.HubMiniChat = (() => {
  const nearPx = 48;
  const noteLen = 160;
  const endLen = 600;

  const ms = (ts) => (typeof ts === "number" ? ts : Date.parse(ts) || 0);
  const cut = (s, n) => {
    const r = Array.from(String(s || "").trim());
    return r.length <= n ? r.join("") : r.slice(0, n).join("").trim() + "…";
  };

  // near says whether a box is scrolled to (about) its end.
  const near = (m) => m.scrollHeight - m.scrollTop - m.clientHeight < nearPx;
  const pill = (n) => n + (n === 1 ? " new step" : " new steps");

  // order sorts events for drawing from scratch: by time, then seq.
  function order(events) {
    return events.slice().sort((a, b) => ms(a.ts) - ms(b.ts) || a.seq - b.seq);
  }

  // steerState says what became of a message to the subagent. Hermes hands
  // it over with the subagent's next tool result, so a result after it means
  // the subagent read it: "read". Without one it "waits", or was "missed"
  // when the subagent ended first.
  function steerState(steer, events, ended) {
    const at = ms(steer.ts);
    if (events.some((e) => e.type === "tool_end" && ms(e.ts) >= at)) return "read";
    return ended ? "missed" : "waits";
  }

  const steerWords = { read: "read by the subagent", waits: "waits for its next step", missed: "the subagent ended before reading it" };

  // steps counts the events that add something to read, for the pill: a
  // result adds nothing new when its call is already shown.
  function steps(events, shownCalls) {
    let n = 0;
    const seen = new Set(shownCalls || []);
    for (const e of events) {
      if (e.type === "tool_start") seen.add(e.call_id);
      if (e.type === "tool_end" && e.call_id && seen.has(e.call_id)) continue;
      if (e.type === "tool_end" && e.call_id) seen.add(e.call_id);
      n++;
    }
    return n;
  }

  // end is the closing card of a subagent that ended: state ("ok", "bad",
  // "stopped"), how long it ran, its result and tokens.
  function end(r) {
    const T = window.HubTimes;
    const took = r.durationS > 0 ? T.span(r.durationS) : "";
    const head = r.state === "ok" ? "Done" + (took ? " in " + took : "")
      : r.state === "stopped" ? "Stopped" + (took ? " after " + took : "")
        : "Failed" + (took ? " after " + took : "");
    const lines = [];
    if (r.result) lines.push(cut(r.result, endLen));
    if (r.tokens) lines.push(r.tokens);
    return { head, body: lines.filter(Boolean).join("\n"), bad: r.state !== "ok" };
  }

  function create(ctx) {
    const { el } = ctx;
    const T = window.HubTimes;
    const box = el("div", "mc" + (ctx.full ? " full" : ""));
    box.setAttribute("role", "log");
    box.setAttribute("aria-label", "Subagent steps");
    box.tabIndex = 0;
    const jump = el("button", "jump");
    jump.type = "button";
    jump.hidden = true;
    jump.append(window.HubIcons.icon("chev"), el("span"));
    box.append(jump);
    const st = { events: [], calls: new Map(), steers: [], unseen: 0, ended: false, info: {}, endNode: null, known: null };
    const view = { node: box, events: () => st.events.slice() };

    const put = (node) => box.insertBefore(node, jump);
    const stamp = (at) => T.stamp(el("span", "ts"), ms(at));

    function showJump() {
      jump.hidden = st.unseen === 0;
      jump.lastChild.textContent = pill(st.unseen);
    }
    function toEnd() {
      box.scrollTop = box.scrollHeight;
      st.unseen = 0;
      showJump();
    }
    jump.onclick = toEnd;
    box.addEventListener("scroll", () => {
      if (st.unseen && near(box)) {
        st.unseen = 0;
        showJump();
      }
    });

    function card(e) {
      if (!st.known) st.known = window.HubToolCard.memory();
      const call = { name: e.name || "tool", args: e.args || null, res: null, out: "", done: false, failed: false,
        startedAt: ms(e.ts), endedAt: 0, duration: undefined, mem: st.known };
      const v = window.HubToolCard.create({ el, toast: ctx.toast || (() => {}), highlight: ctx.highlight, diff: ctx.diff || ((t) => el("pre", "", t)) }, call);
      const node = v.node;
      node.view = v;
      node.call = call;
      node.toolName = call.name;
      node.dataset.ukey = "t:" + (e.call_id || "s" + e.seq);
      node.classList.add("running");
      const c = { node, call };
      if (e.call_id) st.calls.set(e.call_id, c);
      put(node);
      return c;
    }

    function finish(c, e) {
      const { node, call } = c;
      const { res, failed, out } = window.HubToolCard.outcome(call.name, { result: e.result, is_error: e.error });
      if (!call.args && e.args) call.args = e.args;
      call.done = true;
      call.res = res;
      call.failed = failed;
      call.out = out + (e.result_size > (e.result || "").length ? "\n… (" + e.result_size + " characters in all)" : "");
      call.endedAt = ms(e.ts);
      const sec = typeof e.duration_ms === "number" ? e.duration_ms / 1000 : call.startedAt && call.endedAt >= call.startedAt ? (call.endedAt - call.startedAt) / 1000 : undefined;
      call.duration = sec;
      node.duration = sec;
      node.failed = failed;
      node.finished = true;
      node.classList.remove("running");
      node.classList.toggle("failed", failed);
      st.known.learn(call.name, call.args, res);
      node.view.update(call);
    }

    function steer(e) {
      const b = el("div", "msg user sm");
      b.textContent = e.text || "";
      const line = el("div", "queued");
      put(b);
      put(line);
      st.steers.push({ e, b, line });
    }

    function drawSteers() {
      for (const s of st.steers) {
        const state = steerState(s.e, st.events, st.ended);
        s.b.classList.toggle("pending", state === "waits");
        s.line.replaceChildren(stamp(s.e.ts), " · " + steerWords[state]);
      }
    }

    function one(e) {
      switch (e.type) {
        case "goal": {
          const n = el("div", "note");
          n.append("Started ", stamp(e.ts));
          if (st.info.model) n.append(" · " + st.info.model);
          n.title = e.text || "";
          put(n);
          break;
        }
        case "user":
          put(el("div", "note", cut(e.text, noteLen)));
          break;
        case "compaction":
          put(el("div", "note", "Earlier steps were summarized to make room."));
          break;
        case "thinking": {
          const d = el("details", "reasoning");
          const s = el("summary", "", "Thinking ");
          s.append(stamp(e.ts));
          d.append(s, el("div", "body", e.text || ""));
          put(d);
          break;
        }
        case "message": {
          const m = el("div", "msg assistant md");
          const body = el("div", "mc-text");
          if (window.HubMarkdown) window.HubMarkdown.render(body, e.text || "");
          else body.textContent = e.text || "";
          m.append(body, stamp(e.ts));
          put(m);
          break;
        }
        case "tool_start":
          if (!st.calls.has(e.call_id)) card(e);
          break;
        case "tool_end": {
          const c = (e.call_id && st.calls.get(e.call_id)) || card(e);
          finish(c, e);
          break;
        }
        case "steer":
          steer(e);
          break;
      }
    }

    // fold folds the finished calls into the chat's groups; while the
    // subagent runs, the newest calls stay open as cards, as in a chat's
    // running turn.
    function fold() {
      let keep = null;
      if (!st.ended) {
        for (let n = jump.previousElementSibling; n && n.classList.contains("tool"); n = n.previousElementSibling) keep = n;
      }
      window.HubToolGroup.fold(el, box, keep, (c) => c.view && c.querySelector(".notice-head[aria-expanded=\"true\"]"), true);
    }

    function drawEnd() {
      if (st.endNode) st.endNode.remove();
      st.endNode = null;
      if (!st.ended || !st.info.end) return;
      const r = end(st.info.end);
      const n = el("div", "mc-end" + (r.bad ? " bad" : ""));
      const text = el("div");
      text.append(el("b", "", r.head));
      if (r.body) text.append(el("p", "", r.body));
      if (st.info.end.endedAt) text.append(stamp(st.info.end.endedAt));
      n.append(window.HubIcons.icon(r.bad ? "alert" : "checkcircle"), text);
      st.endNode = n;
      put(n);
    }

    // add draws new events (in seq order) and follows them when the reader
    // is at the end.
    view.add = (events) => {
      if (!events.length) return;
      const follow = near(box) || !st.events.length;
      const shown = [...st.calls.keys()];
      // A batch can hold a message the user sent and older steps the hub
      // read after it: draw it in time order.
      for (const e of order(events)) {
        st.events.push(e);
        one(e);
      }
      if (st.endNode) put(st.endNode);
      fold();
      drawSteers();
      if (follow) toEnd();
      else {
        st.unseen += steps(events, shown);
        showJump();
      }
    };

    // reset drops everything drawn, for a transcript Hermes rewrote.
    view.reset = () => {
      for (const n of Array.from(box.children)) if (n !== jump) n.remove();
      Object.assign(st, { events: [], calls: new Map(), steers: [], unseen: 0, endNode: null, known: null });
      showJump();
    };

    // set takes what the row knows: model, and end ({ state, durationS,
    // result, tokens, endedAt }) once the subagent ended.
    view.set = (info) => {
      const ended = !!(info && info.end);
      const changed = ended !== st.ended || JSON.stringify(info.end || null) !== JSON.stringify(st.info.end || null);
      st.info = info || {};
      st.ended = ended;
      if (!changed) return;
      const follow = near(box);
      drawEnd();
      fold();
      drawSteers();
      if (follow) toEnd();
    };

    view.empty = (text) => {
      if (st.events.length || box.querySelector(".mc-empty")) return;
      put(el("div", "note mc-empty", text));
    };
    view.clearEmpty = () => {
      const n = box.querySelector(".mc-empty");
      if (n) n.remove();
    };
    return view;
  }

  return { near, pill, order, steerState, steps, end, create };
})();
