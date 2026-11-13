"use strict";
// HubReconcile decides what a page keeps of itself when a snapshot
// replaces what it shows, so that the chat ends up as a fresh load of it
// would show it, plus the messages this page is still sending and the
// place the reader scrolled to.
window.HubReconcile = (() => {
  // idOf is the ID the page knows a pending input of the snapshot by: the
  // one it sent it under, or the hub's own for an input from elsewhere.
  const idOf = (p) => p.submission_id || p.id;

  // A message the hub accepted stays on screen this long when a snapshot
  // does not show it yet: Hermes stores a steered or queued message only
  // when its turn ends, and the hub's own copy of the chat, or another
  // replica's, may not know it.
  const keepFor = 10 * 60 * 1000;

  // carry tells what to do with each message this page has sent and the
  // snapshot does not account for: "done" when the snapshot's transcript
  // has it (it started), "adopt" with the index of its entry in the
  // snapshot's pending list, or "again" when the snapshot knows nothing of
  // it and the page shows it again as sending. That holds for a message
  // still in flight and for one the hub accepted lately (a steer, a queued
  // or held message): the snapshot is the hub's word for it only when it
  // lists it or shows it started. A message the hub refused is not carried.
  function carry(local, snap, now) {
    const started = new Set();
    const typed = new Set();
    const msgs = snap.messages || [];
    for (const m of msgs) if (m && m.submission_id) started.add(m.submission_id);
    for (const m of msgs.slice(-60)) if (m && m.role === "user" && typeof m.text === "string") typed.add(m.text.trim());
    const pending = snap.pending || [];
    const at = now === undefined ? Date.now() : now;
    const out = [];
    for (const p of local) {
      const lately = !!p.id && !!p.at && at - p.at < keepFor;
      if (!p.inflight && !lately) continue;
      const i = pending.findIndex((x) => idOf(x) === p.id || x.id === p.id);
      if (started.has(p.id) || (!p.inflight && typed.has(String(p.text || "").trim()))) out.push({ p, act: "done" });
      else if (i >= 0) out.push({ p, act: "adopt", at: i });
      else out.push({ p, act: "again" });
    }
    return out;
  }

  // anchor remembers where a reader who is not at the end of the log is:
  // the message row in view and where it starts (place), and the distance
  // to the end of the log for a page that has no row to name. It is null
  // for a reader at the end.
  const anchor = (log, pinned, place) => (pinned ? null : { dist: log.scrollHeight - log.scrollTop, place: place || null });

  // restore scrolls the log back to an anchor, and reports whether it did.
  // find gives the place's row's distance from the top of the log now, or
  // null when the row is not there; then the distance to the end stands.
  function restore(log, kept, find) {
    if (kept === null) return false;
    const at = kept.place && find ? find(kept.place) : null;
    if (at === null || at === undefined) log.scrollTop = Math.max(0, log.scrollHeight - kept.dist);
    else log.scrollTop += at - kept.place.off;
    return true;
  }

  return { idOf, carry, anchor, restore };
})();
