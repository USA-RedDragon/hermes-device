"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR LIVE_URL (env SHOT_PREFIX writes screenshots)
// Asserts that a turn's thinking blocks and tool cards sit in the order
// they happened, for a turn that streams live and for one drawn from
// stored rows, and that the cards are labeled in plain words.
const assert = require("assert");
const http = require("http");
const [bin, url, dir, liveURL] = process.argv.slice(2);
const shot = process.env.SHOT_PREFIX;

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const poke = () => new Promise((res, rej) => http.get(liveURL, (r) => { r.resume(); r.on("end", res); }).on("error", rej));

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 412, height: 915, deviceScaleFactor: 2, mobile: true });
  await send("Page.navigate", { url: url + "/auth/login?next=/" });
  await sleep(1500);
  // R thinking, G tool group, T loose card, A reply.
  const order = () => ev(`(() => Array.from(document.getElementById("log").children).map((n) =>
    n.matches("details.reasoning") ? "R" : n.matches("details.toolgroup") ? "G" : n.classList.contains("tool") ? "T" :
    n.matches(".msg.assistant") ? "A" : "").join(""))()`);
  const bodies = () => ev(`Array.from(document.querySelectorAll("#log details.reasoning .body")).map((n) => n.textContent)`);
  const open = async (id) => {
    await ev(`location.hash = "#/s/pc/${id}"; 1`);
  };

  await open("i1");
  await sleep(1000);
  await poke();
  for (let i = 0; i < 50 && !(await ev(`!!document.querySelector("#log .msg.assistant")`)); i++) await sleep(100);
  await sleep(300);
  assert.strictEqual(await order(), "RGRGRA", "live order");
  assert.deepStrictEqual(await bodies(), ["First thought.", "Second thought.", "Third thought."], "live thinking text");
  if (shot) await send("Page.captureScreenshot", {}).then((r) => require("fs").writeFileSync(shot + "-live-412.png", Buffer.from(r.data, "base64")));

  await open("i2");
  for (let i = 0; i < 50 && !(await ev(`!!document.querySelector("#log .msg.assistant")`)); i++) await sleep(100);
  await sleep(300);
  assert.strictEqual(await order(), "RGRGRGA", "stored order");
  assert.deepStrictEqual(await bodies(), ["First thought.", "Second thought.", "Third thought."], "stored thinking text");
  const titles = await ev(`Array.from(document.querySelectorAll("#log .tool")).map((n) => n.textContent)`);
  assert.ok(titles.some((t) => t.startsWith("Listed your chats")), "peers label: " + JSON.stringify(titles));
  assert.ok(titles.some((t) => t.startsWith("Named this chat rezero-theories")), "name label: " + JSON.stringify(titles));
  assert.ok(!titles.some((t) => /agent_peers|agent_set_name/.test(t)), "no raw names: " + JSON.stringify(titles));
  if (shot) {
    for (const [w, h, name] of [[412, 915, "412"], [1280, 800, "desktop"]]) {
      await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 2, mobile: w < 600 });
      for (const scheme of ["light", "dark"]) {
        await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: scheme }] });
        await sleep(300);
        await ev(`document.querySelectorAll("#log details.toolgroup").forEach((d) => { d.open = true; }); 1`);
        const r = await send("Page.captureScreenshot", {});
        require("fs").writeFileSync(shot + "-" + name + "-" + scheme + ".png", Buffer.from(r.data, "base64"));
      }
    }
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
