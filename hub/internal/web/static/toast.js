"use strict";
// HubToast is the page's one toast: a short note that says an action went
// through or failed, in the top right under the header. show(text) keeps the
// old toast(text) signature; show(text, {bad: true}) is for an error,
// {ok: true} for a finished action, and {name} fills "{name}" in the text
// with a target name clamped to about 40 characters, in bold, whose full
// text is in the title.
//
// How long one stays: 4 s plus 60 ms per word, double for an error. Hover or
// focus inside it pauses the clock, and after that it stays at least 2 s.
// Newest on top, at most 3 shown (the oldest goes first), and the same text
// again refreshes the toast it matches instead of stacking a copy.
//
// stack() is the model with no DOM, so a node test can drive it with a fake
// clock; the rest draws it.
window.HubToast = (() => {
  const MIN_MS = 4000;
  const PER_WORD_MS = 60;
  const ERROR_FACTOR = 2;
  const GRACE_MS = 2000;
  const MAX_VISIBLE = 3;
  const NAME_CHARS = 40;

  // duration is how long a toast with this text stays, in ms.
  function duration(text, bad) {
    const t = String(text).trim();
    const words = t ? t.split(/\s+/).length : 0;
    return (MIN_MS + PER_WORD_MS * words) * (bad ? ERROR_FACTOR : 1);
  }

  // clamp puts a name on one line of at most n characters (by code point),
  // ending in an ellipsis when it was cut.
  function clamp(s, n = NAME_CHARS) {
    const a = Array.from(String(s == null ? "" : s).split(/\s+/).filter(Boolean).join(" "));
    return a.length <= n ? a.join("") : a.slice(0, n - 1).join("").trimEnd() + "\u2026";
  }

  // stack is the list of live toasts, newest first. env: set(fn, ms),
  // clear(handle), now(), max, and change(kind, item) with kind "add",
  // "refresh" or "remove". A toast is { key, text, bad, ok, left }.
  function stack(env) {
    const max = env.max || MAX_VISIBLE;
    const items = [];
    const note = (kind, it) => { if (env.change) env.change(kind, it); };

    function run(it) {
      if (it.h || it.holds || it.gone) return;
      it.t0 = env.now();
      it.h = env.set(() => { it.h = 0; close(it); }, it.left);
    }
    function pause(it) {
      if (!it.h) return;
      env.clear(it.h);
      it.h = 0;
      // Whatever the clock had left, it stays at least the grace after the
      // pointer or focus goes.
      it.left = Math.max(GRACE_MS, it.left - (env.now() - it.t0));
    }
    function close(it) {
      const i = items.indexOf(it);
      if (i < 0) return;
      if (it.h) env.clear(it.h);
      it.h = 0;
      it.gone = true;
      items.splice(i, 1);
      note("remove", it);
    }

    function push(spec) {
      const key = (spec.bad ? "!" : "") + spec.text;
      const same = items.find((x) => x.key === key);
      if (same) {
        pause(same);
        same.left = duration(same.text, same.bad);
        same.ok = !!spec.ok;
        note("refresh", same);
        run(same);
        return same;
      }
      const it = { key, text: spec.text, bad: !!spec.bad, ok: !!spec.ok, full: spec.full || spec.text, parts: spec.parts || null, left: duration(spec.text, spec.bad), holds: 0, h: 0, t0: 0, gone: false };
      items.unshift(it);
      note("add", it);
      while (items.length > max) close(items[items.length - 1]);
      run(it);
      return it;
    }

    return {
      items,
      push,
      close,
      // hold and release count the reasons to wait (pointer over it, focus in it).
      hold(it) { it.holds++; pause(it); },
      release(it) { if (it.holds > 0) it.holds--; run(it); },
    };
  }

  let model = null;
  let box = null;
  const nodes = new Map();

  function draw(kind, it) {
    if (kind === "remove") {
      const n = nodes.get(it);
      if (n) n.remove();
      nodes.delete(it);
      return;
    }
    if (kind === "refresh") {
      const n = nodes.get(it);
      if (n) n.className = cls(it);
      return;
    }
    const n = document.createElement("div");
    n.className = cls(it);
    n.setAttribute("role", it.bad ? "alert" : "status");
    const tx = document.createElement("div");
    tx.className = "tx";
    tx.title = it.full;
    if (it.parts) {
      const b = document.createElement("b");
      b.textContent = it.parts[1];
      tx.append(it.parts[0], b, it.parts[2]);
    } else {
      tx.textContent = it.text;
    }
    const x = document.createElement("button");
    x.className = "x";
    x.type = "button";
    x.setAttribute("aria-label", "Dismiss");
    x.textContent = "\u00d7";
    x.onclick = () => model.close(it);
    n.append(tx, x);
    n.addEventListener("pointerenter", () => model.hold(it));
    n.addEventListener("pointerleave", () => model.release(it));
    n.addEventListener("focusin", () => model.hold(it));
    n.addEventListener("focusout", () => model.release(it));
    nodes.set(it, n);
    box.prepend(n);
  }

  const cls = (it) => "toast" + (it.bad ? " bad" : it.ok ? " ok" : "");

  function ensure() {
    if (box && box.isConnected) return;
    box = document.getElementById("toasts");
    if (!box) {
      box = document.createElement("div");
      box.id = "toasts";
      box.className = "toasts";
      document.body.append(box);
    }
    if (!model) {
      model = stack({
        set: (fn, ms) => setTimeout(fn, ms),
        clear: (h) => clearTimeout(h),
        now: () => Date.now(),
        change: draw,
      });
    }
  }

  // show puts a toast up. The text may hold one "{name}", which opts.name fills.
  function show(text, opts) {
    opts = opts || {};
    text = String(text == null ? "" : text);
    ensure();
    let parts = null;
    let shown = text;
    let full = text;
    const at = text.indexOf("{name}");
    if (at >= 0 && opts.name != null) {
      const short = clamp(opts.name);
      parts = [text.slice(0, at), short, text.slice(at + 6)];
      shown = parts.join("");
      full = text.slice(0, at) + String(opts.name).trim() + text.slice(at + 6);
    }
    return model.push({ text: shown, bad: !!opts.bad, ok: !!opts.ok, full, parts });
  }

  return { show, duration, clamp, stack, MAX_VISIBLE, GRACE_MS };
})();
