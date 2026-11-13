"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR GATEWAY_BREAK_URL (env SHOT_PREFIX writes screenshots)
// Asserts that what the reader opened in a chat (a tool group, a tool card
// in it, a thinking block) is still open after a reconnect that draws the
// whole chat again from a snapshot, and what the reader left closed stays
// closed.
const assert = require("assert");
const http = require("http");
const [bin, url, dir, breakURL] = process.argv.slice(2);
const shot = process.env.SHOT_PREFIX;

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const poke = () => new Promise((res, rej) => http.get(breakURL, (r) => { r.resume(); r.on("end", res); }).on("error", rej));

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 412, height: 915, deviceScaleFactor: 2.625, mobile: true });
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the chat list");
  await ev(`location.hash = "#/s/pc/s9"; 1`);
  await c.until(`document.querySelectorAll("#log details.toolgroup").length === 2 && document.querySelectorAll("#log details.reasoning").length >= 1`, "the chat rows to be drawn");
  const state = () => ev(`(() => {
    const log = document.getElementById("log");
    return {
      groups: Array.from(log.querySelectorAll("details.toolgroup")).map((d) => d.open),
      cards: Array.from(log.querySelectorAll(".tool [aria-expanded]")).map((h) => h.getAttribute("aria-expanded")),
      thinking: Array.from(log.querySelectorAll("details.reasoning")).map((d) => d.open),
    };
  })()`);
  const before = await state();
  assert.ok(before.groups.length === 2 && before.thinking.length >= 1, "rows drawn: " + JSON.stringify(before));
  // The reader opens the first group, its first card, and the thinking block.
  // done is a page expression for what the click changes.
  const click = async (sel, done) => {
    const p = await ev(`(() => { const n = document.querySelector(${JSON.stringify(sel)}); if (!n) throw new Error("no " + ${JSON.stringify(sel)}); n.scrollIntoView({ block: "center" }); const r = n.getBoundingClientRect(); return { x: r.x + Math.min(30, r.width / 2), y: r.y + r.height / 2 }; })()`);
    await send("Input.dispatchMouseEvent", { type: "mousePressed", x: p.x, y: p.y, button: "left", clickCount: 1 });
    await send("Input.dispatchMouseEvent", { type: "mouseReleased", x: p.x, y: p.y, button: "left", clickCount: 1 });
    await c.until(done, "the click on " + sel + " to take effect");
  };
  await click("#log details.toolgroup > summary", `document.querySelector("#log details.toolgroup").open`);
  await ev(`window.__mark = document.querySelector("#log details.toolgroup"); 1`);
  await click("#log details.toolgroup[open] .tool [aria-expanded]", `document.querySelector("#log details.toolgroup[open] .tool [aria-expanded]").getAttribute("aria-expanded") === "true"`);
  await click("#log details.reasoning > summary", `document.querySelector("#log details.reasoning").open`);
  const opened = await state();
  assert.deepStrictEqual(opened.groups, [true, false], "opened group");
  assert.strictEqual(opened.cards[0], "true", "opened card");
  assert.strictEqual(opened.thinking[0], true, "opened thinking");
  if (shot) await send("Page.captureScreenshot", {}).then(async (r) => require("fs").writeFileSync(shot + "-412.png", Buffer.from(r.data, "base64")));
  // A reconnect that draws the chat again.
  await poke();
  await c.until(`!!window.__mark && !document.contains(window.__mark)`, "the snapshot to redraw the chat");
  // The new rows are in the log.
  await c.until(`document.querySelectorAll("#log details.toolgroup").length === 2 && document.querySelectorAll("#log details.reasoning").length >= 1`, "the redrawn rows");
  const after = await state();
  assert.deepStrictEqual(after, opened, "what the reader opened after the snapshot");
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
