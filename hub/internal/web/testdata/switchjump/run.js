"use strict";
// Drives the real hub page at desktop size in headless Chrome (DevTools
// protocol) or headless Firefox (WebDriver BiDi; picked when the path has
// "firefox" in it), with real mouse clicks and wheel turns.
// usage: node run.js BROWSER_BIN HUB_URL PROFILE_DIR (env SWITCHJUMP_N,
// default 30 switches, and SWITCHJUMP_SEED for their order)
// Switches between long chats (tool cards, images, a slow gateway, so the
// hub's held copy comes first and the chat's own snapshot later) in a
// varied order and at varied moments, sometimes after reading back in the
// chat it leaves, and records the log every frame. Asserts that once the
// chosen chat is drawn it shows its newest message (at most 24 px from
// the end) in every frame, until the reader scrolls.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);
const nArg = process.env.SWITCHJUMP_N;
const seedArg = process.env.SWITCHJUMP_SEED;
const kind = /firefox/.test(bin) ? "firefox" : "chrome";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const W = 1504;
const H = 1002;

// The page records the kinds of the chat events it gets, with times.
const preload = `() => {
  const E = window.EventSource;
  window.__kinds = [];
  window.EventSource = function (u, o) {
    const s = new E(u, o);
    s.addEventListener("message", (m) => { try { const e = JSON.parse(m.data); window.__kinds.push({ t: performance.now(), k: e.kind + (e.provisional ? "*" : ""), u: String(u).split("/").slice(-2, -1)[0] }); } catch (_) { /* not ours */ } });
    return s;
  };
  window.EventSource.prototype = E.prototype;
  window.__reports = [];
  const f0 = window.fetch;
  window.fetch = function (u, o) {
    if (String(u).includes("/api/view-report")) window.__reports.push(o && o.body);
    return f0.apply(this, arguments);
  };
  window.__wheels = [];
  addEventListener("wheel", (e) => { window.__wheels.push({ t: Math.round(performance.now()), log: !!(e.target.closest && e.target.closest("#log")), y: e.clientY, dy: e.deltaY }); }, { capture: true, passive: true });
  // Records each time the log's stick lets go of the end or takes it.
  window.__flips = [];
  let made;
  Object.defineProperty(window, "HubStick", { configurable: true, get: () => made, set: (f) => {
    made = (log, o) => {
      const s = f(log, o);
      let was = s.pinned();
      for (const k of Object.keys(s)) {
        if (k === "pinned" || typeof s[k] !== "function") continue;
        const g = s[k];
        s[k] = (...a) => {
          const r = g(...a);
          if (s.pinned() !== was) {
            was = s.pinned();
            window.__flips.push({ t: performance.now(), how: k, pinned: was, top: Math.round(log.scrollTop), gap: Math.round(log.scrollHeight - log.scrollTop - log.clientHeight), stack: was ? "" : String(new Error().stack).split("\\n").slice(2, 6).join(" | ") });
          }
          return r;
        };
      }
      return s;
    };
  } });
}`;

function driver() {
  if (kind === "firefox") {
    const { proc, connect } = require("../bidi.js").launch(bin, dir);
    return {
      proc,
      connect: async () => {
        const c = await connect();
        await c.send("script.addPreloadScript", { functionDeclaration: preload });
        await c.send("browsingContext.setViewport", { context: c.context, viewport: { width: W, height: H }, devicePixelRatio: 1.5 });
        const wheel = (x, y, dy) => c.send("input.performActions", { context: c.context, actions: [{ type: "wheel", id: "w", actions: [{ type: "scroll", x: Math.round(x), y: Math.round(y), deltaX: 0, deltaY: dy }] }] });
        return { ev: c.ev, nav: (u) => c.goto(u), click: c.click, wheel, close: c.close };
      },
    };
  }
  const { proc, connect } = require("../cdp.js").launch(bin, dir);
  return {
    proc,
    connect: async () => {
      const c = await connect();
      await c.send("Page.enable");
      await c.send("Page.addScriptToEvaluateOnNewDocument", { source: "(" + preload + ")()" });
      await c.send("Emulation.setDeviceMetricsOverride", { width: W, height: H, deviceScaleFactor: 1.5, mobile: false });
      const mouse = (type, x, y) => c.send("Input.dispatchMouseEvent", { type, x, y, button: "left", buttons: type === "mousePressed" ? 1 : 0, clickCount: 1 });
      return {
        ev: c.ev,
        nav: (u) => c.send("Page.navigate", { url: u }),
        click: async (x, y) => { await mouse("mouseMoved", x, y); await mouse("mousePressed", x, y); await mouse("mouseReleased", x, y); },
        wheel: (x, y, dy) => c.send("Input.dispatchMouseEvent", { type: "mouseWheel", x, y, deltaX: 0, deltaY: dy }),
        close: c.close,
      };
    },
  };
}

