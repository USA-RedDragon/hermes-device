"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR (env SHOT_PREFIX writes screenshots)
// Asserts that a message the user typed shows without the notes Hermes
// added around it for the model, as a bubble and in the notice card of the
// model-switch note, at 412 px and on a desktop, in light and dark. With a
// prefix it writes PREFIX-412-light.png and the three others.
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
  await c.until(`document.querySelectorAll("#log .msg.user").length >= 2 && document.querySelectorAll("#log .notice").length >= 1`, "the chat to be drawn");
  for (const [name, w, h, mobile] of [["412", 412, 915, true], ["desktop", 1280, 800, false]]) {
    for (const scheme of ["light", "dark"]) {
      await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: mobile ? 2.625 : 1, mobile });
      await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
      await c.until(`innerWidth === ${w} && matchMedia("(prefers-color-scheme: dark)").matches === ${scheme === "dark"}`, "the size and scheme to apply");
      const texts = await ev(`Array.from(document.querySelectorAll("#log .msg.user")).map((n) => n.innerText.trim())`);
      assert.deepStrictEqual(texts, ["is this a bug?", "ping"], name + " " + scheme + ": user bubbles");
      const cards = await ev(`document.querySelectorAll("#log .notice").length`);
      assert.ok(cards >= 1, "the model note is a card");
      assert.strictEqual(await ev(`document.getElementById("log").textContent.includes("Adjust your self-identification")`), true, "the card keeps the note");
      assert.strictEqual(await ev(`Array.from(document.querySelectorAll("#log .msg.user")).some((n) => /\\[Note:|\\[Waiting:/.test(n.innerText))`), false, "no note in a bubble");
      if (shot) {
        const r = await send("Page.captureScreenshot", {});
        fs.writeFileSync(`${shot}-${name}-${scheme}.png`, Buffer.from(r.data, "base64"));
      }
    }
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
