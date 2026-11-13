"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR [SHOT_PREFIX]
// Asserts that a scroll the reader did not make with a wheel, touch, pointer
// or key (find in page, focus on a link, a jump to an anchor: here
// scrollIntoView) stays where it went while the log grows at its end, and
// that a log at its end still follows what is added. With a prefix it
// writes PREFIX-412-light.png and the three others.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, shot] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Page.navigate", { url: url + "/auth/login?next=/" });
  await sleep(1500);
  await ev(`location.hash = "#/s/pc/s9"; 1`);
  for (let i = 0; i < 50 && (await ev(`document.querySelectorAll("#log .msg").length`)) < 40; i++) await sleep(100);
  for (const [name, w, h, mobile] of [["412", 412, 915, true], ["desktop", 1280, 800, false]]) {
    for (const scheme of ["light", "dark"]) {
      await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: mobile ? 2.625 : 1, mobile });
      await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
      await sleep(1000);
      const at = () => ev(`(() => { const l = document.getElementById("log"); return { top: Math.round(l.scrollTop), gap: Math.round(l.scrollHeight - l.scrollTop - l.clientHeight) }; })()`);
      const grow = (text) => ev(`(() => { const d = document.createElement("div"); d.className = "msg assistant"; d.textContent = ${JSON.stringify(text)}; document.getElementById("log").append(d); })()`);
      await ev(`(() => { const l = document.getElementById("log"); l.scrollTop = l.scrollHeight; })()`);
      await sleep(300);
      // A log at its end follows what is added.
      await grow("tail one");
      await sleep(300);
      assert.ok((await at()).gap < 2, name + " " + scheme + ": follows the end");
      // Something that is not the reader's scroll moves the log up, as find in page does.
      await sleep(1500);
      await ev(`document.querySelectorAll("#log .msg")[3].scrollIntoView({ block: "start" }); 1`);
      await sleep(300);
      const found = await at();
      assert.ok(found.gap > 500, name + " " + scheme + ": moved up, gap " + found.gap);
      for (let i = 0; i < 6; i++) { await grow("streamed " + i); await sleep(120); }
      await sleep(400);
      const kept = await at();
      assert.ok(Math.abs(kept.top - found.top) < 3, `${name} ${scheme}: the log went from ${found.top} to ${kept.top}`);
      if (shot) {
        const r = await send("Page.captureScreenshot", {});
        fs.writeFileSync(`${shot}-${name}-${scheme}.png`, Buffer.from(r.data, "base64"));
      }
      await ev(`document.querySelectorAll("#log .msg.assistant").forEach((n) => { if (/^(tail one|streamed)/.test(n.textContent)) n.remove(); }); 1`);
    }
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
