"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Renders every card type with long unbroken content at 360, 412 and 1280 px,
// light and dark, and asserts nothing widens the page or the chat stream.
// OVERFLOW_SHOT=prefix also writes screenshots.
const { spawn } = require("child_process");
const assert = require("assert");
const fs = require("fs");
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



async function run(c, w, touch, dark) {
  const { ev } = c;
  const tag = w + (dark ? "-dark" : "-light");
  const h = w < 600 ? 915 : 800;
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: touch });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: touch, maxTouchPoints: 5 });
  await c.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await c.send("Page.navigate", { url: url + "/auth/login?next=/" });
  await sleep(1500);
  await ev(`(() => { const e = [...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Work")).pop(); e.click(); })()`);
  await sleep(1500);
  // The card types the page must have drawn, so the test cannot pass on an empty stream.
  const kinds = await ev(`({ approvals: document.querySelectorAll("#log .approval").length, tools: document.querySelectorAll("#log .tool, #log details.toolgroup").length,
    notices: document.querySelectorAll("#log .notice").length, table: document.querySelectorAll("#log .md table").length,
    code: document.querySelectorAll("#log .md pre").length, msgs: document.querySelectorAll("#log .msg").length })`);
  assert.ok(kinds.approvals >= 5 && kinds.tools >= 1 && kinds.notices >= 1 && kinds.table >= 1 && kinds.code >= 1 && kinds.msgs >= 2, tag + ": every card type is on the page " + JSON.stringify(kinds));
  // Open every folded group and card, so their bodies count too.
  await ev(`document.querySelectorAll("#log details").forEach((d) => { d.open = true; })`);
  await sleep(300);
  const r = await ev(`(() => {
    const vw = document.documentElement.clientWidth;
    const se = document.scrollingElement, log = document.getElementById("log");
    const bad = [];
    // Inside a box that scrolls on its own, wide content is fine.
    const scrolls = (n) => { for (let p = n.parentElement; p && p !== log; p = p.parentElement) { if (/(auto|scroll|hidden|clip)/.test(getComputedStyle(p).overflowX)) return true; } return false; };
    for (const n of log.querySelectorAll("*")) {
      if (scrolls(n)) continue;
      const b = n.getBoundingClientRect();
      if (!b.width) continue;
      if (b.right > vw + 0.5 || b.left < -0.5) bad.push(n.tagName + "." + String(n.getAttribute("class")) + " " + Math.round(b.left) + ".." + Math.round(b.right));
    }
    return { vw, docScroll: se.scrollWidth, docClient: se.clientWidth, logScroll: log.scrollWidth, logClient: log.clientWidth, logLeft: log.scrollLeft, nbad: bad.length, bad: bad.slice(0, 12) };
  })()`);
  assert.ok(r.docScroll <= r.docClient, `${tag}: the page scrolls sideways ${JSON.stringify(r)}`);
  assert.ok(r.logScroll <= r.logClient, `${tag}: the chat stream is wider than the screen ${JSON.stringify(r)}`);
  assert.strictEqual(r.logLeft, 0, `${tag}: the chat stream is scrolled sideways`);
  assert.strictEqual(r.nbad, 0, `${tag}: elements outside the screen ${JSON.stringify(r)}`);
  // The script of a browser approval sits in a box of its own, apart from the sentence, with no internal jargon.
  const a = await ev(`(() => { const a = [...document.querySelectorAll("#log .approval")].find((x) => /read-only script/.test(x.textContent)); if (!a) return null;
    const pre = a.querySelector("pre"); return { pre: !!pre && pre.textContent.includes("out.length<30"),
      prose: [...a.children].filter((c) => c.tagName !== "PRE").map((c) => c.textContent).join(" "), jargon: /plugin approval rule/.test(a.textContent) }; })()`);
  assert.ok(a && a.pre, tag + ": the script is in its own code block " + JSON.stringify(a));
  assert.ok(!a.prose.includes("out.length"), tag + ": the script is not in the sentence");
  assert.ok(!a.jargon, tag + ": no internal jargon in the card");
  if (process.env.OVERFLOW_SHOT) {
    const shot = async (name) => fs.writeFileSync(process.env.OVERFLOW_SHOT + name + ".png", Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
    await ev(`[...document.querySelectorAll("#log .approval")].find((x) => /read-only script/.test(x.textContent)).scrollIntoView({ block: "center" })`);
    await sleep(300);
    await shot(tag + "-eval");
    await ev(`document.getElementById("log").scrollTop = 0`);
    await sleep(300);
    await shot(tag + "-top");
  }
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  for (const dark of [false, true]) {
    await run(c, 360, true, dark);
    await run(c, 412, true, dark);
    await run(c, 1280, false, dark);
  }
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
