"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR SEEN_PATH GONE_PATH
// Asserts that a vision_analyze card shows a thumbnail of the image it looked
// at beside the question, that a tap opens the image full screen, that the
// open card shows the image (at most 14rem tall) with its path and without
// the generic "Image loaded" output, that a file the hub cannot serve and a
// web link show no image at all, and that none of it widens the page.
// Touch phone and desktop, light and dark. VISIONCARD_SHOT=prefix also writes
// screenshots.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, seen, gone] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");

async function shot(c, name) {
  if (!process.env.VISIONCARD_SHOT) return;
  fs.writeFileSync(process.env.VISIONCARD_SHOT + name + ".png", Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
}

async function press(c, touch, sel, done) {
  for (let i = 0; i < 8; i++) {
    await tap1(c, touch, sel);
    if (!done || (await c.ev(done))) return;
  }
}

async function tap1(c, touch, sel) {
  await new Promise((r) => setTimeout(r, 250));
  // Scroll the target to the middle and read where it is in one step, so the
  // chat's own scrolling cannot move it between the look and the tap.
  const r = await c.poll(async () => {
    const p = await c.ev(`(() => { const e = document.querySelector(${JSON.stringify(sel)}); e.scrollIntoView({ block: "center" }); const b = e.getBoundingClientRect(); const x = b.left + b.width / 2, y = b.top + b.height / 2; const t = document.elementFromPoint(x, y); return t && e.contains(t) ? { x, y } : null; })()`);
    return p;
  }, sel + " to be under its own center");
  if (touch) {
    await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: r.x, y: r.y }] });
    await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  } else {
    for (const type of ["mousePressed", "mouseReleased"]) await c.send("Input.dispatchMouseEvent", { type, x: r.x, y: r.y, button: "left", clickCount: 1 });
  }
  await c.frames(2);
}

// card is the n-th vision card (0 based) in the chat.
const card = (n) => `document.querySelectorAll('.tool[data-tool="vision_analyze"]')[${n}]`;

