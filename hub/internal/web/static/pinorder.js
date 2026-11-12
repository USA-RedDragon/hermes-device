"use strict";
// HubPinOrder lets the user reorder pinned chats: drag a row with the
// mouse, press and hold a row on a touch screen, or use Move up and Move
// down in the row's menu. It uses pointer events, not the HTML5 drag API,
// so one code path serves mouse, pen and touch.
// ctx: api, el, toast(text), banner(text, bad), pinned() (the pinned
// chats in the order shown), redraw(), refresh(), nav() (the scrolling
// list), onMoved(s, byKeyboard) (called after a redraw for a chat that
// moved).
window.HubPinOrder = (ctx) => {
  const { api, el } = ctx;
  const HOLD_MS = 400; // a touch must rest this long before a drag starts
  const SLOP_MOUSE = 5; // px a mouse moves before a click becomes a drag
  const SLOP_TOUCH = 10; // px a finger may drift during the hold
  const EDGE = 48; // px from the list's edge where it scrolls
  const MAX_SCROLL = 14; // px per frame at the very edge

  let drag = null; // the gesture in progress, from press to release
  let held = false; // a drag just ended: swallow the click that follows
  let redrawLater = false;

  const keyOf = (s) => s.backend_id + "\n" + s.id;
  const active = () => !!(drag && drag.on);

  // save stores the new order. Positions are set locally first so the
  // list shows it at once; a failure reloads what the server has.
  async function save(list) {
    list.forEach((s, i) => { s.pin_pos = i; });
    try {
      await api("PUT", "/api/pins/order", { order: list.map((s) => ({ backend_id: s.backend_id, session_id: s.id })) });
    } catch (e) {
      ctx.banner("Could not save the order: " + e.message, true);
    }
    ctx.refresh();
  }

  // move shifts chat s by delta places among the pinned chats.
  async function move(s, delta) {
    const list = ctx.pinned().slice();
    const i = list.findIndex((x) => keyOf(x) === keyOf(s));
    const j = i + delta;
    if (i < 0 || j < 0 || j >= list.length) return;
    list.splice(j, 0, list.splice(i, 1)[0]);
    ctx.toast("Moved " + (delta < 0 ? "up" : "down") + ": place " + (j + 1) + " of " + list.length);
    ctx.redraw();
    ctx.onMoved(s, true);
    await save(list);
  }

  // items are the Move up and Move down entries for a chat's menu: none
  // unless the chat is pinned.
  function items(s, close) {
    if (!s.pinned) return [];
    const list = ctx.pinned();
    const i = list.findIndex((x) => keyOf(x) === keyOf(s));
    if (i < 0) return [];
    const b = (text, delta, off) => {
      const x = el("button", "", text);
      x.type = "button";
      x.setAttribute("role", "menuitem");
      x.disabled = off;
      x.onclick = (e) => { e.stopPropagation(); close(); move(s, delta); };
      return x;
    };
    return [b("Move up", -1, i === 0), b("Move down", 1, i === list.length - 1)];
  }

  const rowsOf = (group) => Array.from(group.querySelectorAll(":scope > .item"));

  function lineAt(group, rows, index) {
    const g = group.getBoundingClientRect();
    let y;
    if (!rows.length) y = 0;
    else if (index >= rows.length) y = rows[rows.length - 1].getBoundingClientRect().bottom - g.top;
    else y = rows[index].getBoundingClientRect().top - g.top;
    drag.line.style.top = Math.round(y - 1) + "px";
  }

  // Where the dragged row would land: how many of the other rows lie
  // above the pointer.
  function targetIndex() {
    let n = 0;
    for (const r of drag.others) {
      const b = r.getBoundingClientRect();
      if (drag.y > b.top + b.height / 2) n++;
    }
    return n;
  }

  function place() {
    const nav = ctx.nav();
    const raw = drag.y - drag.startY + (nav.scrollTop - drag.startScroll);
    const dy = Math.min(drag.maxDy, Math.max(drag.minDy, raw));
    drag.row.style.transform = "translateY(" + dy + "px)";
    drag.target = targetIndex();
    lineAt(drag.group, drag.others, drag.target);
  }

  function frame() {
    drag.raf = 0;
    if (!active()) return;
    const nav = ctx.nav();
    const b = nav.getBoundingClientRect();
    let v = 0;
    if (drag.y < b.top + EDGE) v = -MAX_SCROLL * Math.min(1, (b.top + EDGE - drag.y) / EDGE);
    else if (drag.y > b.bottom - EDGE) v = MAX_SCROLL * Math.min(1, (drag.y - (b.bottom - EDGE)) / EDGE);
    if (v) {
      const before = nav.scrollTop;
      nav.scrollTop += v;
      if (nav.scrollTop !== before) place();
      drag.raf = requestAnimationFrame(frame);
    }
  }

  function begin() {
    const d = drag;
    d.on = true;
    d.others = rowsOf(d.group).filter((r) => r !== d.row);
    d.line = el("div", "pin-drop");
    d.line.setAttribute("aria-hidden", "true");
    d.group.append(d.line);
    d.row.classList.add("lifting");
    document.body.classList.add("pin-dragging");
    d.startScroll = ctx.nav().scrollTop;
    d.startY = d.y;
    // The row stays among the pins: a row moved past them would also
    // stretch the list's scroll area.
    const all = rowsOf(d.group);
    const me = d.row.getBoundingClientRect();
    d.minDy = all[0].getBoundingClientRect().top - me.top;
    d.maxDy = all[all.length - 1].getBoundingClientRect().bottom - me.bottom;
    if (navigator.vibrate) { try { navigator.vibrate(12); } catch (_) { /* not allowed here */ } }
    place();
    frame();
  }

  function stop(commit) {
    const d = drag;
    if (!d) return;
    drag = null;
    clearTimeout(d.timer);
    cancelAnimationFrame(d.raf);
    window.removeEventListener("pointermove", onMove, true);
    window.removeEventListener("pointerup", onUp, true);
    window.removeEventListener("pointercancel", onCancel, true);
    window.removeEventListener("keydown", onKey, true);
    if (!d.on) return;
    held = true;
    setTimeout(() => { held = false; }, 60);
    d.row.classList.remove("lifting");
    d.row.style.transform = "";
    d.line.remove();
    document.body.classList.remove("pin-dragging");
    const list = ctx.pinned().slice();
    const from = list.findIndex((s) => keyOf(s) === d.key);
    if (commit && from >= 0 && d.target !== from) {
      list.splice(d.target, 0, list.splice(from, 1)[0]);
      ctx.redraw();
      ctx.onMoved(list[d.target], false);
      save(list);
    } else if (redrawLater) {
      ctx.redraw();
    }
    redrawLater = false;
  }

  function onMove(e) {
    if (!drag || e.pointerId !== drag.id) return;
    drag.y = e.clientY;
    if (!drag.on) {
      const moved = Math.hypot(e.clientX - drag.x0, e.clientY - drag.y0);
      if (drag.touch) {
        if (moved > SLOP_TOUCH) stop(false);
      } else if (moved > SLOP_MOUSE) {
        begin();
      }
      return;
    }
    e.preventDefault();
    place();
    if (!drag.raf) drag.raf = requestAnimationFrame(frame);
  }

  const onUp = (e) => { if (drag && e.pointerId === drag.id) stop(true); };
  const onCancel = (e) => { if (drag && e.pointerId === drag.id) stop(false); };
  const onKey = (e) => {
    if (e.key === "Escape" && active()) { e.preventDefault(); e.stopPropagation(); stop(false); }
  };

  function down(e, group) {
    if (drag || (e.pointerType === "mouse" && e.button !== 0) || e.isPrimary === false) return;
    const row = e.target.closest && e.target.closest(".item");
    if (!row || row.parentNode !== group || e.target.closest(".more")) return;
    const touch = e.pointerType === "touch";
    drag = { id: e.pointerId, group, row, key: row.dataset.key, touch, on: false, x0: e.clientX, y0: e.clientY, y: e.clientY,
      target: 0, raf: 0, timer: 0 };
    if (touch) drag.timer = setTimeout(() => { if (drag && !drag.on) begin(); }, HOLD_MS);
    window.addEventListener("pointermove", onMove, true);
    window.addEventListener("pointerup", onUp, true);
    window.addEventListener("pointercancel", onCancel, true);
    window.addEventListener("keydown", onKey, true);
  }

  // The click that ends a drag must not open the chat.
  document.addEventListener("click", (e) => {
    if (held) { e.preventDefault(); e.stopPropagation(); }
  }, true);

  // enable makes the rows of the Pinned group draggable.
  function enable(group) {
    group.addEventListener("pointerdown", (e) => down(e, group));
    group.addEventListener("dragstart", (e) => e.preventDefault());
    group.addEventListener("contextmenu", (e) => { if (drag && (drag.touch || drag.on)) e.preventDefault(); });
    // A touch can only be kept from scrolling the list in touchmove, and
    // only a handler that is not passive may do it.
    group.addEventListener("touchmove", (e) => { if (active() && e.cancelable) e.preventDefault(); }, { passive: false });
  }

  // defer holds a redraw back while a drag runs; it returns true then.
  function defer() {
    if (!active()) return false;
    redrawLater = true;
    return true;
  }

  return { enable, items, active, defer };
};
