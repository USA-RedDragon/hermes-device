"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Asserts, on a phone: switching chats right after a swipe never shows
// the Jump to latest pill or the New chat empty state (the chosen chat's
// title and a loading state show until its rows come); a tap, a long press
// or a tap on the message box never shows the pill; a big chat draws its
// newest rows from the first part of a split snapshot and the rest later.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 412, height: 915, deviceScaleFactor: 2.625, mobile: true });
  await send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 5 });
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the chat list");

  const box = async (sel) => ev(`(() => { const n = document.querySelector(${JSON.stringify(sel)}); if (!n) return null; const r = n.getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2, w: r.width }; })()`);
  // A press of holdMs is the input under test (a long press), not a wait.
  const hold = (ms) => new Promise((r) => setTimeout(r, ms));
  const touch = async (x, y, holdMs, jitter) => {
    await send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
    // A finger never holds still: it drifts a few pixels, a pixel a frame.
    for (let k = 1; k <= jitter; k++) { await c.frames(1); await send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x + k / 2, y: y - k }] }); }
    await hold(holdMs);
    await send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  };
  const tap = async (sel, holdMs = 60, jitter = 0) => {
    await c.steady(`document.querySelector(${JSON.stringify(sel)})`, sel + " to hold still");
    const p = await box(sel);
    assert.ok(p && p.w, "no box for " + sel);
    await touch(p.x, p.y, holdMs, jitter);
  };
  const swipe = async (dy) => {
    const p = await box("#log");
    await send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: p.x, y: p.y }] });
    for (let k = 1; k <= 8; k++) { await send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: p.x, y: p.y + (dy * k) / 8 }] }); await c.frames(1); }
    await send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  };
  // watch records, every frame, the pill, the title and what the log shows.
  await ev(`window.__watch = () => { window.__stopWatch = false; const f = []; window.__frames = f; const t0 = performance.now();
    const log = document.getElementById("log"), pill = document.getElementById("tobottom");
    const step = () => { f.push({ t: performance.now() - t0, hash: location.hash, pill: !pill.hidden, title: document.getElementById("title").textContent,
      loading: !!log.querySelector(".loading"), rows: log.querySelectorAll(".msg").length });
      if (!window.__stopWatch) requestAnimationFrame(step); };
    requestAnimationFrame(step); }; 1`);
  const pillShown = () => ev(`!document.getElementById("tobottom").hidden`);
  const item = (id) => `#sessions a.go[href="#/s/pc/${id}"]`;

  await ev(`location.hash = "#/s/pc/s9"; 1`);
  await c.until(`document.getElementById("title").textContent === "Long" && !document.querySelector("#log .loading") && document.querySelectorAll("#log .msg").length > 0`, "the first chat to be drawn");
  await c.until(`window.HubLogSettled()`, "the log to settle");
  assert.strictEqual(await pillShown(), false, "pill after the first open");

  // Taps on the log: a tap, a tap whose finger moves 2 px, a long press, the message box.
  await ev(`(() => { const l = document.getElementById("log"); l.scrollTop = l.scrollHeight; })()`);
  await c.until(`(() => { const l = document.getElementById("log"); return l.scrollHeight - l.scrollTop - l.clientHeight < 2; })()`, "the log at its end");
  for (const [name, held, jitter] of [["tap", 60, 0], ["tap with jitter", 60, 10], ["long press", 800, 6]]) {
    const p = await ev(`(() => { const ms = document.querySelectorAll("#log .msg"); const n = ms[ms.length - 1]; const r = n.getBoundingClientRect(); return { x: r.x + r.width / 2, y: Math.min(r.y + r.height / 2, innerHeight - 200) }; })()`);
    await touch(p.x, p.y, held, jitter);
    await c.frames(4);
    assert.strictEqual(await pillShown(), false, name + " on a message showed the pill");
  }
  // A tap on the message box raises the keyboard (a shorter viewport) and
  // closing it gives the height back.
  await tap("#input");
  for (const h of [850, 760, 650, 575]) { await send("Emulation.setDeviceMetricsOverride", { width: 412, height: h, deviceScaleFactor: 2.625, mobile: true }); await c.frames(1); }
  await c.until(`innerHeight === 575`, "the short viewport");
  await c.until(`window.HubLogSettled()`, "the log to settle with the keyboard up");
  assert.strictEqual(await pillShown(), false, "a tap on the message box showed the pill");
  await ev(`document.getElementById("input").blur(); 1`);
  await tap("#log");
  for (const h of [650, 760, 850, 915]) { await send("Emulation.setDeviceMetricsOverride", { width: 412, height: h, deviceScaleFactor: 2.625, mobile: true }); await c.frames(1); }
  await c.until(`innerHeight === 915`, "the full viewport");
  await c.until(`window.HubLogSettled()`, "the log to settle with the keyboard down");
  assert.strictEqual(await pillShown(), false, "closing the keyboard showed the pill");

  // Switch right after a swipe, as a reader does.
  for (const [id, title] of [["bigchat", "Big chat"], ["s8", "Second"], ["s9", "Long"], ["bigchat", "Big chat"]]) {
    await swipe(150);
    await swipe(-2500);
    await tap("#menu");
    await c.until(`document.getElementById("side").classList.contains("open") && document.getElementById("side").getAnimations().length === 0`, "the sidebar to open");
    await ev(`window.__watch(); 1`);
    await tap(item(id));
    // Frames are recorded until the chosen chat is drawn and the log has settled.
    await c.until(`location.hash === "#/s/pc/${id}" && document.getElementById("title").textContent === ${JSON.stringify(title)} && !document.querySelector("#log .loading") && document.querySelectorAll("#log .msg").length > 0 && window.HubLogSettled()`, `switch to ${id}: the chat to be drawn`);
    await c.until(`window.__frames.filter((x) => x.hash === "#/s/pc/${id}").length > 12`, `switch to ${id}: frames of the chosen chat to record`);
    await c.frames(4);
    await ev(`window.__stopWatch = true; 1`);
    const f = await ev(`window.__frames`);
    assert.ok(f.length > 10, "frames recorded");
    const pill = f.filter((x) => x.pill);
    assert.strictEqual(pill.length, 0, `switch to ${id}: pill in ${pill.length} frames, first at ${pill[0] && Math.round(pill[0].t)} ms`);
    const mine = f.filter((x) => x.hash === "#/s/pc/" + id);
    assert.ok(mine.length > 10, "frames of the chosen chat");
    // (The frame between the hash change and the route still shows the
    // chat it leaves.)
    const bad = mine.filter((x) => x.title === "New chat" || x.title === "Loading\u2026");
    assert.strictEqual(mine[mine.length - 1].title, title, "title after the switch");
    assert.strictEqual(bad.length, 0, `switch to ${id}: title ${JSON.stringify(bad[0] && bad[0].title)} at ${bad[0] && Math.round(bad[0].t)} ms`);
    const empty = mine.filter((x) => !x.loading && x.rows === 0);
    assert.strictEqual(empty.length, 0, `switch to ${id}: an empty log at ${empty[0] && Math.round(empty[0].t)} ms`);
    assert.strictEqual(await ev(`(() => { const l = document.getElementById("log"); return Math.round(l.scrollHeight - l.scrollTop - l.clientHeight) < 2; })()`), true, `switch to ${id}: not at the end`);
  }
  // The big chat drew all its history in the end, from the split snapshot.
  await c.until(`document.querySelectorAll("#log .msg.user").length >= 900`, "the big chat's older rows");
  const users = await ev(`document.querySelectorAll("#log .msg.user").length`);
  assert.strictEqual(users, 900, "big chat rows after the older part");
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
