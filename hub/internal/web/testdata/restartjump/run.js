"use strict";
// Drives the real hub page in headless Chrome or Firefox through a restart
// of Hermes or of the hub, with the chat open at its newest message.
// usage: node run.js BROWSER_BIN HUB_URL PROFILE_DIR POKE_URL MODE
// MODE is gw (Hermes restarts) or hub (the hub restarts). After the
// restart the page reconnects; then it switches to another chat and back.
// Asserts the open chat stays at its newest message (at most 24 px from
// the end) in every frame, through the reconnect and on the first and the
// later opens, and that the page reports no view that left the end.
const assert = require("assert");
const [bin, url, dir, poke, mode] = process.argv.slice(2);
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
    if (String(u).includes("/api/view-report")) { if (!String(o && o.body).includes('"kind":"loaded"')) window.__reports.push(o && o.body); }
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
  const install = () => ev(`(() => { window.__rec = () => { window.__stop = false; const f = []; window.__frames = f; const t0 = performance.now(); window.__t0 = t0;
    const log = document.getElementById("log");
    const snap = (why) => { f.push({ t: Math.round(performance.now() - t0), why, hash: location.hash, top: Math.round(log.scrollTop), gap: Math.round(log.scrollHeight - log.scrollTop - log.clientHeight), h: log.scrollHeight,
      loading: !!log.querySelector(".loading"), who: (window.__kinds.filter((k) => k.k.startsWith("snapshot")).pop() || {}).u || "", rows: log.querySelectorAll(".msg").length }); };
    const onScroll = () => snap("scroll");
    log.addEventListener("scroll", onScroll, { passive: true });
    window.__unrec = () => log.removeEventListener("scroll", onScroll);
    const step = () => { snap("frame"); if (!window.__stop) requestAnimationFrame(step); else window.__unrec(); };
    requestAnimationFrame(step); }; return 1; })()`);
  await install();
  const linkBox = (id) => ev(`(() => { const a = document.querySelector('#sessions a.go[href="#/s/pc/${id}"]'); if (!a) return null; a.scrollIntoView({ block: "nearest" }); const r = a.getBoundingClientRect(); return { x: r.x + Math.min(40, r.width / 2), y: r.y + r.height / 2 }; })()`);
  const drawn = (id) => `location.hash === "#/s/pc/${id}" && !document.querySelector("#log .loading") && window.__kinds.some((k) => k.u === "${id}" && k.k.startsWith("snapshot"))`;
  const fails = [];
  const check = async (what, id) => {
    await ev(`(() => { window.__stop = true; return 1; })()`);
    const f = await ev(`window.__frames`);
    const kinds = await ev(`window.__kinds.map((k) => ({ t: Math.round(k.t - window.__t0), k: k.k, u: k.u }))`);
    const flips = await ev(`window.__flips.map((k) => ({ ...k, t: Math.round(k.t - window.__t0) }))`);
    if (process.env.RESTARTJUMP_DEBUG) console.log(what, id, "events:", kinds.map((k) => k.t + ":" + k.k + ":" + k.u).join(" "), "min/max gap", Math.min(...f.map((x) => x.gap)), Math.max(...f.filter((x) => x.hash === "#/s/pc/" + id).map((x) => x.gap)));
    const mine = f.filter((x) => x.hash === "#/s/pc/" + id);
    const first = mine.findIndex((x) => !x.loading && x.rows > 0);
    const bad = first < 0 ? mine : mine.slice(first).filter((x) => x.gap > 24 && x.why === "frame");
    const last = mine[mine.length - 1];
    if (first < 0 || last.gap > 24 || bad.length > 1) {
      const at = bad.length ? mine.indexOf(bad[0]) : 0;
      fails.push({ what, id, bad: bad.length, last, kinds, flips, around: mine.slice(Math.max(0, at - 6), at + 10) });
      console.log(`${what} (${id}): ${bad.length} frames off the end, last gap ${last && last.gap}`);
    }
  };
  const record = () => ev(`(() => { window.__kinds.length = 0; window.__flips.length = 0; window.__rec(); return 1; })()`);
  const open = async (id) => {
    const p = await linkBox(id);
    assert.ok(p, "no link for " + id);
    await c.click(p.x, p.y);
    await until(drawn(id), id + " drawn");
    await sleep(2500);
  };

  await open("long1");
  await until(`window.__kinds.some((k) => k.u === "long1" && k.k === "snapshot")`, "long1's own snapshot");
  await sleep(1500);
  if (mode === "away") {
    // Chats seen before are kept by the hub; Hermes is away when they
    // are opened next, and has newer turns when it is back.
    for (const id of ["long2", "long3", "long4", "long1"]) await open(id);
    await record();
    assert.ok((await fetch(poke + "/gwdown")).ok);
    await sleep(2500);
    for (const id of ["long2", "long3", "long2"]) await open(id);
    assert.ok((await fetch(poke + "/gwup")).ok);
    await sleep(8000);
    await check("opened while Hermes was away", "long2");
    for (const id of ["long3", "long1", "long2"]) {
      await record();
      await open(id);
      await check("open " + id + " after Hermes was away", id);
    }
    if (fails.length) {
      for (const x of fails.slice(0, 4)) {
        console.log(`--- ${x.what}; events: ` + x.kinds.map((k) => k.t + ":" + k.k + ":" + k.u).join(" "));
        for (const k of x.flips) console.log(`  flip t=${k.t} ${k.how} pinned=${k.pinned} top=${k.top} gap=${k.gap} ${k.stack}`);
        for (const r of x.around) console.log(`  t=${r.t} ${r.why} top=${r.top} gap=${r.gap} h=${r.h} rows=${r.rows} loading=${r.loading}`);
      }
      assert.fail(`${fails.length} checks did not stay at the newest message`);
    }
    console.log("ok");
    c.close();
    process.exit(0);
  }
  // The restart, with long1 open at its newest message.
  await record();
  // bggw and bghub: the tab is hidden for the restart and shown after.
  const bg = mode.startsWith("bg");
  const port = bg ? (require("fs").readFileSync(dir + "/DevToolsActivePort", "utf8").split("\n")[0]).trim() : "";
  let mineId = "";
  if (bg) {
    mineId = (await (await fetch(`http://127.0.0.1:${port}/json/list`)).json()).find((t) => t.type === "page" && t.url.startsWith(url)).id;
    await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" });
    await until(`document.visibilityState === "hidden"`, "the tab to be hidden");
  }
  const res = await fetch(poke + "/" + (bg ? mode.slice(2) : mode));
  assert.ok(res.ok, "the restart failed: " + res.status);
  await sleep(6000);
  if (bg) {
    await fetch(`http://127.0.0.1:${port}/json/activate/${mineId}`);
    await until(`document.visibilityState === "visible"`, "the tab to be shown");
    await sleep(3000);
  }
  if (mode === "hidden") {
    // A real hidden tab (Chrome: another tab in front): the page reloads
    // for the new release while nobody looks, and the reader comes back.
    const port = (require("fs").readFileSync(dir + "/DevToolsActivePort", "utf8").split("\n")[0]).trim();
    const list = async () => (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
    const mine = (await list()).find((t) => t.type === "page" && t.url.startsWith(url));
    await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, { method: "PUT" });
    await until(`document.visibilityState === "hidden"`, "the tab to be hidden");
    await ev(`(() => { window.__gone = true; return 1; })()`);
    await until(`!window.__gone`, "the reload for the new release", 60000);
    await install();
    await until(`!!window.__rec`, "the recorder");
    await record();
    await sleep(500);
    await fetch(`http://127.0.0.1:${port}/json/activate/${mine.id}`);
    await until(`document.visibilityState === "visible"`, "the tab to be shown");
    await sleep(3000);
  }
  if (mode === "release") {
    // The page sees the new release when its stream is back, and reloads
    // while the tab is hidden.
    await ev(`(() => { window.__gone = true; Object.defineProperty(document, "hidden", { configurable: true, get: () => true }); document.dispatchEvent(new Event("visibilitychange")); return 1; })()`);
    await until(`!window.__gone && document.querySelectorAll('#sessions a.go[href^="#/s/pc/long"]').length >= 4`, "the reload for the new release", 60000);
    await install();
    await record();
    await sleep(100);
  }
  await check("reconnect after the " + mode + " restart", "long1");
  // The first open after the restart, and later ones.
  for (const id of ["long2", "long1", "long3", "long1", "long2"]) {
    await record();
    await open(id);
    await check("open " + id + " after the " + mode + " restart", id);
  }
  const reports = await ev(`window.__reports`);
  assert.deepStrictEqual(reports, [], "the page reported views that left the newest message");
  if (fails.length) {
    for (const x of fails.slice(0, 4)) {
      console.log(`--- ${x.what}; events: ` + x.kinds.map((k) => k.t + ":" + k.k + ":" + k.u).join(" "));
      for (const k of x.flips) console.log(`  flip t=${k.t} ${k.how} pinned=${k.pinned} top=${k.top} gap=${k.gap} ${k.stack}`);
      for (const r of x.around) console.log(`  t=${r.t} ${r.why} top=${r.top} gap=${r.gap} h=${r.h} rows=${r.rows} loading=${r.loading}`);
    }
    assert.fail(`${fails.length} checks did not stay at the newest message`);
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
