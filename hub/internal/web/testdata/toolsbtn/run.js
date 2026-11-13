"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// At 360, 412 and 1280 px, light and dark: the Tools button is inside the
// message box (not in the top bar, no chip on the model line), opens and
// closes the panel by click and Escape (focus returns to the button), is
// reachable by Tab, never sits under typed text, and nothing overflows
// sideways. TOOLSBTN_SHOT=dir also writes toolsbtn-*.png screenshots.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir] = process.argv.slice(2);
const { connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const until = async (ev, expr, what) => {
  for (let i = 0; i < 400; i++) {
    if (await ev(expr)) return;
    await sleep(50);
  }
  throw new Error("timed out waiting for " + what);
};
const open = `!document.getElementById("qpanel").hidden`;
const LONG = "the quick brown fox jumps over the lazy dog and keeps running along the river bank until the sun goes down behind the hills ".repeat(4) + "Unbroken" + "X".repeat(80);

async function run(c, w, dark) {
  const { ev, send } = c;
  const tag = (w === 1280 ? "desktop" : String(w)) + (dark ? "-dark" : "-light");
  const mobile = w < 600;
  const shot = async (name) => {
    if (!process.env.TOOLSBTN_SHOT) return;
    await sleep(200);
    fs.writeFileSync(`${process.env.TOOLSBTN_SHOT}/toolsbtn-${tag}-${name}.png`, Buffer.from((await send("Page.captureScreenshot", { format: "png" })).data, "base64"));
  };
  await send("Emulation.setDeviceMetricsOverride", { width: w, height: mobile ? 780 : 800, deviceScaleFactor: 1, mobile });
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await ev(`window.__old = true`);
  await send("Page.navigate", { url: url + "/auth/login?next=/" });
  await until(ev, `!window.__old && document.getElementById("sessions") && document.getElementById("sessions").children.length > 0`, tag + ": chats listed");
  await ev(`location.hash = "#/s/pc/s1"`);
  await until(ev, `!document.getElementById("qpbtn").hidden`, tag + ": tools button");

  // One entry point, in the message box.
  assert.strictEqual(await ev(`document.querySelectorAll("#qpbtn").length`), 1, tag + ": one tools button");
  assert.ok(await ev(`!!document.querySelector(".composer .taw #qpbtn")`), tag + ": the button is inside the message box");
  assert.ok(await ev(`!document.querySelector(".bar #qpbtn, .bar [aria-label='Tools, skills and safety']")`), tag + ": no tools button in the top bar");
  assert.ok(await ev(`!document.querySelector(".info .qp, .qp, #info [title='Tools, skills and safety']")`), tag + ": no chip on the model line");
  assert.strictEqual(await ev(`document.getElementById("qpbtn").getAttribute("aria-label")`), "Tools, skills and safety", tag + ": label");
  assert.strictEqual(await ev(`document.querySelectorAll("[aria-label='Tools, skills and safety']").length`), 1, tag + ": one control with the label");

  // Opens and closes by click; the accent shows while open.
  await ev(`document.getElementById("qpbtn").click()`);
  await until(ev, open, tag + ": panel opens");
  assert.strictEqual(await ev(`document.getElementById("qpbtn").getAttribute("aria-expanded")`), "true", tag + ": expanded");
  assert.ok(await ev(`document.getElementById("qpbtn").classList.contains("on")`), tag + ": accent class while open");
  await until(ev, `/Web search/.test(document.getElementById("qpanel").innerText)`, tag + ": panel lists tools");
  const g = await ev(`(() => { const p = document.getElementById("qpanel").getBoundingClientRect(), k = document.getElementById("composer").getBoundingClientRect();
    return { pb: p.bottom, pt: p.top, pl: p.left, pr: p.right, ct: k.top, ch: innerHeight, cw: innerWidth }; })()`);
  assert.ok(g.pl >= 0 && g.pr <= g.cw && g.pt >= 0, tag + ": panel inside the window " + JSON.stringify(g));
  if (mobile) assert.ok(Math.abs(g.pb - g.ch) < 2, tag + ": a bottom sheet " + JSON.stringify(g));
  else assert.ok(g.pb <= g.ct + 1, tag + ": the panel sits above the message box " + JSON.stringify(g));
  await ev(`document.getElementById("qpanel").scrollTop = 0`);
  await shot("panel");
  // Escape closes and returns focus to the button.
  await ev(`(document.querySelector("#qpanel input, #qpanel button") || document.getElementById("qpanel")).focus()`);
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
  await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
  await until(ev, `document.getElementById("qpanel").hidden`, tag + ": Escape closes");
  assert.strictEqual(await ev(`document.activeElement.id`), "qpbtn", tag + ": focus returns to the button");
  assert.strictEqual(await ev(`document.getElementById("qpbtn").getAttribute("aria-expanded")`), "false", tag + ": collapsed");
  // Click again toggles it shut.
  await ev(`document.getElementById("qpbtn").click()`);
  await until(ev, open, tag + ": reopens");
  await ev(`document.getElementById("qpbtn").click()`);
  await until(ev, `document.getElementById("qpanel").hidden`, tag + ": the button closes it");

  // Keyboard: Tab from the box reaches the button, then Send.
  await ev(`document.getElementById("input").focus()`);
  await send("Input.dispatchKeyEvent", { type: "keyDown", key: "Tab", code: "Tab", windowsVirtualKeyCode: 9 });
  await send("Input.dispatchKeyEvent", { type: "keyUp", key: "Tab", code: "Tab", windowsVirtualKeyCode: 9 });
  assert.strictEqual(await ev(`document.activeElement.id`), "qpbtn", tag + ": Tab reaches the button");

  // A long line grows the box and never runs under the button.
  await ev(`(() => { const i = document.getElementById("input"); i.value = ${JSON.stringify(LONG)}; i.dispatchEvent(new Event("input")); })()`);
  await sleep(300);
  const t = await ev(`(() => { const i = document.getElementById("input"), cs = getComputedStyle(i), b = document.getElementById("qpbtn").getBoundingClientRect(), r = i.getBoundingClientRect();
    const tw = [...i.parentNode.querySelectorAll("textarea")].find((x) => x !== i);
    return { h: r.height, pr: parseFloat(cs.paddingRight), room: r.right - b.left, bl: b.left, bt: b.top, bb: b.bottom, tt: r.top, tb: r.bottom, br: b.right, ir: r.right,
      twPad: tw ? parseFloat(getComputedStyle(tw).paddingRight) : -1, twW: tw ? tw.offsetWidth : -1, w: i.offsetWidth, sh: i.scrollHeight, ch: i.clientHeight }; })()`);
  assert.ok(t.h > 60, tag + ": the box grew " + JSON.stringify(t));
  assert.ok(t.pr >= t.room, tag + ": the text stops before the button " + JSON.stringify(t));
  assert.ok(t.bt >= t.tt && t.bb <= t.tb && t.br <= t.ir, tag + ": the button is inside the box " + JSON.stringify(t));
  assert.strictEqual(t.twPad, t.pr, tag + ": the hidden twin pads like the box " + JSON.stringify(t));
  assert.strictEqual(t.twW, t.w, tag + ": the twin is as wide as the box");
  assert.ok(t.sh <= t.ch + 2 || t.h >= 180 || t.h >= (await ev(`innerHeight * 0.4`)) - 2, tag + ": no hidden text below the box " + JSON.stringify(t));
  // The paperclip, box and Send all fit, with no sideways scroll.
  const f = await ev(`(() => { const rs = ["attach", "input", "send"].map((id) => document.getElementById(id).getBoundingClientRect());
    return { rs: rs.map((r) => [Math.round(r.left), Math.round(r.right)]), vw: innerWidth, sw: document.documentElement.scrollWidth, bw: document.body.scrollWidth, tw: document.getElementById("input").offsetWidth }; })()`);
  assert.ok(f.sw <= f.vw && f.bw <= f.vw, tag + ": horizontal overflow " + JSON.stringify(f));
  assert.ok(f.rs[0][0] >= 0 && f.rs[2][1] <= f.vw && f.rs[0][1] <= f.rs[1][0] && f.rs[1][1] <= f.rs[2][0], tag + ": paperclip, box, send in a row " + JSON.stringify(f));
  assert.ok(f.tw >= 150, tag + ": the box is wide enough " + JSON.stringify(f));
  await shot("composer");
  await ev(`(() => { const i = document.getElementById("input"); i.value = ""; i.dispatchEvent(new Event("input")); })()`);
}

(async () => {
  const c = await connect();
  try {
    await c.send("Page.enable");
    for (const w of [360, 412, 1280]) for (const dark of [false, true]) await run(c, w, dark);
  } finally {
    c.close();
  }
})().catch((e) => { console.error(e.stack || e); process.exit(1); });
