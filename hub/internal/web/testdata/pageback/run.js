"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Asserts that a touch screen still answers taps after a page opened from a
// card is left by the Back button (history.back), as well as by its own Back
// button: the page view is gone, taps reach the message box, the chat list
// and the page's own Open button, and no overlay or inert state is left.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// Polls until expr is truthy in the page, so a slow machine waits for the
// render instead of racing a fixed sleep.
const until = async (ev, expr, what) => {
  for (let i = 0; i < 400; i++) {
    if (await ev(expr)) return;
    await sleep(50);
  }
  throw new Error("timed out waiting for " + what);
};




async function device(c, w, h) {
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: true });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 5 });
}

// tap sends a real touch at the center of the element the selector finds.
async function tap(c, sel) {
  const r = await c.ev(`(() => { const e = document.querySelector(${JSON.stringify(sel)}); if (!e) return null; const b = e.getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);
  assert.ok(r, "no element for " + sel);
  await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: r.x, y: r.y }] });
  await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await sleep(250);
  return r;
}

async function swipe(c, x0, y0, x1, y1) {
  await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: x0, y: y0 }] });
  for (let i = 1; i <= 8; i++) {
    await c.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x0 + (x1 - x0) * i / 8, y: y0 + (y1 - y0) * i / 8 }] });
    await sleep(16);
  }
  await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await sleep(300);
}
async function tapAt(c, x, y) {
  await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
  await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await sleep(250);
}

// state reports what could swallow a tap.
const state = (ev, sel) => ev(`(() => {
  const e = document.querySelector(${JSON.stringify(sel)});
  const b = e.getBoundingClientRect();
  const top = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
  const pv = document.querySelector(".pageview");
  return { hit: !!top && (top === e || e.contains(top) || top.contains(e)), top: top ? top.tagName + "." + top.className : "",
    pageview: pv ? { hidden: pv.hidden, display: getComputedStyle(pv).display } : null,
    inert: [...document.querySelectorAll("[inert]")].length, bodyPE: getComputedStyle(document.body).pointerEvents,
    bodyOverflow: getComputedStyle(document.body).overflow, hash: location.hash };
})()`);

async function check(c, how, leave) {
  const { ev } = c;
  await ev(`window.__clicks = []; document.addEventListener("click", (e) => window.__clicks.push(e.target.id || e.target.className || e.target.tagName), true)`);
  await tap(c, ".page-card button");
  await sleep(800);
  assert.ok(/^#\/page\//.test(await ev(`location.hash`)), how + ": the page opened");
  assert.ok(await ev(`!!document.querySelector(".pageview:not([hidden])")`), how + ": the page view is shown");
  // The reader scrolls the page with a finger before leaving it.
  await swipe(c, 206, 700, 206, 300);
  await swipe(c, 206, 700, 206, 300);
  // A fling still coasting when Back is pressed.
  await c.send("Input.synthesizeScrollGesture", { x: 206, y: 700, yDistance: -600, speed: 4000, gestureSourceType: "touch" });
  await tapAt(c, 206, 500);
  await leave();
  await sleep(100);
  for (const sel of ["#input", ".page-card button"]) {
    const s = await state(ev, sel);
    assert.ok(s.hit, `${how}: ${sel} is under its own center after leaving the page: ${JSON.stringify(s)}`);
    assert.strictEqual(s.pageview, null, `${how}: the page view is still in the document: ${JSON.stringify(s)}`);
    assert.strictEqual(await ev(`document.querySelectorAll("iframe").length`), 0, how + ": a frame is left in the document");
    const frames = (await c.send("Target.getTargets")).targetInfos.filter((x) => x.type === "iframe");
    assert.strictEqual(frames.length, 0, how + ": the page's frame process is still alive: " + JSON.stringify(frames.map((x) => x.url)));
    assert.strictEqual(s.inert, 0, how + ": inert left behind");
    assert.strictEqual(s.bodyPE, "auto", how + ": body pointer-events");
  }
  if (process.env.PAGEBACK_SHOT && how === "Back button") {
    fs.writeFileSync(process.env.PAGEBACK_SHOT, Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
  }
  await ev(`window.__clicks.length = 0`);
  await tap(c, "#input");
  assert.strictEqual(await ev(`document.activeElement && document.activeElement.id`), "input", how + ": a tap on the message box focuses it");
  await ev(`document.activeElement.blur()`);
  await tap(c, ".page-card button");
  await sleep(600);
  assert.ok(/^#\/page\//.test(await ev(`location.hash`)), how + ": a tap on Open opens the page again");
  await leave();
  await sleep(600);
  // The sidebar: a tap on a chat in the list switches chats.
  await ev(`document.getElementById("menu") && document.getElementById("menu").click()`);
  await sleep(400);
  const row = await ev(`(() => { const e = [...document.querySelectorAll("#sessions li, #sessions a, #sessions button")].find((x) => x.textContent.includes("Second") && x.getBoundingClientRect().width > 0); if (!e) return null; e.setAttribute("data-t", "1"); return true; })()`);
  assert.ok(row, how + ": the Second chat is listed");
  await tap(c, "[data-t]");
  await sleep(800);
  assert.ok(/Second/.test(await ev(`document.getElementById("title").textContent`)), how + ": a tap on a chat in the list opens it");
  await ev(`location.hash = ""`);
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  const { ev } = c;
  await device(c, 412, 915);
  const open = async () => {
    await c.send("Page.navigate", { url: url + "/auth/login?next=/" });
    await until(ev, `[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Pages"))`, "the Pages session is listed");
    await ev(`(() => { const e = [...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Pages")).pop(); e.click(); })()`);
    await sleep(1200);
  };
  await open();
  await check(c, "Back button", () => ev(`history.back()`));
  await open();
  await check(c, "Close button", () => tap(c, ".pageview-bar button"));
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
