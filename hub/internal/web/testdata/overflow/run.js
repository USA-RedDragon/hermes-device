"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Renders every card type with long unbroken content at 360, 412 and 1280 px,
// light and dark, and asserts nothing widens the page or the chat stream.
// OVERFLOW_SHOT=prefix also writes screenshots.
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





async function run(c, w, touch, dark) {
  const { ev } = c;
  const tag = w + (dark ? "-dark" : "-light");
  const h = w < 600 ? 915 : 800;
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: touch });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: touch, maxTouchPoints: 5 });
  await c.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await c.send("Page.navigate", { url: url + "/auth/login?next=/" });
  await until(ev, `[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Work"))`, tag + ": the Work session is listed");
  await ev(`(() => { const e = [...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Work")).pop(); e.click(); })()`);
  await until(ev, `document.querySelectorAll("#log .approval").length >= 5 && document.querySelectorAll("#log .msg").length >= 2`, tag + ": the chat is drawn");
  // The card types the page must have drawn, so the test cannot pass on an empty stream.
  const kinds = await ev(`({ approvals: document.querySelectorAll("#log .approval").length, tools: document.querySelectorAll("#log .tool, #log details.toolgroup").length,
    notices: document.querySelectorAll("#log .notice").length, table: document.querySelectorAll("#log .md table").length,
    code: document.querySelectorAll("#log .md pre").length, msgs: document.querySelectorAll("#log .msg").length })`);
  assert.ok(kinds.approvals >= 5 && kinds.tools >= 1 && kinds.notices >= 1 && kinds.table >= 1 && kinds.code >= 1 && kinds.msgs >= 2, tag + ": every card type is on the page " + JSON.stringify(kinds));
  // Open every folded group and card, so their bodies count too.
  await ev(`document.querySelectorAll("#log details").forEach((d) => { d.open = true; })`);
  await sleep(300);
  const r = await ev(`(() => {
    const vw = document.documentElement.clientWidth;
    const se = document.scrollingElement, log = document.getElementById("log");
    const bad = [];
    // Inside a box that scrolls on its own, wide content is fine.
    const scrolls = (n) => { for (let p = n.parentElement; p && p !== log; p = p.parentElement) { if (/(auto|scroll|hidden|clip)/.test(getComputedStyle(p).overflowX)) return true; } return false; };
    for (const n of log.querySelectorAll("*")) {
      if (scrolls(n)) continue;
      const b = n.getBoundingClientRect();
      if (!b.width) continue;
      if (b.right > vw + 0.5 || b.left < -0.5) bad.push(n.tagName + "." + String(n.getAttribute("class")) + " " + Math.round(b.left) + ".." + Math.round(b.right));
    }
    return { vw, docScroll: se.scrollWidth, docClient: se.clientWidth, logScroll: log.scrollWidth, logClient: log.clientWidth, logLeft: log.scrollLeft, nbad: bad.length, bad: bad.slice(0, 12) };
  })()`);
  assert.ok(r.docScroll <= r.docClient, `${tag}: the page scrolls sideways ${JSON.stringify(r)}`);
  assert.ok(r.logScroll <= r.logClient, `${tag}: the chat stream is wider than the screen ${JSON.stringify(r)}`);
  assert.strictEqual(r.logLeft, 0, `${tag}: the chat stream is scrolled sideways`);
  assert.strictEqual(r.nbad, 0, `${tag}: elements outside the screen ${JSON.stringify(r)}`);
  // The script of a browser approval sits in a box of its own, apart from the sentence, with no internal jargon.
  const a = await ev(`(() => { const a = [...document.querySelectorAll("#log .approval")].find((x) => /read-only script/.test(x.textContent)); if (!a) return null;
    const pre = a.querySelector("pre"); return { pre: !!pre && pre.textContent.includes("out.length<30"),
      prose: [...a.children].filter((c) => c.tagName !== "PRE").map((c) => c.textContent).join(" "), jargon: /plugin approval rule/.test(a.textContent) }; })()`);
  assert.ok(a && a.pre, tag + ": the script is in its own code block " + JSON.stringify(a));
  assert.ok(!a.prose.includes("out.length"), tag + ": the script is not in the sentence");
  assert.ok(!a.jargon, tag + ": no internal jargon in the card");
  if (process.env.OVERFLOW_SHOT) {
    const shot = async (name) => fs.writeFileSync(process.env.OVERFLOW_SHOT + name + ".png", Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
    await ev(`[...document.querySelectorAll("#log .approval")].find((x) => /read-only script/.test(x.textContent)).scrollIntoView({ block: "center" })`);
    await sleep(300);
    await shot(tag + "-eval");
    await ev(`document.getElementById("log").scrollTop = 0`);
    await sleep(300);
    await shot(tag + "-top");
  }
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  for (const dark of [false, true]) {
    await run(c, 360, true, dark);
    await run(c, 412, true, dark);
    await run(c, 1280, false, dark);
  }
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
