"use strict";
// HubStick keeps the reader's place in a scrolling log. A reader at the end
// stays at the end, however the log's height or its content changes. A
// reader above it stays on the same message. Only the reader moves between
// the two: scrolling away from the end, or back to within gap pixels of it,
// or pin() and release() for a click that means the same.
//
// A scroll event that nothing of the reader's caused is not the reader. A
// composer that grows lays the log out at an in-between height first; the
// browser clamps scrollTop there and fires an event, after the layout has
// settled, for a scrollTop nobody chose, and it looks like the reader
// scrolling up. The reader is known by what the reader does: heard() is
// called for a wheel, a touch, a navigation key or a press on the
// scrollbar, and only a scroll event during or soon after one counts.
// Any other change of scrollTop is undone by the next sync().
//
// o.probe(log) returns {n, off}: the first message in view and how far its
// top is below the top of the log, or null. o.place(n, log) returns that
// distance now, or null when n is gone. o.changed() runs when the state
// the reader sees may have changed.
window.HubStick = (log, o) => {
  const gapMax = o.gap || 30;
  const changed = o.changed || (() => {});
  const now = o.now || Date.now;
  const hold = o.hold || 1200;
  let until = 0;
  let pinned = true;
  let anchor = null;
  let rec = { h: 0, c: 0, top: 0 };

  const gap = () => log.scrollHeight - log.scrollTop - log.clientHeight;
  const note = () => { rec = { h: log.scrollHeight, c: log.clientHeight, top: log.scrollTop }; };
  const take = () => { anchor = pinned ? null : o.probe(log); };
  const set = (top) => { log.scrollTop = top; note(); };

  // sync is for after anything that may have changed the log's size or its
  // content, and for a log that was not shown: it puts the log back where
  // the reader is.
  function sync() {
    if (log.hidden) { changed(); return; }
    if (pinned) {
      if (gap() >= 1) set(log.scrollHeight);
      else note();
      return;
    }
    const at = anchor ? o.place(anchor.n, log) : null;
    if (at === null) {
      take();
      note();
      return;
    }
    const d = at - anchor.off;
    if (d >= 1 || d <= -1) set(log.scrollTop + d);
    else note();
  }

  // scrolled is for each scroll event of the log.
  function scrolled() {
    if (log.hidden) return;
    if (Math.abs(log.scrollTop - rec.top) < 1) return;
    if (now() >= until) { sync(); return; }
    until = Math.max(until, now() + 300);
    note();
    pinned = gap() < gapMax;
    take();
    changed();
  }

  // heard says the reader is scrolling, for ms (default hold) from now.
  function heard(ms) { until = now() + (ms || hold); }

  function pin() {
    pinned = true;
    anchor = null;
    set(log.scrollHeight);
    changed();
  }

  function release() {
    pinned = false;
    take();
    note();
    changed();
  }

  // look takes the place the log shows now, for a caller that moved it.
  function look() {
    note();
    pinned = gap() < gapMax;
    take();
    changed();
  }

  return { sync, scrolled, heard, pin, release, look, refresh: changed, pinned: () => pinned };
};
