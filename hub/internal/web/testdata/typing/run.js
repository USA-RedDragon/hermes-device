"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Asserts that typing in the message box moves nothing (no layout shift, a
// steady composer, header, log and draft note, no page scroll) with the
// on-screen keyboard open, and that a touch screen never focuses the box
// when a chat is opened or switched, while a desktop still does.
const { spawn } = require("child_process");
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);

const proc = spawn(bin, ["--headless=new", "--no-sandbox", "--disable-gpu", "--remote-debugging-port=0", "--user-data-dir=" + dir, "--hide-scrollbars", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function connect() {
  const ws = await new Promise((res, rej) => {
    let buf = "";
    proc.stderr.on("data", (d) => { buf += d; const m = /ws:\/\/\S+/.exec(buf); if (m) res(m[0]); });
    proc.on("exit", () => rej(new Error("chrome exited: " + buf)));
  });
  const t = await (await fetch(`http://127.0.0.1:${new URL(ws).port}/json/new?about:blank`, { method: "PUT" })).json();
  const sock = new WebSocket(t.webSocketDebuggerUrl);
  await new Promise((r) => (sock.onopen = r));
  let id = 0;
  const pend = new Map();
  sock.onmessage = (m) => {
    const d = JSON.parse(m.data);
    if (d.id && pend.has(d.id)) { const { res, rej } = pend.get(d.id); pend.delete(d.id); d.error ? rej(new Error(JSON.stringify(d.error))) : res(d.result); }
  };
  const send = (method, params = {}) => new Promise((res, rej) => { const i = ++id; pend.set(i, { res, rej }); sock.send(JSON.stringify({ id: i, method, params })); });
  const ev = async (expr) => {
    const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails));
    return r.result.value;
  };
  return { send, ev, close: () => { sock.close(); proc.kill(); } };
}

const openChat = (ev, title) => ev(`(() => { const e = [...document.querySelectorAll("a,button,li,div")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith(${JSON.stringify(title)})).pop(); if (!e) return false; e.click(); return true; })()`);

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
  await send("Page.navigate", { url: url + "/auth/login?next=/" });
  await sleep(1500);
  assert.ok(await ev(`matchMedia("(pointer: coarse)").matches`), "pointer: coarse is emulated");
  for (const title of ["Long", "Second", "Long"]) {
    assert.ok(await openChat(ev, title), "chat " + title + " is listed");
    await sleep(1200);
    assert.strictEqual(await ev(`document.activeElement && document.activeElement.id`), "", "touch: opening " + title + " must not focus the message box");
  }

  // The keyboard is open: the layout viewport is shorter.
  await device(c, true, 412, 575);
  await sleep(500);
  await ev(`(() => { const i = document.getElementById("input"); i.focus(); i.value = ""; i.dispatchEvent(new Event("input")); })()`);
  await sleep(1500);
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
  const key = async (ch) => { await send("Input.dispatchKeyEvent", { type: "keyDown", text: ch, key: ch }); await send("Input.dispatchKeyEvent", { type: "keyUp", key: ch }); };
  // 20 quick keys, then 4 with pauses long enough for the draft to save.
  const typed = [];
  for (const ch of "hello there friend 12") { await key(ch); await sleep(120); typed.push(ch); }
  for (const ch of "abcd") { await key(ch); await sleep(1000); typed.push(ch); }
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
  await send("Page.navigate", { url: url + "/" });
  await sleep(1500);
  assert.ok(await openChat(ev, "Second"), "desktop: chat is listed");
  await sleep(1200);
  assert.strictEqual(await ev(`document.activeElement && document.activeElement.id`), "input", "desktop: opening a chat focuses the box");
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