async function run(c, w, h, touch, dark) {
  const { ev } = c;
  const tag = (w < 600 ? "phone" : "desktop") + "-" + (dark ? "dark" : "light");
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: touch });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: touch, maxTouchPoints: 5 });
  await c.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await c.load(url + "/auth/login?next=/", `[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Vision"))`, "the Vision session is listed");
  await ev(`[...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Vision")).pop().click()`);
  await until(ev, `document.querySelectorAll('.tool[data-tool="vision_analyze"]').length === 4`, "four vision cards");
  // Runs of finished cards fold into a group: open them all.
  await ev(`document.querySelectorAll("details.toolgroup").forEach((d) => { d.open = true; })`);
  await until(ev, `!!${card(0)}.querySelector(".vis-thumb img") && ${card(0)}.querySelector(".vis-thumb img").complete`, tag + ": the thumbnail to load");
  await until(ev, `!${card(1)}.querySelector("img") && !!${card(1)}.querySelector(".vis-gone")`, tag + ": the missing image to turn into a placeholder");

  // Collapsed: the thumbnail renders, sits beside a question clamped to two lines.
  await ev(`${card(0)}.scrollIntoView()`);
  assert.ok(await ev(`${card(0)}.querySelector(".vis-thumb img").naturalWidth === 640`), tag + ": the thumbnail is the real image");
  const th = await ev(`(() => { const b = ${card(0)}.querySelector(".vis-thumb").getBoundingClientRect(); const f = parseFloat(getComputedStyle(document.documentElement).fontSize); return { w: b.width / f, h: b.height / f }; })()`);
  assert.ok(Math.abs(th.w - 5.2) < 0.3 && Math.abs(th.h - 3.6) < 0.3, tag + ": thumbnail size " + JSON.stringify(th));
  assert.strictEqual(await ev(`${card(0)}.querySelector(".notice-title").textContent`), "Looked at an image", tag + ": title");
  assert.ok(/^Done/.test(await ev(`${card(0)}.querySelector(".notice-meta").textContent`)), tag + ": status line " + await ev(`${card(0)}.querySelector(".notice-meta").textContent`));
  const q = await ev(`(() => { const s = ${card(0)}.querySelector(".vis-q .qt"); const lh = parseFloat(getComputedStyle(s).lineHeight); return { h: s.getBoundingClientRect().height, lh, text: s.textContent }; })()`);
  assert.ok(q.h <= q.lh * 2 + 2, tag + ": the question is clamped to 2 lines " + JSON.stringify(q));
  assert.ok(/Describe the window title/.test(q.text), tag + ": the question is the whole text, clamped by CSS");
  assert.ok(await ev(`${card(0)}.querySelector(".notice-body-wrap").hidden`), tag + ": the body is closed");
  await c.poll(async () => { await ev(`${card(0)}.scrollIntoView({ block: "center" })`); await c.frames(3); return ev(`(() => { const b = ${card(0)}.getBoundingClientRect(); return b.top > 60 && b.bottom < window.innerHeight; })()`); }, tag + ": the first card in view");
  await shot(c, tag + "-collapsed");

  // Tap on the thumbnail opens it full screen; Escape closes it.
  await press(c, touch, `.tool[data-tool="vision_analyze"] .vis-thumb`, `!!document.querySelector(".lightbox img")`);
  await until(ev, `!!document.querySelector(".lightbox img")`, tag + ": the thumbnail to open full screen");
  assert.ok((await ev(`document.querySelector(".lightbox img").src`)).includes("/media?path=" + encodeURIComponent(seen)), tag + ": full screen shows the served image");
  await until(ev, `document.querySelector(".lightbox img").complete && document.querySelector(".lightbox img").naturalWidth === 640`, tag + ": the full screen image to load");
  await shot(c, tag + "-fullscreen");
  await c.send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape" });
  await c.send("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape" });
  await until(ev, `document.querySelectorAll(".lightbox").length === 0`, tag + ": Escape to close the viewer");

  // Not available here, and a web link: no image, nothing broken.
  assert.ok(await ev(`!${card(1)}.querySelector("img")`), tag + ": no <img> for a missing file");
  assert.ok(/Image not available here/.test(await ev(`${card(1)}.querySelector(".vis").textContent`)), tag + ": says it is not available");
  assert.ok((await ev(`${card(1)}.querySelector(".vis-path").textContent`)).endsWith("/gone.png"), tag + ": the path is shown small");
  assert.ok(await ev(`getComputedStyle(${card(1)}.querySelector(".vis-gone")).borderTopStyle === "dashed"`), tag + ": dashed placeholder");
  assert.ok(await ev(`!${card(2)}.querySelector("img")`), tag + ": no <img> for a web link");
  assert.strictEqual(await ev(`${card(2)}.querySelector("a.vis-link").href`), "https://example.com/charts/q3.png", tag + ": the link");
  assert.ok(await ev(`!${card(3)}.querySelector("img")`), tag + ": no <img> for an image the hub does not serve");
  await ev(`${card(1)}.scrollIntoView()`);
  await shot(c, tag + "-unavailable");

  // Expanded: the image, its path, the question, the call, no generic output.
  await ev(`${card(0)}.querySelector(".notice-head").click()`);
  await until(ev, `!${card(0)}.querySelector(".notice-body-wrap").hidden && !!${card(0)}.querySelector(".vis-big img")`, tag + ": the open card to show the image");
  assert.ok(await ev(`${card(0)}.querySelector(".vis").hidden`), tag + ": the thumbnail row folds away when open");
  const big = await ev(`(() => { const i = ${card(0)}.querySelector(".vis-big img"); const f = parseFloat(getComputedStyle(document.documentElement).fontSize); return { h: i.getBoundingClientRect().height / f, w: i.getBoundingClientRect().width, ok: i.complete && i.naturalWidth === 640 }; })()`);
  assert.ok(big.ok && big.h <= 14.05, tag + ": the open image is at most 14rem tall " + JSON.stringify(big));
  assert.strictEqual(await ev(`${card(0)}.querySelector(".vis-path").textContent`), seen, tag + ": the path");
  const labels = await ev(`[...${card(0)}.querySelectorAll(".notice-body-wrap .tool-sec > b")].map((b) => b.textContent).join(",")`);
  assert.strictEqual(labels, "Question,Call", tag + ": blocks " + labels);
  assert.ok(!/Image loaded into your context/.test(await ev(`${card(0)}.innerText`)), tag + ": no generic output");
  assert.ok((await ev(`${card(0)}.querySelector(".notice-body-wrap pre.tool-box").textContent`)).includes('"image_url"'), tag + ": the Call JSON");
  await ev(`${card(0)}.scrollIntoView()`);
  await shot(c, tag + "-expanded");
  await press(c, touch, `.tool[data-tool="vision_analyze"] .vis-big`, `!!document.querySelector(".lightbox img")`);
  await until(ev, `!!document.querySelector(".lightbox img")`, tag + ": the open image to open full screen");
  await c.send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape" });
  await c.send("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape" });
  await until(ev, `document.querySelectorAll(".lightbox").length === 0`, tag + ": Escape to close the second time");
  // The placeholder, open.
  await ev(`${card(1)}.querySelector(".notice-head").click()`);
  await until(ev, `!${card(1)}.querySelector(".notice-body-wrap").hidden`, tag + ": the second card to open");
  assert.ok(/Image not available here/.test(await ev(`${card(1)}.querySelector(".vis-sec").textContent`)), tag + ": open placeholder");
  assert.ok(await ev(`!${card(1)}.querySelector("img")`), tag + ": open placeholder has no image");
  assert.ok(await ev(`document.documentElement.scrollWidth <= window.innerWidth + 1`), tag + ": the page does not scroll sideways");
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  await run(c, 412, 915, true, false);
  await run(c, 412, 915, true, true);
  await run(c, 1280, 800, false, false);
  await run(c, 1280, 800, false, true);
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
