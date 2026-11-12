"use strict";
// HubReconcile decides what a page keeps of itself when a snapshot
// replaces what it shows, so that the chat ends up as a fresh load of it
// would show it, plus the messages this page is still sending and the
// place the reader scrolled to.
window.HubReconcile = (() => {
  // idOf is the ID the page knows a pending input of the snapshot by: the
  // one it sent it under, or the hub's own for an input from elsewhere.
  const idOf = (p) => p.submission_id || p.id;

  // carry tells what to do with each message this page has sent and has no
  // answer for yet (inflight): "done" when the snapshot's transcript has it
  // (it started), "adopt" with the index of its entry in the snapshot's
  // pending list, or "again" when the snapshot knows nothing of it and the
  // page shows it again as sending. A message the hub answered is not
  // carried: the snapshot is the hub's word for it.
  function carry(local, snap) {
    const started = new Set();
    for (const m of snap.messages || []) if (m && m.submission_id) started.add(m.submission_id);
    const pending = snap.pending || [];
    const out = [];
    for (const p of local) {
      if (!p.inflight) continue;
      const at = pending.findIndex((x) => idOf(x) === p.id || x.id === p.id);
      if (started.has(p.id)) out.push({ p, act: "done" });
      else if (at >= 0) out.push({ p, act: "adopt", at });
      else out.push({ p, act: "again" });
    }
    return out;
  }

  // anchor remembers where a reader who is not at the end of the log is:
  // the distance to its end, which stays the same when the same rows are
  // drawn again. It is null for a reader at the end.
  const anchor = (log, pinned) => (pinned ? null : log.scrollHeight - log.scrollTop);

  // restore scrolls the log back to an anchor, and reports whether it did.
  function restore(log, kept) {
    if (kept === null) return false;
    log.scrollTop = Math.max(0, log.scrollHeight - kept);
    return true;
  }

  return { idOf, carry, anchor, restore };
})();
