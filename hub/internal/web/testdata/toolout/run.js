"use strict";
// Drives the real hub page in headless Chrome. usage: node run.js CHROME
// HUB_URL PROFILE_DIR TAIL
// A chat whose snapshot cut two long tool outputs: closed cards build no
// output block and ask for nothing, opening a card fetches its output once
// and shows it whole (TAIL is a line only the full text has), and a short
// output opens with no request.
const assert = require("assert");
const [bin, url, dir, tail] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");

(async () => {
  const c = await connect();
  const { ev } = c;
  await c.send("Page.enable");
  const count = () => ev(`performance.getEntriesByType("resource").filter((r) => r.name.includes("/tool-output")).map((r) => r.name)`);
  await c.load(url + "/auth/login?next=/", `[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Cut"))`, "the Cut session is listed");
  await ev(`[...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Cut")).pop().click()`);
  await until(ev, `document.querySelectorAll("#log .tool").length === 3`, "three tool cards");
  await ev(`document.querySelectorAll("details.toolgroup").forEach((d) => { d.open = true; })`);
  assert.strictEqual(await ev(`document.querySelectorAll("#log .tool-box").length`), 0, "closed cards build no output blocks");
  assert.strictEqual((await count()).length, 0, "nothing is fetched for closed cards");

  const card = (n) => `document.querySelectorAll("#log .tool")[${n}]`;
  await ev(`${card(0)}.querySelector(".notice-head").click()`);
  await until(ev, `${card(0)}.querySelector(".notice-body-wrap .tool-box") && ${card(0)}.textContent.includes(${JSON.stringify(tail)})`, "the full output of the first card");
  assert.strictEqual((await count()).length, 1, "one request for the opened card");
  // Closing and opening again asks for nothing more.
  await ev(`${card(0)}.querySelector(".notice-head").click()`);
  await ev(`${card(0)}.querySelector(".notice-head").click()`);
  await c.frames(3);
  assert.strictEqual((await count()).length, 1, "no second request");
  // The copy button holds the whole text.
  assert.ok(await ev(`[...${card(0)}.querySelectorAll(".tool-copy")].pop().hubText.includes(${JSON.stringify(tail)})`), "copy has the whole output");
  // A short output needs no request.
  await ev(`${card(1)}.querySelector(".notice-head").click()`);
  await until(ev, `!!${card(1)}.querySelector(".notice-body-wrap .tool-box")`, "the short card to open");
  assert.strictEqual((await count()).length, 1, "a short output is not fetched");
  assert.strictEqual(await ev(`document.querySelectorAll("#log .tool-box").length > 0`), true);
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
