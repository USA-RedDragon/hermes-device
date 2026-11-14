"use strict";
// HubStick keeps the reader's place in a scrolling log. A reader at the end
// stays at the end, however the log's height or its content changes. A
// reader above it stays on the same message. Only the reader moves between
// the two: scrolling away from the end, or back to within gap pixels of it,
// or pin() and release() for a click that means the same.
//
// A scroll event that the layout caused is not the reader. A composer that
// grows lays the log out at an in-between height first; the browser clamps
// scrollTop there and fires an event, after the layout has settled, for a
// scrollTop nobody chose, and it looks like the reader scrolling up. Such an
// event comes with a change of the log's own height or a shorter content,
// or soon after one, and sync() undoes it. Every other scroll is the
// reader's, whatever moved it: heard() is called for a wheel, a touch, a
// navigation key or a press on the scrollbar, and its scrolls count for a
// while even next to a layout change; find in page, focus moving to a link
// and a jump to an anchor have no such call and count on their own. Content
// that grows below does not make a scroll the layout's: a reply streams.
//
// A chat just opened is at its end until the reader acts in it. Until
// then a scroll nobody announced is undone, whatever the layout did: the
// browser can still be moving the log for the chat shown before (a
// touchpad's fling, a smooth scroll), and that is not the reader reading
// this chat. It ends with the reader's first wheel, touch, pointer or key
// (heard), o.fresh ms (default 1000) after drawn() says the chat's
// history is on the page, or 15 s after the open.
//
// o.probe(log) returns {n, off}: the first message in view and how far its
// top is below the top of the log, or null. o.place(n, log) returns that
// distance now, or null when n is gone. o.changed() runs when the state
// the reader sees may have changed.
window.HubStick = (log, o) => {
  const gapMax = o.gap || 24;
  const changed = o.changed || (() => {});
  const now = o.now || Date.now;
  const hold = o.hold || 1200;
  let until = 0;
  let pinned = true;
  let anchor = null;
  const settle = o.settle || 600;
  let rec = { h: 0, c: 0, top: 0 };
  let resized = -Infinity;
  const freshFor = o.fresh || 1000;
  let fresh = 0;
  // acted says the reader acted in the log since the last reset.
  let acted = false;
  // trail keeps the last moves of the log since the last reset, for a
  // report of a view that left the end without the reader.
  let trail = [];
  const keep = (what) => {
    if (trail.length >= 40) trail.shift();
    trail.push({ what, t: Math.round(now() - opened), top: Math.round(log.scrollTop), h: log.scrollHeight, c: log.clientHeight, pinned });
  };
  let opened = now();

  const gap = () => log.scrollHeight - log.scrollTop - log.clientHeight;
  const note = () => {
    if (log.clientHeight !== rec.c) resized = now();
    rec = { h: log.scrollHeight, c: log.clientHeight, top: log.scrollTop };
  };
  // layoutMoved says a scroll that the reader did not announce came from the
  // layout: the log changed height, shrank, or did so just before.
  const layoutMoved = () => log.clientHeight !== rec.c || log.scrollHeight < rec.h || now() - resized < settle;
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
    if (now() < until) until = Math.max(until, now() + 300);
    else if (layoutMoved() || (pinned && now() < fresh)) { keep("undo"); sync(); return; }
    const up = log.scrollTop < rec.top;
    note();
    // Only a real distance from the end leaves it: a tap's jitter, a
    // sub-pixel scrollTop after layout, or a log that grows taller (a
    // sidebar that opens, a keyboard that closes) lowering scrollTop at the
    // end keep a reader at the end pinned. A reader above the end pins
    // again by coming back within gapMax.
    const g = gap();
    const was = pinned;
    pinned = g < gapMax && (pinned || !up || g < 1);
    if (was !== pinned) keep("scroll");
    take();
    changed();
  }

  // heard says the reader is scrolling, for ms (default hold) from now.
  function heard(ms) {
    until = now() + (ms || hold);
    fresh = 0;
    acted = true;
  }

  function pin() {
    pinned = true;
    anchor = null;
    set(log.scrollHeight);
    changed();
  }

  function release() {
    keep("release");
    pinned = false;
    take();
    note();
    changed();
  }

  // look takes the place the log shows now, for a caller that moved it.
  function look() {
    note();
    pinned = gap() < gapMax;
    keep("look");
    take();
    changed();
  }

  // reset is for a log that now shows another chat: the reader is at its
  // end, and a scroll the old chat's swipe was still heard for is not the
  // reader's (emptying the log drops scrollTop to 0).
  function reset() {
    until = 0;
    opened = now();
    fresh = opened + 15000;
    acted = false;
    trail = [];
    pin();
  }

  // drawn says the chat's history is on the page: a chat just opened is
  // the reader's to move freshFor ms from now.
  function drawn() {
    if (fresh > now()) fresh = Math.min(fresh, now() + freshFor);
  }

  // settled says the log has taken in its latest size and no layout
  // change is recent, so a scroll now is the reader's.
  const settled = () => log.clientHeight === rec.c && now() - resized >= settle && now() >= fresh;

  return { settled, sync, scrolled, heard, pin, release, look, reset, drawn, refresh: changed, pinned: () => pinned,
    acted: () => acted, trail: () => trail.slice() };
};
