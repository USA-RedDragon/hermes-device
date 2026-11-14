"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR MODE   (MODE: open | locked)
// Settings > Agent > Long chats shows a slider (30% to 90%, step 5%) instead of
// pills. open: it renders with the saved value snapped to the nearest step
// (display only, nothing saved), shows the value while dragging without saving,
// saves once on release, moves 5% per arrow key, shows one line when the model
// will summarize later than chosen, and does not overflow. locked: the slider
// is disabled and nothing is sent. SLIDER_SHOT=dir writes slider-build-*.png.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, mode] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");
const R = "#sum-range";
let first = true;

(async () => {
  const c = await connect();
  const { ev, send } = c;
  const q = (sel) => `document.querySelector(${JSON.stringify(sel)})`;
  await send("Page.enable");
  for (const [w, h, mobile] of [[412, 900, true], [1280, 900, false]]) {
    for (const dark of [false, true]) {
      const tag = `${w}-${dark ? "dark" : "light"}`;
      await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile });
      await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
      await ev(`window.__old = true`);
      await send("Page.navigate", { url: url + "/auth/login?next=/" });
      await until(ev, `!window.__old && ${q("#qpbtn")} && ${q("#gearbtn")}`, tag + ": the hub page");
      await ev(`(() => { window.__puts = []; const f = window.fetch; window.fetch = function (u, o) { if (o && o.method === "PUT" && /settings/.test(String(u))) window.__puts.push(o.body); return f.apply(this, arguments); }; })()`);
      // The gear answers once the page has loaded the session; click until the dialog opens.
      await until(ev, `(() => { if (!document.getElementById("settings").open) ${q("#gearbtn")}.click(); return document.getElementById("settings").open; })()`, tag + ": Settings to open");
      if (process.env.SLIDER_DEBUG) console.log(await ev(`JSON.stringify({ open: document.getElementById("settings").open, tabs: document.getElementById("settab-agent") !== null, body: document.body.innerText.slice(0, 300), err: window.__err })`));
      await until(ev, q("#settab-agent"), tag + ": Settings tabs");
      await ev(`${q("#settab-agent")}.click()`);
      await until(ev, `${q(R)} && !${q("#set-agent .wait")}`, tag + ": the slider");
      const g = () => ev(`(() => { const r = document.querySelector("${R}"); return { v: r.value, min: r.min, max: r.max, step: r.step, dis: r.disabled, out: document.getElementById("sum-value").textContent,
        h: r.getBoundingClientRect().height, pills: [...document.querySelectorAll("#set-agent button")].filter((b) => /^\d+%$/.test(b.textContent.trim())).length, puts: window.__puts.length,
        floor: document.getElementById("sum-floor").hidden ? "" : document.getElementById("sum-floor").textContent,
        label: document.getElementById("sum-label").textContent, text: document.getElementById("set-agent").innerText, over: document.documentElement.scrollWidth > innerWidth }; })()`);
      let s = await g();
      assert.deepStrictEqual([s.min, s.max, s.step], ["30", "90", "5"], tag + ": range");
      assert.strictEqual(s.label, "Summarize older messages when the chat is this full", tag + ": label");
      assert.ok(s.text.includes("Lower keeps chats faster and cheaper; higher keeps more detail before summarizing."), tag + ": helper");
      assert.ok(s.h >= 36, tag + ": touch height " + s.h);
      assert.ok(!s.over, tag + ": no sideways overflow");
      assert.strictEqual(s.pills, 0, tag + ": no pills in Long chats");
      await ev(`${q(R)}.scrollIntoView({ block: "center" })`);
      const shot = async (n) => {
        if (!process.env.SLIDER_SHOT) return;
        await new Promise((r) => setTimeout(r, 200));
        fs.writeFileSync(`${process.env.SLIDER_SHOT}/slider-build-${n}.png`, Buffer.from((await send("Page.captureScreenshot", { format: "png" })).data, "base64"));
      };
      if (mode === "locked") {
        assert.ok(s.dis, tag + ": disabled when locked");
        assert.ok(/Set in the deployment config/.test(s.text), tag + ": lock line");
        assert.strictEqual(s.v, "35", tag + ": value shown snapped");
        assert.strictEqual(s.puts, 0, tag + ": nothing sent");
        await shot(tag + "-locked");
        continue;
      }
      assert.ok(!s.dis, tag + ": enabled");
      // The saved 0.33 shows at 35%; nothing was written.
      // The first load has the saved 0.33 (shown at 35%); later loads have what this run saved (50%).
      const start = first ? 35 : 50;
      first = false;
      assert.strictEqual(s.v, String(start), tag + ": snapped value");
      assert.strictEqual(s.out, start + "%", tag + ": value next to the slider");
      assert.strictEqual(s.puts, 0, tag + ": opening saves nothing");
      // The fake model has a small window, so 35% is below what it will use.
      await until(ev, `!document.getElementById("sum-floor").hidden`, tag + ": the window line");
      s = await g();
      assert.ok(/^With .+, summarizing starts at 75% at the earliest\.$/.test(s.floor), tag + ": line " + s.floor);
      // Drag: press, move; the value follows, nothing is saved until release.
      const pos = async (pct) => ev(`(() => { const r = document.querySelector("${R}").getBoundingClientRect(), t = 22.4; return { x: r.left + t / 2 + (${pct} - 30) / 60 * (r.width - t), y: r.top + r.height / 2 }; })()`);
      let p = await pos(start);
      await send("Input.dispatchMouseEvent", { type: "mousePressed", x: p.x, y: p.y, button: "left", clickCount: 1 });
      p = await pos(80);
      await send("Input.dispatchMouseEvent", { type: "mouseMoved", x: p.x, y: p.y, button: "left", buttons: 1 });
      s = await g();
      assert.strictEqual(s.v, "80", tag + ": dragged value");
      assert.strictEqual(s.out, "80%", tag + ": value shows while dragging");
      assert.strictEqual(s.puts, 0, tag + ": no save while dragging");
      assert.strictEqual(s.floor, "", tag + ": no line at 80%: " + s.floor);
      await send("Input.dispatchMouseEvent", { type: "mouseReleased", x: p.x, y: p.y, button: "left", clickCount: 1 });
      await until(ev, `window.__puts.length === 1`, tag + ": saved on release");
      assert.strictEqual(await ev(`window.__puts[0]`), JSON.stringify({ compression_threshold: 0.8 }), tag + ": body");
      await until(ev, `/Saved/.test(document.getElementById("set-agent").innerText)`, tag + ": saved note");
      // Keyboard: arrows move by 5% and save each.
      await ev(`${q(R)}.focus()`);
      await send("Input.dispatchKeyEvent", { type: "keyDown", key: "ArrowRight", code: "ArrowRight", windowsVirtualKeyCode: 39 });
      await send("Input.dispatchKeyEvent", { type: "keyUp", key: "ArrowRight", code: "ArrowRight", windowsVirtualKeyCode: 39 });
      await until(ev, `window.__puts.length === 2`, tag + ": arrow saves");
      s = await g();
      assert.strictEqual(s.v, "85", tag + ": arrow moves 5%");
      assert.strictEqual(s.out, "85%", tag + ": output after arrow");
      assert.strictEqual(await ev(`window.__puts[1]`), JSON.stringify({ compression_threshold: 0.85 }), tag + ": arrow body");
      // Back to the default 50%, for the screenshot: the line shows below 75%.
      for (let i = 0; i < 7; i++) {
        await send("Input.dispatchKeyEvent", { type: "keyDown", key: "ArrowLeft", code: "ArrowLeft", windowsVirtualKeyCode: 37 });
        await send("Input.dispatchKeyEvent", { type: "keyUp", key: "ArrowLeft", code: "ArrowLeft", windowsVirtualKeyCode: 37 });
      }
      await until(ev, `window.__puts.length === 9`, tag + ": seven more saves");
      s = await g();
      assert.strictEqual(s.v, "50", tag + ": back at 50");
      assert.ok(s.floor.includes("75%"), tag + ": line below 75%");
      assert.ok(!s.over, tag + ": no overflow");
      await shot(tag);
    }
  }
  c.close();
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
