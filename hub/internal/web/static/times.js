"use strict";
// HubTimes writes the times of a chat in the browser's time zone and in
// 12-hour form: "2:14 PM" for today, "Wed Nov 11, 11:52 PM" for an earlier
// day (with the year when it is not this one), the full "Thu Nov 12 2026,
// 2:14:07 PM CST" for a hover or a long press, "329 ms" or "4 min" for a
// run length, and "3:12" for a live counter. Times are Unix milliseconds. An
// optional zone (an IANA name) and now serve tests.
window.HubTimes = (() => {
  const locale = "en-US";
  const cache = new Map();

  function fmt(zone, key, options) {
    const id = zone + "|" + key;
    let f = cache.get(id);
    if (!f) {
      f = new Intl.DateTimeFormat(locale, Object.assign({ timeZone: zone || undefined }, options));
      cache.set(id, f);
    }
    return f;
  }

  const parts = (f, ms) => {
    const out = {};
    for (const p of f.formatToParts(ms)) out[p.type] = p.value;
    return out;
  };

  const dayParts = (ms, zone) => parts(fmt(zone, "day", { year: "numeric", month: "numeric", day: "numeric" }), ms);

  function sameDay(a, b, zone) {
    const x = dayParts(a, zone);
    const y = dayParts(b, zone);
    return x.year === y.year && x.month === y.month && x.day === y.day;
  }

  // hm is "2:14 PM".
  function hm(ms, zone) {
    const p = parts(fmt(zone, "hm", { hour: "numeric", minute: "2-digit", hour12: true }), ms);
    return p.hour + ":" + p.minute + " " + p.dayPeriod.toUpperCase();
  }

  const valid = (ms) => typeof ms === "number" && isFinite(ms) && ms > 0;

  // clock is the time of day for a time today and the weekday, date and
  // time for an earlier day.
  function clock(ms, now, zone) {
    if (!valid(ms)) return "";
    if (!valid(now)) now = Date.now();
    if (ms >= now || sameDay(ms, now, zone)) return hm(ms, zone);
    const p = parts(fmt(zone, "date", { weekday: "short", month: "short", day: "numeric", year: "numeric" }), ms);
    const year = dayParts(ms, zone).year === dayParts(now, zone).year ? "" : " " + p.year;
    return p.weekday + " " + p.month + " " + p.day + year + ", " + hm(ms, zone);
  }

  // range is the time span of a run of work: "2:14 to 2:15 PM", the day
  // once for a span that stays in one day, and each end whole for a span that
  // does not. A span of one minute reads as that minute.
  function range(a, b, now, zone) {
    const x = clock(a, now, zone);
    const y = clock(b, now, zone);
    if (!x || !y || x === y) return x || y;
    const split = (s) => {
      const k = s.split(" ");
      return { day: k.slice(0, -2).join(" "), min: k[k.length - 2], per: k[k.length - 1] };
    };
    const p = split(x);
    const q = split(y);
    if (p.day !== q.day) return x + " to " + y;
    return (p.day ? p.day + " " : "") + p.min + (p.per === q.per ? "" : " " + p.per) + " to " + q.min + " " + q.per;
  }

  // full is the time to the second, with the date and the zone.
  function full(ms, zone) {
    if (!valid(ms)) return "";
    const d = parts(fmt(zone, "full", { weekday: "short", month: "short", day: "numeric", year: "numeric" }), ms);
    const t = parts(fmt(zone, "fullt", { hour: "numeric", minute: "2-digit", second: "2-digit", hour12: true, timeZoneName: "short" }), ms);
    return d.weekday + " " + d.month + " " + d.day + " " + d.year + ", " + t.hour + ":" + t.minute + ":" + t.second + " " +
      t.dayPeriod.toUpperCase() + " " + t.timeZoneName;
  }

  // span writes a run length given in seconds: 329 ms, 3.2 s, 12 s, 4 min,
  // 4 min 5 s, 1 h 3 min.
  function span(sec) {
    if (typeof sec !== "number" || !isFinite(sec) || sec < 0) return "";
    if (sec < 1) return Math.max(1, Math.round(sec * 1000)) + " ms";
    if (sec < 10) return sec.toFixed(1) + " s";
    if (sec < 60) return Math.round(sec) + " s";
    const s = Math.round(sec);
    if (s < 3600) return Math.floor(s / 60) + " min" + (s % 60 ? " " + (s % 60) + " s" : "");
    const m = Math.round(s / 60);
    return Math.floor(m / 60) + " h" + (m % 60 ? " " + (m % 60) + " min" : "");
  }

  // counter writes the elapsed milliseconds as 3:12, or 1:03:12 from an hour.
  function counter(ms) {
    const s = Math.max(0, Math.floor(ms / 1000));
    const pad = (n) => String(n).padStart(2, "0");
    if (s < 3600) return Math.floor(s / 60) + ":" + pad(s % 60);
    return Math.floor(s / 3600) + ":" + pad(Math.floor((s % 3600) / 60)) + ":" + pad(s % 60);
  }

  // stamp makes node show the time ms and its full form on hover. A time that
  // is not known leaves the node empty.
  function stamp(node, ms) {
    if (!valid(ms)) {
      node.textContent = "";
      node.removeAttribute("title");
      delete node.dataset.at;
      return node;
    }
    node.textContent = clock(ms);
    node.title = full(ms);
    node.dataset.at = String(ms);
    return node;
  }

  const live = new Set();
  let timer = 0;

  function tick() {
    for (const n of live) {
      if (!n.isConnected) {
        live.delete(n);
        continue;
      }
      n.textContent = counter(Date.now() - Number(n.dataset.since));
    }
    if (!live.size) {
      clearInterval(timer);
      timer = 0;
    }
  }

  // ticking makes node count the time since ms, once a second, until it
  // leaves the page.
  function ticking(node, ms) {
    node.dataset.since = String(ms);
    node.textContent = counter(Date.now() - ms);
    live.add(node);
    if (!timer) timer = setInterval(tick, 1000);
    return node;
  }

  // running fills node with "started 2:20 PM · running 3:12". The counter
  // runs until node leaves the page.
  function running(node, ms) {
    const when = stamp(document.createElement("span"), ms);
    if (!valid(ms)) {
      node.replaceChildren();
      return node;
    }
    const count = ticking(document.createElement("span"), ms);
    node.replaceChildren("started ", when, " · running ", count);
    return node;
  }

  // finished fills node with "2:14 PM · 329 ms": when it ended and, when
  // known, how long it ran. With neither it is left empty.
  function finished(node, ms, sec, lead) {
    const kids = lead ? [lead] : [];
    const run = span(sec);
    if (valid(ms)) kids.push(stamp(document.createElement("span"), ms));
    if (run) kids.push(run);
    const out = [];
    kids.forEach((k, i) => {
      if (i) out.push(" · ");
      out.push(k);
    });
    node.replaceChildren(...out);
    return node;
  }

  // press calls show(text) with the full time of a time under a finger held
  // for half a second, for a phone, which has no hover.
  function press(root, show) {
    let held = 0;
    const stop = () => {
      clearTimeout(held);
      held = 0;
    };
    root.addEventListener("touchstart", (e) => {
      const n = e.target && e.target.closest ? e.target.closest("[data-at]") : null;
      stop();
      if (n) held = setTimeout(() => show(n.title), 500);
    }, { passive: true });
    for (const ev of ["touchend", "touchmove", "touchcancel", "scroll"]) root.addEventListener(ev, stop, { passive: true, capture: true });
  }

  return { clock, range, full, span, counter, stamp, ticking, running, finished, press, valid };
})();
