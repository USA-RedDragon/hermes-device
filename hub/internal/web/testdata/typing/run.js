"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Asserts that typing in the message box moves nothing (no layout shift, a
// steady composer, header, log and draft note, no page scroll) with the
// on-screen keyboard open, and that a touch screen never focuses the box
// when a chat is opened or switched, while a desktop still does.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);

const rowFor = (title) => `[...document.querySelectorAll("a,button,li,div")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith(${JSON.stringify(title)})).pop()`;
// Waits until the chat is listed, so a slow render does not race the click.
const openChat = async (c, title) => {
  const { ev } = c;
  await c.until(`!!(${rowFor(title)})`, "chat " + title + " to be listed");
  return ev(`(() => { const e = ${rowFor(title)}; if (!e) return false; e.click(); return true; })()`);
};

async function device(c, touch, w, h) {
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: touch });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: touch, ...(touch ? { maxTouchPoints: 5 } : {}) });
  await c.send("Emulation.setEmulatedMedia", { media: "", features: [{ name: "pointer", value: touch ? "coarse" : "fine" }, { name: "hover", value: touch ? "none" : "hover" }] });
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  const { ev, send } = c;

  // A touch screen: opening and switching chats leave the box unfocused.
  await device(c, true, 412, 915);
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the chat list");
  assert.ok(await ev(`matchMedia("(pointer: coarse)").matches`), "pointer: coarse is emulated");
  for (const title of ["Long", "Second", "Long"]) {
    assert.ok(await openChat(c, title), "chat " + title + " is listed");
    // The chat is drawn, so a focus on open would have happened by now.
    await c.until(`document.getElementById("title").textContent.startsWith(${JSON.stringify(title)}) && !document.querySelector("#log .loading") && document.querySelectorAll("#log .msg").length > 0`, "chat " + title + " to be drawn");
    await c.frames(2);
    assert.strictEqual(await ev(`document.activeElement && document.activeElement.id`), "", "touch: opening " + title + " must not focus the message box");
  }

  // The keyboard is open: the layout viewport is shorter.
  await device(c, true, 412, 575);
  await c.until(`innerHeight === 575`, "the short viewport to apply");
  await ev(`(() => { const i = document.getElementById("input"); i.focus(); i.value = ""; i.dispatchEvent(new Event("input")); })()`);
  await c.until(`document.activeElement.id === "input"`, "the message box to take focus");
  await c.frames(2);
  await ev(`(() => {
    window.__ls = 0;
    new PerformanceObserver((l) => { for (const e of l.getEntries()) window.__ls += e.value; }).observe({ type: "layout-shift" });
    const top = (s) => Math.round(document.querySelector(s).getBoundingClientRect().top * 100) / 100;
    const log = document.querySelector(".log");
    window.__snap = () => ({ comp: top(".composer"), row: top(".composer-row"), ta: document.getElementById("input").getBoundingClientRect().height,
      note: top("#draftnote"), hdr: top(".main > :first-child"), log: log.scrollTop, y: window.scrollY, vv: visualViewport.offsetTop });
    window.__rows = [];
    const loop = () => { window.__rows.push(window.__snap()); requestAnimationFrame(loop); };
    requestAnimationFrame(loop);
  })()`);
  const base = await ev(`window.__snap()`);
  // Count finished draft saves, so a pause lasts until the save is done.
  await ev(`(() => { window.__puts = 0; const f = window.fetch; window.fetch = (...a) => { const p = f(...a); if (a[1] && a[1].method === "PUT" && /\\/draft$/.test(String(a[0]))) p.finally(() => { window.__puts++; }); return p; }; })()`);
  const key = async (ch) => { await send("Input.dispatchKeyEvent", { type: "keyDown", text: ch, key: ch }); await send("Input.dispatchKeyEvent", { type: "keyUp", key: ch }); };
  // 20 quick keys, then 4 with pauses long enough for the draft to save.
  const typed = [];
  for (const ch of "hello there friend 12") { await key(ch); await c.frames(2); typed.push(ch); }
  for (const ch of "abcd") {
    const puts = await ev(`window.__puts`);
    await key(ch);
    await c.until(`window.__puts > ${puts}`, "the draft to save after " + ch);
    await c.frames(2);
    typed.push(ch);
  }
  const rows = await ev(`window.__rows`);
  const notes = await ev(`document.getElementById("draftnote").textContent`);
  assert.strictEqual(await ev(`document.getElementById("input").value`), "hello there friend 12abcd", "the keys reached the box");
  assert.ok(rows.length > 20, "frames were sampled");
  for (const k of ["comp", "row", "ta", "note", "hdr", "log", "y", "vv"]) {
    const seen = new Set(rows.map((r) => r[k]).concat([base[k]]));
    assert.strictEqual(seen.size, 1, `${k} moved while typing: ${[...seen].join(", ")}`);
  }
  assert.ok(base.hdr >= 0 && base.vv === 0 && base.y === 0, "the header stays on screen: " + JSON.stringify(base));
  assert.strictEqual(await ev(`window.__ls`), 0, "layout shift while typing");
  assert.ok(/Draft/.test(notes), "the draft note says something after a pause: " + notes);

  // A desktop: opening a chat still focuses the box.
  await device(c, false, 1200, 800);
  await c.load(url + "/", `document.getElementById("sessions").children.length > 0`, "the chat list on a desktop");
  assert.ok(await openChat(c, "Second"), "desktop: chat is listed");
  await c.until(`document.activeElement && document.activeElement.id === "input"`, "desktop: the box to take focus on open");
  assert.strictEqual(await ev(`document.activeElement && document.activeElement.id`), "input", "desktop: opening a chat focuses the box");
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
