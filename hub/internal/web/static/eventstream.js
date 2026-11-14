"use strict";
// HubBackoff is the wait between tries of something that keeps failing:
// it doubles from base up to max, and each wait is a random point in its
// upper half, so pages that lost the hub together do not return together.
// reset starts over after a success.
window.HubBackoff = (o) => {
  const base = (o && o.base) || 500;
  const max = (o && o.max) || 15000;
  const rand = (o && o.rand) || Math.random;
  let step = 0;
  return {
    next() {
      const cap = Math.min(max, base * 2 ** step);
      if (cap < max) step++;
      return Math.round(cap / 2 + (rand() * cap) / 2);
    },
    reset() { step = 0; },
    steps: () => step,
  };
};

// HubStream owns the event stream of a chat. It does not leave the
// reconnecting to the browser, which retries every 2 seconds for ever and
// gives up for good on a reconnect that got an error status. When the
// stream ends or fails it opens a new one, with the last event id as
// ?since=, after a wait that doubles while streams die young. A stream is
// healthy once it has stayed open for healthy ms: that clears the banner,
// starts the waits over and asks for one check of the hub's version. Until
// then a stream that ended counts as a failure, whatever it carried.
// o.query() may give more query text for the stream URL (the page's fp).
// h: message(m) for each event, lost() when no healthy stream came for 5
// seconds, back() when one did after lost(), healthy() when a stream is
// healthy after a failure, failed() every minute once nothing opened for
// 30 seconds. o: base and max of the waits, rand, healthy, showAfter.
window.HubStream = (url, h, o) => {
  const opt = o || {};
  const healthy = opt.healthy || 2000;
  const showAfter = opt.showAfter || 5000;
  const giveUp = opt.giveUp || 30000;
  const moveWait = 250;
  const wait = window.HubBackoff({ base: opt.base, max: opt.max, rand: opt.rand });
  let es = null;
  let dead = false;
  let lastId = "";
  let moving = false;
  let downAt = 0;
  let shown = false;
  let failedAt = 0;
  let bannerTimer = 0;
  let retryTimer = 0;
  let healthyTimer = 0;

  function down() {
    if (!downAt) downAt = Date.now();
    if (bannerTimer || shown) return;
    bannerTimer = setTimeout(() => {
      bannerTimer = 0;
      if (dead || !downAt) return;
      shown = true;
      h.lost();
    }, Math.max(0, showAfter - (Date.now() - downAt)));
  }

  function good() {
    healthyTimer = 0;
    if (dead) return;
    clearTimeout(bannerTimer);
    bannerTimer = 0;
    wait.reset();
    const was = downAt !== 0;
    downAt = 0;
    failedAt = 0;
    if (shown) {
      shown = false;
      h.back();
    }
    if (was && h.healthy) h.healthy();
  }

  function giveUpCheck() {
    if (!downAt || Date.now() - downAt < giveUp) return;
    if (failedAt && Date.now() - failedAt < 60000) return;
    failedAt = Date.now();
    if (h.failed) h.failed();
  }

  function connect() {
    retryTimer = 0;
    if (dead) return;
    let target = lastId ? url + (url.includes("?") ? "&" : "?") + "since=" + encodeURIComponent(lastId) : url;
    const more = opt.query ? opt.query() : "";
    if (more) target += (target.includes("?") ? "&" : "?") + more;
    // The stream this page opened before its scripts ran, when it is the
    // one asked for: its first events are here already.
    const early = window.HubEarlyStream ? window.HubEarlyStream.take(target) : null;
    const src = early ? early.es : new EventSource(target);
    es = src;
    src.onopen = () => {
      if (dead || es !== src) return;
      moving = false;
      clearTimeout(healthyTimer);
      healthyTimer = setTimeout(good, healthy);
    };
    src.onmessage = (m) => {
      if (dead || es !== src) return;
      if (m.lastEventId) lastId = m.lastEventId;
      h.message(m);
    };
    src.onerror = () => {
      if (dead || es !== src) return;
      src.close();
      es = null;
      clearTimeout(healthyTimer);
      healthyTimer = 0;
      down();
      giveUpCheck();
      retryTimer = setTimeout(connect, moving ? moveWait : wait.next());
    };
    if (early) {
      // After the caller has its handle (a handler may use it).
      queueMicrotask(() => {
        if (src.readyState === EventSource.OPEN) src.onopen();
        for (const m of early.got) src.onmessage(m);
        if (early.failed) src.onerror();
      });
    }
  }

  connect();
  return {
    moving: () => { moving = true; },
    close: () => {
      dead = true;
      clearTimeout(bannerTimer);
      clearTimeout(retryTimer);
      clearTimeout(healthyTimer);
      if (es) es.close();
      es = null;
    },
  };
};

// HubEarlyStream opens the event stream of the chat the page loads on
// (#/s/<backend>/<session>) as soon as this file runs, before the page
// asks who the user is and builds itself, and keeps what comes until
// HubStream takes it over. take(url) gives it to the first HubStream for
// that URL; any other URL, or none in 10 s, closes it.
window.HubEarlyStream = (() => {
  if (typeof location === "undefined" || typeof document === "undefined") return null;
  const m = /^#\/s\/([^/?#]+)\/([^/?#]+)$/.exec(location.hash);
  if (!m || document.documentElement.classList.contains("pagechat") || typeof EventSource !== "function") return null;
  const url = "/api/sessions/" + m[1] + "/" + m[2] + "/events";
  const es = new EventSource(url);
  const s = { es, got: [], failed: false };
  es.onmessage = (e) => { s.got.push(e); };
  es.onerror = () => { s.failed = true; es.close(); };
  let taken = false;
  const drop = () => { if (!taken) { taken = true; es.close(); } };
  setTimeout(drop, 10000);
  return {
    take(target) {
      if (taken) return null;
      taken = true;
      if (target !== url || s.failed) { es.close(); return null; }
      es.onmessage = null;
      es.onerror = null;
      return s;
    },
  };
})();
