"use strict";
// HubToolGroup folds a run of finished tool cards into one line that opens
// to show them, with the words a chat uses for its steps ("Ran 3 commands,
// read a file (12s)"). The chat and the subagent view in Activity share it.
// summary(cards) is the pure part, run by a node test: a card is
// { toolName, duration (s), failed, call: { startedAt, endedAt, duration } }
// (times in Unix ms). create(el, cards) builds the group; fold(el, parent,
// keep, open, join) folds the runs of finished cards among parent's
// children; refresh(group) writes a group's line again after cards joined.
window.HubToolGroup = (() => {
  const words = {
    terminal: ["ran a command", "ran %d commands"],
    read_file: ["read a file", "read %d files"],
    write_file: ["edited a file", "edited %d files"],
    patch: ["edited a file", "edited %d files"],
    search_files: ["searched files", "searched files %d times"],
    web_search: ["searched the web", "searched the web %d times"],
    web_extract: ["read a web page", "read %d web pages"],
    browser_snapshot: ["read a tab", "read %d tabs"],
    todo: ["updated the to-do list", "updated the to-do list %d times"],
    process_manage: ["managed a background command", "managed background commands %d times"],
    delegate_task: ["used a subagent", "used subagents %d times"],
    agent_send: ["sent a message", "sent %d messages"],
    memory: ["used memory", "used memory %d times"],
    skill_view: ["used a skill", "used skills %d times"],
    skill_manage: ["used a skill", "used skills %d times"],
    skills_list: ["used a skill", "used skills %d times"],
  };

  const word = (name) => words[name] || (String(name).startsWith("browser_") ? ["used the browser", "used the browser %d times"] : ["used " + name, "used " + name + " %d times"]);

  // summary is the group's line: what the cards did, their total time and
  // the span of time they ran, and how many failed.
  function summary(cards) {
    const T = window.HubTimes;
    const counts = new Map();
    let total = 0;
    let failed = 0;
    let from = 0;
    let to = 0;
    for (const c of cards) {
      const w = word(c.toolName || "tool");
      const key = w[0];
      counts.set(key, { w, n: (counts.get(key)?.n || 0) + 1 });
      if (typeof c.duration === "number") total += c.duration;
      if (c.failed) failed++;
      const at = c.call || {};
      const end = at.endedAt || at.startedAt || 0;
      const begin = at.startedAt || (end && typeof at.duration === "number" ? end - at.duration * 1000 : end);
      if (T.valid(begin)) from = from ? Math.min(from, begin) : begin;
      if (T.valid(end)) to = Math.max(to, end);
    }
    const parts = Array.from(counts.values()).map(({ w, n }) => (n === 1 ? w[0] : w[1].replace("%d", n)));
    let text = parts.join(", ");
    text = text[0].toUpperCase() + text.slice(1);
    if (total) text += " (" + T.span(total) + ")";
    const span = T.range(from, to || from);
    if (span) text += " \u00B7 " + span;
    return { text, failed };
  }

  function line(el, d, cards) {
    const s = summary(cards);
    const sum = el("summary", "", s.text);
    if (s.failed) sum.append(el("span", "bad", s.failed === 1 ? " 1 step failed" : " " + s.failed + " steps failed"));
    d.classList.toggle("failed", s.failed > 0);
    return sum;
  }

  function create(el, cards) {
    const d = el("details", "toolgroup");
    if (cards[0].dataset && cards[0].dataset.ukey) d.dataset.ukey = "g:" + cards[0].dataset.ukey;
    d.cards = cards.slice();
    d.append(line(el, d, cards), el("div", "group-body"));
    return d;
  }

  // refresh writes the line of group d again from its cards.
  function refresh(el, d) {
    d.firstChild.replaceWith(line(el, d, d.cards));
  }

  const finished = (n) => n.classList.contains("tool") && !n.classList.contains("running");

  // fold folds each run of finished tool cards among parent's children
  // into one group. Cards from keep on stay as they are; open(card) opens
  // the group of a card the reader had open. With join, a run right after
  // a group joins that group instead of starting another.
  function fold(el, parent, keep, open, join) {
    let run = [];
    const flush = () => {
      if (!run.length) return;
      const prev = run[0].previousElementSibling;
      if (join && prev && prev.matches("details.toolgroup") && prev.cards) {
        prev.cards.push(...run);
        prev.lastChild.append(...run);
        refresh(el, prev);
        if (open && run.some(open)) prev.open = true;
      } else {
        const g = create(el, run);
        if (open && run.some(open)) g.open = true;
        parent.insertBefore(g, run[0]);
        g.lastChild.append(...run);
      }
      run = [];
    };
    let live = false;
    for (const n of Array.from(parent.children)) {
      if (n === keep) live = true;
      if (!live && finished(n)) run.push(n);
      else flush();
    }
    flush();
  }

  return { words, word, summary, create, refresh, fold };
})();
