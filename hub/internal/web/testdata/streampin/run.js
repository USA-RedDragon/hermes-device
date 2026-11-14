"use strict";
// Drives the real hub page in headless Chrome. usage: node run.js CHROME
// HUB_URL PROFILE_DIR SIGNAL_FILE. A reply streams into a long chat while
// the page is watched: at the bottom, the view stays at the bottom and the
// streamed block is what a full render draws; once the reader scrolls up,
// the view stays on the same message while the reply goes on.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, signal] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false });
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the chat list");
  await ev(`location.hash = "#/s/pc/sp"; 1`);
  // The page draws a window of the chat, its newest rows first.
  await c.until(`document.getElementById("log").textContent.includes("Answer 59 fills") && document.querySelectorAll("#log .msg").length >= 20 && !document.querySelector("#log .loading")`, "the chat");
  await c.until(`window.HubLogSettled()`, "the log to settle");
  const gap = `(() => { const l = document.getElementById("log"); return l.scrollHeight - l.scrollTop - l.clientHeight; })()`;
  assert.ok((await ev(gap)) < 2, "the chat opens at its end");

  // The reply keeps growing, and the page draws what arrived once a frame, so a
  // sample can land between a word and its frame: the block is compared with a
  // full render of the text it holds, sampled until the two are from the same frame.
  const matches = async () => { let r; for (let i = 0; i < 200; i++) { r = await ev(same); if (r.live) return r; await new Promise((f) => setTimeout(f, 7)); } return r; };
  // Watch the gap each frame from the first streamed word on.
  await ev(`window.__gaps = []; window.__watch = true; (() => { const l = document.getElementById("log"); const f = () => { if (/tok0 /.test(l.textContent)) window.__gaps.push(l.scrollHeight - l.scrollTop - l.clientHeight); if (window.__watch) requestAnimationFrame(f); }; requestAnimationFrame(f); })(); 1`);
  fs.writeFileSync(signal, "go");
  await c.until(`/tok0 /.test(document.getElementById("log").textContent)`, "the reply to start");
  // The first segment is 150 words, then a pause: wait for the pause.
  await c.until(`/tok149 /.test(document.getElementById("log").textContent)`, "the first segment");
  await c.frames(6);
  const same = `(() => { const nodes = document.querySelectorAll("#log .msg.assistant.md"); const n = nodes[nodes.length - 1]; const d = document.createElement("div"); window.hubMarkdown.render(d, n.raw); const plain = (x) => { const c = x.cloneNode(true); c.querySelectorAll("code.hljs").forEach((k) => { k.textContent = k.textContent; k.removeAttribute("class"); }); return c.innerHTML; }; return { live: plain(n) === plain(d), inc: !!n.mdInc, kids: n.childNodes.length, raw: n.raw.length }; })()`;
  // Under load the frame that draws the last words can come late: wait for it.
  let s = await matches();
  assert.ok(s.live, "mid-stream block differs from a full render: " + JSON.stringify(s));
  assert.ok(s.inc && s.kids > 3, "the stream drew incrementally: " + JSON.stringify(s));
  let gaps = await ev(`window.__gaps.slice()`);
  assert.ok(gaps.length > 20, "frames watched: " + gaps.length);
  assert.ok(Math.max(...gaps) < 30, "the view left the bottom while streaming: max gap " + Math.max(...gaps));

  // The reader scrolls up in the second segment and keeps their place.
  await c.until(`/tok160 /.test(document.getElementById("log").textContent)`, "the second segment");
  const p = await ev(`(() => { const r = document.getElementById("log").getBoundingClientRect(); return { x: r.x + r.width / 2, y: r.y + r.height / 2 }; })()`);
  await ev(`window.__watch = false; 1`);
  for (let i = 0; i < 3; i++) { await send("Input.dispatchMouseEvent", { type: "mouseWheel", x: p.x, y: p.y, deltaX: 0, deltaY: -400 }); await c.frames(2); }
  const mark = `(() => { const l = document.getElementById("log"); const top = l.getBoundingClientRect().top; const m = [...l.querySelectorAll(".msg.user")].find((x) => x.getBoundingClientRect().bottom > top + 40); return { q: m.textContent, top: m.getBoundingClientRect().top, gap: l.scrollHeight - l.scrollTop - l.clientHeight, pill: !document.getElementById("tobottom").hidden }; })()`;
  const a = await ev(mark);
  assert.ok(a.gap > 300, "the reader scrolled away: " + JSON.stringify(a));
  await c.until(`/tok300 /.test(document.getElementById("log").textContent)`, "more words to stream");
  const b = await ev(mark);
  assert.strictEqual(b.q, a.q, "same message in view");
  assert.ok(Math.abs(b.top - a.top) < 2, "the view moved while the reader read: " + JSON.stringify([a, b]));
  assert.ok(b.pill, "the jump pill shows");
  assert.ok(b.gap > a.gap, "the reply grew below the reader");

  // The end: the reply is what a full render of its text draws.
  await c.until(`/final-end/.test(document.getElementById("log").textContent)`, "the reply to end");
  await c.frames(6);
  s = await matches();
  assert.ok(s.live, "the final block differs from a full render: " + JSON.stringify(s));
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
