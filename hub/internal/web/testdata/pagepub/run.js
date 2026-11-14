"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Asserts that a page_publish call of a chat shows the page card (its title
// and an Open button) in place of a generic tool card with a raw path, that
// a reply naming the same file shows no second card, and that Open shows
// the page full screen. With PAGEPUB_SHOT it saves screenshots at 1280 and
// 412 px wide, light and dark, as PAGEPUB_SHOT + "<width>-<scheme>.png".
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");

async function shot(c, name) {
  await until(c.ev, `document.getAnimations().length === 0`, "transitions to end");
  if (!process.env.PAGEPUB_SHOT) return;
  await c.frames(2);
  fs.writeFileSync(process.env.PAGEPUB_SHOT + name + ".png", Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  const { ev } = c;
  await c.send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false });
  await c.send("Page.navigate", { url: url + "/auth/login?next=/" });
  await until(ev, `[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Published"))`, "the chat is listed");
  await ev(`[...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Published")).pop().click()`);
  await until(ev, `!!document.querySelector(".tool-page .page-card")`, "the page card of the page_publish call");

  const cards = await ev(`(() => ({
    n: document.querySelectorAll(".page-card").length,
    name: document.querySelector(".tool-page .page-card .name").textContent,
    open: !!document.querySelector(".tool-page .page-card button.primary"),
    generic: [...document.querySelectorAll(".tool.notice")].map((x) => x.textContent),
    raw: document.body.innerText.includes("page-0123456789abcdef"),
    grouped: !!document.querySelector(".toolgroup .tool-page"),
  }))()`);
  assert.strictEqual(cards.n, 1, "the page shows once, though the reply names it too: " + JSON.stringify(cards));
  assert.strictEqual(cards.name, "Tracker", "the card is titled by the page");
  assert.ok(cards.open, "the card has an Open button");
  assert.strictEqual(cards.generic.filter((t) => /page/i.test(t)).length, 0, "no generic card for the call: " + JSON.stringify(cards.generic));
  assert.ok(!cards.raw, "no raw path on the screen");
  assert.ok(!cards.grouped, "the card is not folded into a group");

  for (const [w, h] of [[1280, 900], [412, 915]]) {
    await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: w < 500 });
    await ev(`document.getElementById("app").classList.contains("act-closed") === false && document.getElementById("actclose").click()`);
    for (const scheme of ["light", "dark"]) {
      await c.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
      await shot(c, w + "-" + scheme);
    }
  }
  await c.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "light" }] });

  await ev(`document.querySelector(".tool-page .page-card button.primary").click()`);
  await until(ev, `/^#\\/page\\//.test(location.hash) && !!document.querySelector(".pageview:not([hidden]) iframe")`, "the page view to open");
  const view = await ev(`(() => { const p = document.querySelector(".pageview"); return { title: p.querySelector(".name").textContent,
    src: p.querySelector("iframe").getAttribute("src"), sandbox: p.querySelector("iframe").getAttribute("sandbox"), full: p.getBoundingClientRect().width === innerWidth }; })()`);
  assert.strictEqual(view.title, "Tracker");
  assert.ok(view.src.includes("/page?path=") && view.src.includes(encodeURIComponent("/opt/data/hermes/cache/pages/hermes-device/pp1/page-0123456789abcdef.html")), view.src);
  assert.ok(!/allow-same-origin/.test(view.sandbox), "the frame is sandboxed: " + view.sandbox);
  assert.ok(view.full, "the page view fills the screen");
  await shot(c, "view-412-light");
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
