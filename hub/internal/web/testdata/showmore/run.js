"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Opens the chat's quick panel at 1280 and 412 px, light and dark, clicks
// "Show N more" (tools) and "Show all N" (skills), and asserts the panel
// stays open and lists everything, and that a click outside still closes it.
// SHOWMORE_SHOT=dir also writes showmore-*.png screenshots.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");
const open = `!document.getElementById("qpanel").hidden`;
const link = (re) => `[...document.querySelectorAll("#qpanel button.more")].find((b) => ${re}.test(b.textContent))`;

async function run(c, w, dark) {
  const { ev, send } = c;
  const tag = w + (dark ? "-dark" : "-light");
  const mobile = w < 600;
  await send("Emulation.setDeviceMetricsOverride", { width: w, height: mobile ? 915 : 800, deviceScaleFactor: 1, mobile });
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions") && document.getElementById("sessions").children.length > 0`, tag + ": chats listed");
  await ev(`location.hash = "#/s/pc/s1"`);
  await until(ev, `!document.getElementById("qpbtn").hidden`, tag + ": panel button");
  await ev(`document.getElementById("qpbtn").click()`);
  await until(ev, `${link("/^Show \\d+ more/")}`, tag + ": Show N more is listed");
  const toolRows = `[...document.querySelectorAll("#qpanel .qsec")].find((s) => /^tools/i.test(s.innerText))`;
  const visible = (sel) => `[...${sel}.querySelectorAll("input.toggle")].filter((x) => x.offsetParent).length`;
  const before = await ev(visible(toolRows));
  const n = await ev(`Number(${link("/^Show \\d+ more/")}.textContent.match(/\\d+/)[0])`);
  // A real click at the link's position, so the page sees a trusted pointer event.
  // done is a page expression for what the click changes (omitted when it
  // changes nothing: then a frame or two lets the page's handlers run).
  const press = async (expr, done) => {
    await c.steady(expr, "the click target to hold still");
    const r = await ev(`(() => { const b = ${expr}; b.scrollIntoView({ block: "center" }); const r = b.getBoundingClientRect(); return [r.x + r.width / 2, r.y + r.height / 2]; })()`);
    for (const type of ["mousePressed", "mouseReleased"]) await send("Input.dispatchMouseEvent", { type, x: r[0], y: r[1], button: "left", clickCount: 1 });
    if (done) await until(ev, done, tag + ": the click to take effect"); else await c.frames(2);
  };
  await press(link("/^Show \\d+ more/"), `!${link("/^Show \\d+ more/")}`);
  assert.ok(await ev(open), tag + ": Show more closed the panel");
  const after = await ev(visible(toolRows));
  assert.strictEqual(after, before + n, `${tag}: all tools listed (${before} + ${n}, got ${after})`);
  assert.ok(!(await ev(link("/^Show \\d+ more/"))), tag + ": the link is gone");
  // Skills: Show all N.
  assert.ok(await ev(link("/^Show all \\d+/")), tag + ": Show all is listed");
  const sn = await ev(`Number(${link("/^Show all \\d+/")}.textContent.match(/\\d+/)[0])`);
  await press(link("/^Show all \\d+/"), `!${link("/^Show all \\d+/")}`);
  assert.ok(await ev(open), tag + ": Show all closed the panel");
  const skills = await ev(`(() => { const s = [...document.querySelectorAll("#qpanel .qsec")].find((x) => /^skills/i.test(x.innerText)); return [...s.querySelectorAll("input.toggle")].filter((x) => x.offsetParent).length; })()`);
  assert.ok(skills >= sn, `${tag}: all ${sn} skills listed, got ${skills}`);
  // The skill search is inside the panel too.
  await press(`document.querySelector("#qpanel input[type=search]")`);
  assert.ok(await ev(open), tag + ": the skill search closed the panel");
  if (process.env.SHOWMORE_SHOT) {
    await ev(`document.querySelector("#qpanel").scrollTop = 0`);
    await c.frames(2);
    fs.writeFileSync(process.env.SHOWMORE_SHOT + "/showmore-" + tag + ".png", Buffer.from((await send("Page.captureScreenshot", { format: "png" })).data, "base64"));
  }
  // A click outside still closes it.
  await send("Input.dispatchMouseEvent", { type: "mousePressed", x: 2, y: 2, button: "left", clickCount: 1 });
  await send("Input.dispatchMouseEvent", { type: "mouseReleased", x: 2, y: 2, button: "left", clickCount: 1 });
  await until(ev, `!(${open})`, tag + ": the panel to close after a click outside");
  assert.ok(!(await ev(open)), tag + ": a click outside did not close the panel");
}

(async () => {
  const c = await connect();
  try {
    await c.send("Page.enable");
    for (const w of [1280, 412]) for (const dark of [false, true]) await run(c, w, dark);
  } finally {
    c.close();
  }
})().catch((e) => { console.error(e.stack || e); process.exit(1); });
