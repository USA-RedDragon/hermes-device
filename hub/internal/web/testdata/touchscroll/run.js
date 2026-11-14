"use strict";
// Drives the real hub page in headless Chrome as a phone (mobile metrics,
// touch emulation) with touch events from the DevTools protocol. usage:
// node run.js CHROME HUB_URL PROFILE_DIR START_FILE STOP_FILE. The gateway
// streams a reply from START_FILE until STOP_FILE.
// A finger drags the log down a few pixels a frame, as a slow drag on a
// phone does: the view follows the finger from its first moves and never
// goes back toward the end, stays where the finger left it while the reply
// streams, and follows the reply again once the finger brings it back to the
// end. A flick goes on after the finger lifts, and the same drag moves an
// idle log.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, start, stop] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 412, height: 915, deviceScaleFactor: 2.625, mobile: true });
  await send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 5 });
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the chat list");
  await ev(`location.hash = "#/s/pc/ts"; 1`);
  await c.until(`document.getElementById("log").textContent.includes("Answer 59 fills") && document.querySelectorAll("#log .msg").length >= 20 && !document.querySelector("#log .loading") && !document.getElementById("log").hidden`, "the chat");
  await c.until(`window.HubLogSettled()`, "the log to settle");
  const gap = `(() => { const l = document.getElementById("log"); return Math.round(l.scrollHeight - l.scrollTop - l.clientHeight); })()`;
  assert.ok((await ev(gap)) < 2, "the chat opens at its end");
  const box = await ev(`(() => { const r = document.getElementById("log").getBoundingClientRect(); return { x: Math.round(r.x + r.width / 2), top: Math.round(r.top), bottom: Math.round(r.bottom) }; })()`);
  const pill = `!document.getElementById("tobottom").hidden`;
  // mark is the first message in view and where its top is.
  const mark = `(() => { const l = document.getElementById("log"); const top = l.getBoundingClientRect().top; const m = [...l.querySelectorAll(".msg")].find((x) => x.getBoundingClientRect().bottom > top + 40); return { i: [...l.querySelectorAll(".msg")].indexOf(m), top: Math.round(m.getBoundingClientRect().top), gap: Math.round(l.scrollHeight - l.scrollTop - l.clientHeight), pill: ${pill} }; })()`;
  const touch = (type, y) => send("Input.dispatchTouchEvent", { type, touchPoints: type === "touchEnd" ? [] : [{ x: box.x, y, id: 1, radiusX: 4, radiusY: 4, force: 1 }] });
  // drag moves a finger from y by step px a frame, n times, and returns the
  // scrollTop of the log after each move.
  const drag = async (y, step, n, lift = true) => {
    const tops = [];
    await touch("touchStart", y);
    await c.frames(1);
    tops.push(await ev(`Math.round(document.getElementById("log").scrollTop)`));
    for (let k = 1; k <= n; k++) {
      await touch("touchMove", y + step * k);
      await c.frames(1);
      tops.push(await ev(`Math.round(document.getElementById("log").scrollTop)`));
    }
    if (lift) await touch("touchEnd", y + step * n);
    return tops;
  };
  // back counts the moves that put the view back toward the end: a finger
  // that drags down only ever moves the log up.
  const back = (tops) => tops.filter((t, k) => k > 0 && t > tops[k - 1] + 1).length;

  // A reply streams while the reader is at the end: the view follows it.
  fs.writeFileSync(start, "go");
  await c.until(`/tok30 /.test(document.getElementById("log").textContent)`, "the reply to stream");
  await c.frames(4);
  assert.ok((await ev(gap)) < 30, "the view follows the reply");

  // A slow drag: 4 px a frame, 75 frames.
  let tops = await drag(box.top + 120, 4, 75);
  const t0 = tops.shift();
  assert.ok(t0 - tops[9] >= 15, `the view did not follow the first moves of the finger while streaming: scrollTop ${t0} then ${tops.slice(0, 10).join(" ")}`);
  assert.strictEqual(back(tops), 0, `the view went back toward the end during the drag while streaming: ${t0} ${tops.join(" ")}`);
  assert.ok(t0 - tops[tops.length - 1] >= 200, `the drag of 300 px moved the view ${t0 - tops[tops.length - 1]} px while streaming: ${tops.join(" ")}`);
  await c.frames(10);
  const a = await ev(mark);
  assert.ok(a.pill && a.gap > 150, "the reader left the end: " + JSON.stringify(a));
  await new Promise((r) => setTimeout(r, 700));
  const b = await ev(mark);
  assert.ok(b.i === a.i && Math.abs(b.top - a.top) < 2, "the view moved after the finger lifted: " + JSON.stringify([a, b]));
  assert.ok(b.gap > a.gap, "the reply grew below the reader: " + JSON.stringify([a, b]));

  // A flick: the log goes on moving after the finger lifts, and the
  // reply streaming below does not pull it back toward the end.
  const f0 = await ev(`Math.round(document.getElementById("log").scrollTop)`);
  const flick = await drag(box.top + 100, 40, 8);
  for (let k = 0; k < 40; k++) { await c.frames(1); flick.push(await ev(`Math.round(document.getElementById("log").scrollTop)`)); }
  assert.strictEqual(back(flick), 0, `the view went back toward the end during a flick while streaming: ${f0} ${flick.join(" ")}`);
  assert.ok(f0 - flick[flick.length - 1] >= 200, `a flick of 320 px moved the view ${f0 - flick[flick.length - 1]} px: ${flick.join(" ")}`);
  assert.ok(await ev(pill), "Jump to latest shows after the flick");

  // Dragging back to the end follows the reply again once the finger lifts.
  // Drags up of 600 px each, from low in the log (not on Jump to latest), until it reaches the end.
  const from = Math.round(box.top + (box.bottom - box.top) * 0.7);
  for (let k = 0; k < 20 && (await ev(gap)) > 30; k++) await drag(from, -30, 20);
  await c.until(`(${gap}) < 30 && !(${pill})`, "the view back at the end after the drag");
  await c.frames(4);
  const before = await ev(`document.getElementById("log").scrollHeight`);
  await c.until(`document.getElementById("log").scrollHeight > ${before + 60}`, "more of the reply");
  await c.frames(3);
  const pinnedAgain = await ev(mark);
  assert.ok(pinnedAgain.gap < 30 && !pinnedAgain.pill, "back at the end, the view follows the reply: " + JSON.stringify(pinnedAgain));

  // The reply ends; an idle log moves with the finger too.
  fs.writeFileSync(stop, "stop");
  await c.until(`/final-end/.test(document.getElementById("log").textContent) && !document.querySelector("#log .msg.streaming")`, "the reply to end");
  await new Promise((r) => setTimeout(r, 800));
  await c.frames(4);
  assert.ok((await ev(gap)) < 2, "the view is at the end when the reply ends");
  tops = await drag(box.top + 120, 3, 60);
  const i0 = tops.shift();
  assert.strictEqual(back(tops), 0, `the view went back toward the end during an idle drag: ${i0} ${tops.join(" ")}`);
  assert.ok(i0 - tops[tops.length - 1] >= 120, `an idle drag of 180 px moved the view ${i0 - tops[tops.length - 1]} px: ${tops.join(" ")}`);
  await new Promise((r) => setTimeout(r, 500));
  const idle = await ev(mark);
  assert.ok(idle.pill && idle.gap >= 120, "an idle drag leaves the end: " + JSON.stringify(idle));

  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
