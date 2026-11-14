"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR (env PERF_RATE throttles
// the CPU, PERF_REPORT=1 prints the numbers)
// The performance budget of opening chats: four long chats (1,500 rows,
// 300 tool cards each) opened once each, then switched back to six times.
// Times run from the click to the end of the frame that shows the chat's
// newest message. Their budgets grow with a calibration (a fixed layout
// job, timed in the same page), so a slow or busy machine moves both, and
// sit at about twice what was measured on a fast machine and on one four
// times slower. The page must also hold only a window of each
// long chat, and a chat switched back to must come from the page's own
// copy (snapshot.same), with the hub's part under its own budget.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);
const rate = Number(process.env.PERF_RATE) || 1;
const report = !!process.env.PERF_REPORT;
const { proc, connect } = require("../cdp.js").launch(bin, dir);

const LONG = ["p0", "p1", "p2", "p3"];
const SHORT = "ps";
const last = (c) => (c === SHORT ? "short answer 9" : c + " answer 599");
const median = (a) => { const s = [...a].sort((x, y) => x - y); return s[s.length >> 1]; };

// The page records the kinds of the chat events it gets.
const preload = `(() => {
  const E = window.EventSource;
  window.__kinds = [];
  window.EventSource = function (u, o) {
    const es = new E(u, o);
    es.addEventListener("message", (m) => { try { const d = JSON.parse(m.data); const c = u.replace(/^.*\\/sessions\\/[^/]+\\//, "").replace(/\\/events.*/, ""); window.__kinds.push(c + " " + d.kind); if (d.fp) (window.__fps = window.__fps || {})[c] = d.fp; } catch (_) { /* not JSON */ } });
    return es;
  };
  window.EventSource.prototype = E.prototype;
})()`;

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Page.addScriptToEvaluateOnNewDocument", { source: preload });
  await send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false });
  if (rate > 1) await send("Emulation.setCPUThrottlingRate", { rate });
  await c.load(url + "/auth/login?next=/%23/s/pc/" + SHORT, `document.querySelectorAll("#sessions .item").length >= 5 && document.getElementById("log").textContent.includes(${JSON.stringify(last(SHORT))})`, "the short chat");
  await c.frames(10);

  // The hub follows each long chat first, as it does a chat it has shown:
  // the first open of a chat since the hub started waits for Hermes, which
  // is not the page's time (and is slow under the race detector).
  for (const chat of LONG) {
    await ev(`(async () => {
      const ac = new AbortController();
      const r = await fetch("/api/sessions/pc/${chat}/events", { signal: ac.signal });
      const rd = r.body.getReader(); const dec = new TextDecoder(); let got = "";
      while (!got.includes('"kind":"snapshot"')) { const { value, done } = await rd.read(); if (done) break; got += dec.decode(value, { stream: true }); }
      ac.abort();
      return 1;
    })()`);
  }

  // calibration: lay out a fixed block of 1,500 rows, five times.
  const cal = [];
  for (let i = 0; i < 5; i++) {
    cal.push(await ev(`(() => {
      const box = document.createElement("div");
      box.style.cssText = "position:absolute;left:0;top:0;width:600px;visibility:hidden";
      const t0 = performance.now();
      for (let j = 0; j < 1500; j++) { const d = document.createElement("div"); d.className = "msg assistant"; d.textContent = "calibration row " + j + " with a few words to wrap in it"; box.append(d); }
      document.body.append(box);
      void box.offsetHeight;
      const ms = performance.now() - t0;
      box.remove();
      return ms;
    })()`));
    await c.frames(2);
  }
  const k = median(cal);

  const open = async (chat) => {
    const ms = await ev(`(async () => {
      const a = document.querySelector('#sessions a.go[href="#/s/pc/${chat}"]');
      if (!a) return -1;
      performance.clearResourceTimings();
      const t0 = performance.now();
      a.click();
      for (;;) {
        await new Promise((r) => requestAnimationFrame(r));
        const l = document.getElementById("log");
        if (l.querySelector(".msg") && l.textContent.includes(${JSON.stringify(last(chat))})) {
          await new Promise((r) => { const ch = new MessageChannel(); ch.port1.onmessage = r; ch.port2.postMessage(0); });
          return performance.now() - t0;
        }
        if (performance.now() - t0 > 20000) return -2;
      }
    })()`);
    assert.ok(ms > 0, `${chat} did not show its newest message (${ms})`);
    // Let the rest of the open (older rows, the list) settle before the next.
    await c.frames(30);
    return ms;
  };
  // hubTime is the hub's own time (Server-Timing hub) before the first
  // byte of a request, read from its headers; an event stream is cut off
  // once they are in.
  const hubTime = (path, q) => ev(`(async () => {
    const ac = new AbortController();
    const r = await fetch(${JSON.stringify(path)} + ${JSON.stringify(q || "")}, { signal: ac.signal, credentials: "same-origin" });
    const m = /hub;dur=([\\d.]+)/.exec(r.headers.get("Server-Timing") || "");
    ac.abort();
    return m ? Number(m[1]) : -1;
  })()`);

  const first = [];
  const back = [];
  const hubBack = [];
  const hubList = [];
  for (const chat of LONG) {
    first.push(await open(chat));
    const nodes = await ev(`document.getElementById("log").getElementsByTagName("*").length`);
    const rows = await ev(`document.querySelectorAll("#log .msg").length`);
    // The window of a long chat: its newest rows, not 1,500.
    assert.ok(nodes <= 6000 && rows <= 160, `${chat} holds ${nodes} nodes and ${rows} rows in the log; the budget is 6000 and 160`);
    await open(SHORT);
  }
  for (let i = 0; i < 6; i++) {
    const chat = LONG[i % LONG.length];
    await ev(`window.__kinds.length = 0`);
    back.push(await open(chat));
    const fp = await ev(`(() => { const k = window.__fps && window.__fps[${JSON.stringify(chat)}]; return k || ""; })()`);
    hubBack.push(await hubTime("/api/sessions/pc/" + chat + "/events", fp ? "?fp=" + fp : ""));
    hubList.push(await hubTime("/api/sessions"));
    const kinds = await ev(`window.__kinds.slice()`);
    assert.ok(kinds.includes(chat + " snapshot.same") && !kinds.includes(chat + " snapshot"),
      `switching back to ${chat} drew it again from a snapshot: ${JSON.stringify(kinds)}`);
    await open(SHORT);
  }
  const out = { rate, calibration: k, first, back, hubBack, hubList, firstRatio: median(first) / k, backRatio: median(back) / k };
  if (report) console.log(JSON.stringify(out));
  // Budgets: a fixed part (frames) and a part that grows with the
  // calibration, about twice what a fast machine and one four times
  // slower measured (back 18 and 36 ms, first open 25 to 40 and 90 ms,
  // with calibrations of 13 and 53 ms).
  const backBudget = 25 + 1.5 * k;
  const firstBudget = 25 + 2.5 * k;
  assert.ok(median(back) <= backBudget, `switching back took ${median(back).toFixed(1)} ms (median), budget ${backBudget.toFixed(1)} (calibration ${k.toFixed(1)} ms)`);
  assert.ok(median(first) <= firstBudget, `opening a long chat took ${median(first).toFixed(1)} ms (median), budget ${firstBudget.toFixed(1)} (calibration ${k.toFixed(1)} ms)`);
  // The hub answers a chat it holds, and the chat list, from memory.
  assert.ok(median(hubBack) >= 0 && median(hubBack) <= 25, `the hub took ${median(hubBack)} ms before the first byte of a chat it holds; budget 25`);
  assert.ok(median(hubList) >= 0 && median(hubList) <= 25, `the hub took ${median(hubList)} ms for the chat list; budget 25`);
  c.close();
})().catch((e) => {
  console.error(e.stack || String(e));
  proc.kill();
  process.exit(1);
});
