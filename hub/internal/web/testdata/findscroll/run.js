"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR (env SHOT_PREFIX writes screenshots)
// Asserts that a scroll the reader did not make with a wheel, touch, pointer
// or key (find in page, focus on a link, a jump to an anchor: here
// scrollIntoView) stays where it went while the log grows at its end, and
// that a log at its end still follows what is added. With a prefix it
// writes PREFIX-412-light.png and the three others.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir] = process.argv.slice(2);
const shot = process.env.SHOT_PREFIX;

const { proc, connect } = require("../cdp.js").launch(bin, dir);

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the chat list");
  await ev(`location.hash = "#/s/pc/s9"; 1`);
  await c.until(`document.querySelectorAll("#log .msg").length >= 40`, "the chat to be drawn");
  for (const [name, w, h, mobile] of [["412", 412, 915, true], ["desktop", 1280, 800, false]]) {
    for (const scheme of ["light", "dark"]) {
      await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: mobile ? 2.625 : 1, mobile });
      await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
      // The page has the new size and scheme, and the log has taken the size
      // in: a scroll before that would count as the layout's, not the reader's.
      const settled = `innerWidth === ${w} && matchMedia("(prefers-color-scheme: dark)").matches === ${scheme === "dark"} && window.HubLogSettled()`;
      await c.until(settled, name + " " + scheme + ": the layout to settle");
      const at = () => ev(`(() => { const l = document.getElementById("log"); return { top: Math.round(l.scrollTop), gap: Math.round(l.scrollHeight - l.scrollTop - l.clientHeight) }; })()`);
      const grow = (text) => ev(`(() => { const d = document.createElement("div"); d.className = "msg assistant"; d.textContent = ${JSON.stringify(text)}; document.getElementById("log").append(d); })()`);
      await ev(`(() => { const l = document.getElementById("log"); l.scrollTop = l.scrollHeight; })()`);
      await c.until(`document.getElementById("log").scrollHeight - document.getElementById("log").scrollTop - document.getElementById("log").clientHeight < 2`, name + " " + scheme + ": the log at its end");
      // The page has taken the reader's scroll to the end in: it follows again (no pill).
      await c.until(`document.getElementById("tobottom").hidden`, name + " " + scheme + ": the page to follow the end again");
      // A log at its end follows what is added.
      await grow("tail one");
      await c.until(`document.getElementById("log").scrollHeight - document.getElementById("log").scrollTop - document.getElementById("log").clientHeight < 2`, name + " " + scheme + ": the log to follow the end");
      // Something that is not the reader's scroll moves the log up, as find in page does.
      await c.until(`window.HubLogSettled()`, name + " " + scheme + ": the log to take the growth in");
      await ev(`document.querySelectorAll("#log .msg")[3].scrollIntoView({ block: "start" }); 1`);
      // The page has taken the scroll in: the jump pill shows.
      await c.until(`!document.getElementById("tobottom").hidden`, name + " " + scheme + ": the page to see the scroll");
      const found = await at();
      assert.ok(found.gap > 500, name + " " + scheme + ": moved up, gap " + found.gap);
      for (let i = 0; i < 6; i++) { await grow("streamed " + i); await c.frames(2); }
      const kept = await at();
      assert.ok(Math.abs(kept.top - found.top) < 3, `${name} ${scheme}: the log went from ${found.top} to ${kept.top}`);
      if (shot) {
        const r = await send("Page.captureScreenshot", {});
        fs.writeFileSync(`${shot}-${name}-${scheme}.png`, Buffer.from(r.data, "base64"));
      }
      await ev(`document.querySelectorAll("#log .msg.assistant").forEach((n) => { if (/^(tail one|streamed)/.test(n.textContent)) n.remove(); }); 1`);
      await c.frames(2);
    }
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
