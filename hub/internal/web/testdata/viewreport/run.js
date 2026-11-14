"use strict";
// Drives the real hub page in headless Chrome or Firefox and checks the
// view report: a log moved off the newest message by the page itself,
// long after the chat opened and with nobody touching it, produces exactly
// one report that goes through fetch to the hub; a real wheel scroll
// produces none.
// usage: node run.js BROWSER_BIN HUB_URL PROFILE_DIR MODE
// MODE is jump (a script moves the log) or wheel (the reader scrolls up).
// Prints ok when the page's own count is right; the Go test counts the
// lines the hub logged.
const assert = require("assert");
const [bin, url, dir, mode] = process.argv.slice(2);
const kind = /firefox/.test(bin) ? "firefox" : "chrome";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const W = 1504;
const H = 1002;

const preload = `() => {
  window.__reports = [];
  window.__answers = [];
  const f0 = window.fetch;
  window.fetch = function (u, o) {
    const p = f0.apply(this, arguments);
    if (String(u).includes("/api/view-report")) {
      const b = JSON.parse(o.body);
      window.__reports.push({ kind: b.kind, keepalive: !!o.keepalive, body: o.body });
      p.then((r) => window.__answers.push({ kind: b.kind, status: r.status }), () => window.__answers.push({ kind: b.kind, status: 0 }));
    }
    return p;
  };
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
        return { ev: c.ev, nav: (u) => c.goto(u), wheel, close: c.close };
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
      return {
        ev: c.ev,
        nav: (u) => c.send("Page.navigate", { url: u }),
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
  await c.nav(url + "/auth/login?next=/#/s/pc/long1");
  await until(`location.hash === "#/s/pc/long1" && !document.querySelector("#log .loading") && document.querySelectorAll("#log .msg").length > 5`, "long1 drawn");
  await until(`window.__reports.some((r) => r.kind === "loaded")`, "the page loaded report");
  // Well past the open: the report is not only for the first seconds.
  await sleep(16500);
  const gap = () => ev(`(() => { const l = document.getElementById("log"); return Math.round(l.scrollHeight - l.scrollTop - l.clientHeight); })()`);
  assert.ok((await gap()) <= 24, "the chat is not at its newest message before the test moves it");
  const left = () => ev(`window.__reports.filter((r) => r.kind === "left")`);
  if (mode === "jump") {
    await ev(`(() => { const l = document.getElementById("log"); l.scrollTop -= 400; return 1; })()`);
    await until(`window.__reports.some((r) => r.kind === "left")`, "the view report", 10000);
    // Back to the end and away again inside 30 s: no second report.
    await sleep(600);
    await ev(`(() => { const l = document.getElementById("log"); l.scrollTop = l.scrollHeight; return 1; })()`);
    await sleep(600);
    await ev(`(() => { const l = document.getElementById("log"); l.scrollTop -= 400; return 1; })()`);
    await sleep(1500);
    const rs = await left();
    assert.strictEqual(rs.length, 1, "reports: " + rs.length);
    assert.ok(rs[0].keepalive, "the report goes with keepalive");
    const b = JSON.parse(rs[0].body);
    for (const k of ["chat", "ms", "up", "vis", "nav", "reconnect60", "loaded60", "trail", "stack", "gap"]) assert.ok(k in b, "no " + k + " in the report");
    assert.ok(b.up > 16000, "up = " + b.up);
    assert.ok(/^[0-9a-f]{8}$/.test(b.chat), "chat = " + b.chat);
    assert.ok(!rs[0].body.includes("long1"), "the report names the chat");
    await until(`window.__answers.some((a) => a.kind === "left" && a.status === 204)`, "the hub's answer");
  } else {
    const box = await ev(`(() => { const r = document.getElementById("log").getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
    await c.wheel(box.x, box.y, -1500);
    await until(`(() => { const l = document.getElementById("log"); return l.scrollHeight - l.scrollTop - l.clientHeight > 100; })()`, "the wheel to move the log");
    await sleep(2000);
    const rs = await left();
    assert.strictEqual(rs.length, 0, "a reader's scroll was reported");
  }
  const loaded = await ev(`window.__reports.filter((r) => r.kind === "loaded").length`);
  assert.strictEqual(loaded, 1, "page loaded reports: " + loaded);
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