// A small seeded random, so a failing order can be run again.
let seed = Number(seedArg) || 7;
const rand = () => { seed = (seed * 1103515245 + 12345) % 2147483648; return seed / 2147483648; };
const pick = (a) => a[Math.floor(rand() * a.length)];

const { proc, connect } = driver();
(async () => {
  const c = await connect();
  const { ev } = c;
  const until = async (expr, what, ms = 30000) => {
    const end = Date.now() + ms;
    for (;;) {
      const v = await ev(`(() => { try { return ${expr}; } catch (e) { return false; } })()`);
      if (v) return v;
      if (Date.now() > end) throw new Error("timed out waiting for " + what);
      await sleep(25);
    }
  };
  await c.nav(url + "/auth/login?next=/");
  await until(`document.querySelectorAll('#sessions a.go[href^="#/s/pc/long"]').length >= 4`, "the long chats in the list");
  await until(`innerWidth > 1400`, "the desktop size");
  const chats = await ev(`Array.from(document.querySelectorAll("#sessions a.go")).map((a) => a.getAttribute("href").split("/").pop()).filter((id) => id.startsWith("long"))`);
  assert.ok(chats.length >= 4, "the long chats are listed: " + JSON.stringify(chats));

  // rec records, from now until stopped, every frame and every scroll of the log.
  const install = () => ev(`(() => { window.__rec = () => { window.__stop = false; const f = []; window.__frames = f; const t0 = performance.now(); window.__t0 = t0;
    const log = document.getElementById("log");
    const snap = (why) => { f.push({ t: Math.round(performance.now() - t0), why, hash: location.hash, top: Math.round(log.scrollTop), gap: Math.round(log.scrollHeight - log.scrollTop - log.clientHeight), h: log.scrollHeight,
      loading: !!log.querySelector(".loading"), who: (window.__kinds.filter((k) => k.k.startsWith("snapshot")).pop() || {}).u || "", rows: log.querySelectorAll(".msg").length, pinned: document.getElementById("tobottom").hidden }); };
    const onScroll = () => snap("scroll");
    log.addEventListener("scroll", onScroll, { passive: true });
    window.__unrec = () => log.removeEventListener("scroll", onScroll);
    const step = () => { snap("frame"); if (!window.__stop) requestAnimationFrame(step); else window.__unrec(); };
    requestAnimationFrame(step); }; return 1; })()`);
  await install();
  const linkBox = (id) => ev(`(() => { const a = document.querySelector('#sessions a.go[href="#/s/pc/${id}"]'); if (!a) return null; a.scrollIntoView({ block: "nearest" }); const r = a.getBoundingClientRect(); return { x: r.x + Math.min(40, r.width / 2), y: r.y + r.height / 2 }; })()`);
  const logBox = () => ev(`(() => { const r = document.getElementById("log").getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
  const drawn = (id) => `location.hash === "#/s/pc/${id}" && !document.querySelector("#log .loading") && window.__kinds.some((k) => k.u === "${id}" && k.k.startsWith("snapshot"))`;

  const atEnd = `(() => { const l = document.getElementById("log"); return l.scrollHeight - l.scrollTop - l.clientHeight; })()`;

  // A new release reloads the page and puts back the view it saved: here
  // the reader was up in long3. While long3 is still loading (its Hermes
  // is slow) the reader opens long1 from the list; long1 opens at its
  // newest message, not at the place saved for long3.
  {
    const p0 = await linkBox("long3");
    await c.click(p0.x, p0.y);
    await until(drawn("long3"), "long3 drawn");
    await until(`window.__kinds.some((k) => k.k === "snapshot")`, "long3's own snapshot");
    // The hub lets go of long3 a second after nobody shows it.
    await ev(`(() => { location.hash = "#/"; return 1; })()`);
    await sleep(1800);
    await ev(`(() => { history.replaceState(null, "", "/#/s/pc/long3"); sessionStorage.setItem("hub.view", JSON.stringify({ at: Date.now(), hash: "#/s/pc/long3",
      log: { top: 4000, pinned: false, anchor: { i: 40, off: 0 } }, side: 0, details: [], act: null, files: null, settings: "", focus: null }));
      location.reload(); return 1; })()`);
    await sleep(300);
    await until(`document.querySelectorAll('#sessions a.go[href^="#/s/pc/long"]').length >= 4 && !document.documentElement.classList.contains("restoring")`, "the list after the reload");
    await until(`document.querySelector("#log .loading") || document.querySelector("#log .msg")`, "long3 opening");
    const p1 = await linkBox("long1");
    await c.click(p1.x, p1.y);
    await until(drawn("long1"), "long1 drawn after the reload");
    await until(`window.__kinds.some((k) => k.k === "snapshot.older")`, "long1's older rows");
    await sleep(800);
    const g = await ev(atEnd);
    const pill = await ev(`!document.getElementById("tobottom").hidden`);
    await install();
    assert.ok(g <= 24 && !pill, `after a reload that saved a place in long3, long1 opened ${g} px above its newest message (Jump to latest shown: ${pill})`);
  }

  const n = Number(nArg) || 30;
  let cur = null;
  const fails = [];
  for (let i = 0; i < n; i++) {
    let to = pick(chats);
    if (to === cur) to = chats[(chats.indexOf(to) + 1) % chats.length];
    // Sometimes read back in the chat being left, as a reader does.
    if (cur && rand() < 0.3) {
      const b = await logBox();
      for (let k = 0; k < 3; k++) { await c.wheel(b.x, b.y, -400); await sleep(30); }
      await sleep(pick([50, 300, 900]));
    }
    // Sometimes go through another screen first (a new chat, usage), which
    // hides the log.
    if (cur && rand() < 0.25) {
      await ev(`(() => { location.hash = ${JSON.stringify(pick(["#/", "#/usage"]))}; return 1; })()`);
      await sleep(pick([100, 600]));
    }
    await ev(`(() => { window.__kinds.length = 0; window.__flips.length = 0; window.__rec(); return 1; })()`);
    const p = await linkBox(to);
    assert.ok(p, "no link for " + to);
    // A touchpad's scroll goes on after the fingers lift: wheel events
    // keep coming, now with the pointer on the chat clicked in the list.
    const coast = cur && rand() < 0.5;
    if (coast) {
      const b = await logBox();
      await c.wheel(b.x, b.y, -120);
    }
    await c.click(p.x, p.y);
    if (coast) for (let k = 0; k < 4; k++) { await c.wheel(p.x, p.y, -60); await sleep(16); }
    const how = pick(["settled", "settled", "drawn", "early"]);
    // Switch again early (before the chat is drawn or its own snapshot is
    // here) now and then; else watch until the chat's own snapshot came.
    if (how === "early") {
      await sleep(pick([0, 50, 200, 600]));
    } else {
      await until(drawn(to), `switch ${i} to ${to}: the chat drawn`);
      if (how === "settled") await until(`window.__kinds.some((k) => k.k === "snapshot")`, `switch ${i} to ${to}: its own snapshot`, 30000);
      await sleep(pick([300, 900, 1500]));
    }
    await ev(`(() => { window.__stop = true; return 1; })()`);
    const f = await ev(`window.__frames`);
    const kinds = await ev(`window.__kinds.map((k) => ({ t: Math.round(k.t - window.__t0), k: k.k }))`);
    const flips = await ev(`window.__flips.map((k) => ({ ...k, t: Math.round(k.t - window.__t0) }))`);
    cur = to;
    if (how === "early") continue;
    const mine = f.filter((x) => x.hash === "#/s/pc/" + to);
    // The first frames can still show the chat left.
    const first = mine.findIndex((x) => !x.loading && x.rows > 0 && x.who === to);
    assert.ok(first >= 0, `switch ${i} to ${to}: never drawn; ` + f.map((x) => `${x.t}:${x.why[0]}:${x.hash.slice(7)}:${x.loading ? "L" : x.who}:${x.rows}:${x.gap}`).join(" ") + JSON.stringify(kinds));
    const bad = mine.slice(first).filter((x) => x.gap > 24 && x.why === "frame");
    // A frame between a change and the scroll that answers it can be off;
    // the view is wrong when it stays off.
    const last = mine[mine.length - 1];
    if (last.gap > 24 || bad.length > 1) {
      fails.push({ i, to, how, last, firstBad: bad[0], kinds, flips, around: mine.slice(Math.max(first, mine.indexOf(bad[0]) - 6), mine.indexOf(bad[0]) + 10) });
      console.log(`switch ${i} to ${to} (${how}${coast ? ", coasting" : ""}): ${bad.length} frames off the end, last gap ${last.gap}`);
    }
  }
  // The page reports a chat that left its end with no reader input; none
  // of these switches may.
  const reports = await ev(`window.__reports`);
  assert.deepStrictEqual(reports, [], "the page reported views that left the newest message");
  if (fails.length) {
    for (const x of fails.slice(0, 4)) {
      console.log(`--- switch ${x.i} to ${x.to} (${x.how}); events: ` + x.kinds.map((k) => k.t + ":" + k.k).join(" "));
      for (const k of x.flips) console.log(`  flip t=${k.t} ${k.how} pinned=${k.pinned} top=${k.top} gap=${k.gap} ${k.stack}`);
      for (const r of x.around) console.log(`  t=${r.t} ${r.why} top=${r.top} gap=${r.gap} h=${r.h} rows=${r.rows} loading=${r.loading} pill=${!r.pinned}`);
    }
    assert.fail(`${fails.length} of ${n} switches did not stay at the newest message`);
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
