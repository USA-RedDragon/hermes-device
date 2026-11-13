"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Asserts that a pending image chip in the composer opens the image viewer by
// tap, click, Enter and Space, that the x only removes, that Escape and Back
// close the viewer and leave no overlay catching taps, and that an image in a
// queued message bubble opens it too. Touch phone and desktop, light and dark.
// CHIPVIEW_SHOT=prefix also writes screenshots.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const { until } = require("../cdp.js");
// The viewer is up with an image, or gone with nothing left of it.
const shown = (ev) => until(ev, `!!document.querySelector(".lightbox img") && document.querySelector(".lightbox img").src`, "the viewer to open");
const gone = (ev, how) => until(ev, `document.querySelectorAll(".lightbox").length === 0`, how + ": the viewer to close");




async function device(c, w, h) {
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: true });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 5 });
}

// tap sends a real touch at the center of the element the selector finds.
async function tap(c, sel) {
  await c.steady(`document.querySelector(${JSON.stringify(sel)})`, sel + " to hold still");
  const r = await c.ev(`(() => { const e = document.querySelector(${JSON.stringify(sel)}); if (!e) return null; const b = e.getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);
  assert.ok(r, "no element for " + sel);
  await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: r.x, y: r.y }] });
  await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  return r;
}

async function swipe(c, x0, y0, x1, y1) {
  await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: x0, y: y0 }] });
  for (let i = 1; i <= 8; i++) {
    await c.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ x: x0 + (x1 - x0) * i / 8, y: y0 + (y1 - y0) * i / 8 }] });
    await c.frames(1); // one frame between moves: the finger's pace
  }
  await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await c.frames(2);
}
async function tapAt(c, x, y) {
  await c.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
  await c.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await c.frames(2);
}


const png = "iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mP8z8Dwn4EIwDiqkL4KAcT9GO0U4BxoAAAAAElFTkSuQmCC";

async function shot(c, name) {
  if (!process.env.CHIPVIEW_SHOT) return;
  fs.writeFileSync(process.env.CHIPVIEW_SHOT + name + ".png", Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
}

// addFile puts a PNG in the composer as a picker would.
const addFile = (ev, name) => ev(`(async () => {
  const bin = Uint8Array.from(atob(${JSON.stringify(png)}), (ch) => ch.charCodeAt(0));
  const dt = new DataTransfer();
  dt.items.add(new File([bin], ${JSON.stringify(name)}, { type: "image/png" }));
  const f = document.getElementById("files");
  f.files = dt.files;
  f.dispatchEvent(new Event("change"));
})()`);

// clean reports what could swallow a tap once the viewer is closed.
const clean = async (c, how) => {
  const { ev } = c;
  assert.strictEqual(await ev(`document.querySelectorAll(".lightbox").length`), 0, how + ": the viewer is gone");
  assert.strictEqual(await ev(`document.querySelectorAll("[inert]").length`), 0, how + ": inert left behind");
  assert.strictEqual(await ev(`getComputedStyle(document.body).pointerEvents`), "auto", how + ": body pointer-events");
  assert.ok(await ev(`!!document.activeElement && (document.activeElement.id === "input" || !!document.activeElement.closest("#chips"))`), how + ": focus is back in the composer");
  const hit = await ev(`(() => { const e = document.getElementById("input"); const b = e.getBoundingClientRect();
    const top = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2); return top === e; })()`);
  assert.ok(hit, how + ": the message box is under its own center");
};

async function run(c, w, h, touch, dark) {
  const { ev } = c;
  const tag = (w < 600 ? "phone" : "desktop") + "-" + (dark ? "dark" : "light");
  await c.send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: touch });
  await c.send("Emulation.setTouchEmulationEnabled", { enabled: touch, maxTouchPoints: 5 });
  await c.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await c.load(url + "/auth/login?next=/", `[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Work"))`, "the Work session is listed");
  await ev(`(() => { const e = [...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Work")).pop(); e.click(); })()`);
  await until(ev, `/Waiting to run/.test(document.querySelector("#log").innerText) && !!document.querySelector(".msg.user.pending .att img")`, "the chat to open with its waiting message");
  const press = async (sel) => {
    await c.steady(`document.querySelector(${JSON.stringify(sel)})`, sel + " to hold still");
    if (touch) return tap(c, sel);
    const r = await ev(`(() => { const b = document.querySelector(${JSON.stringify(sel)}).getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);
    for (const type of ["mousePressed", "mouseReleased"]) await c.send("Input.dispatchMouseEvent", { type, x: r.x, y: r.y, button: "left", clickCount: 1 });
    await c.frames(2);
  };
  const key = async (k, code) => {
    await c.send("Input.dispatchKeyEvent", { type: "keyDown", key: k, code, text: k === "Enter" ? "\r" : k === " " ? " " : undefined });
    await c.send("Input.dispatchKeyEvent", { type: "keyUp", key: k, code });
    await c.frames(2);
  };

  // The image in a message that waits to run opens the viewer.
  assert.ok(/Waiting to run/.test(await ev(`document.querySelector("#log").innerText`)), tag + ": a message waits to run");
  await ev(`document.querySelector(".msg.user.pending .att img").scrollIntoView()`);
  await press(".msg.user.pending .att img");
  assert.ok(await shown(ev), tag + ": the waiting message's image opens the viewer");
  await shot(c, tag + "-waiting");
  await key("Escape", "Escape");
  await gone(ev, tag + ": Escape on the waiting message's viewer");
  assert.strictEqual(await ev(`document.querySelectorAll(".lightbox").length`), 0, tag + ": waiting viewer closes");
  await ev(`document.getElementById("input").focus()`);

  await addFile(ev, "Screenshot_20261113_030545_Chrome.png");
  await until(ev, `document.querySelectorAll("#chips .chip").length === 1 && document.querySelector("#chips .chip img").src.startsWith("blob:")`, "the chip to show");
  assert.strictEqual(await ev(`document.querySelectorAll("#chips .chip").length`), 1, tag + ": one chip");
  const blob = await ev(`document.querySelector("#chips .chip img").src`);
  assert.ok(blob.startsWith("blob:"), tag + ": the chip shows a local image");
  assert.strictEqual(await ev(`getComputedStyle(document.querySelector("#chips .chip .open")).cursor`), "pointer", tag + ": pointer cursor");
  await shot(c, tag + "-chip");

  for (const [how, sel] of [["thumbnail", "#chips .chip img"], ["name", "#chips .chip .name"]]) {
    await press(sel);
    assert.ok(await shown(ev), `${tag}: ${how} opens the viewer`);
    assert.strictEqual(await ev(`document.querySelector(".lightbox img").src`), blob, tag + ": the viewer shows the local image");
    if (how === "thumbnail") await shot(c, tag + "-viewer");
    await key("Escape", "Escape");
    await gone(ev, `${tag}: Escape after ${how}`);
    await clean(c, `${tag}: Escape after ${how}`);
    assert.strictEqual(await ev(`document.querySelectorAll("#chips .chip").length`), 1, tag + ": opening does not remove");
  }

  // Keyboard: focus the chip's button, then Enter and Space.
  for (const [k, code] of [["Enter", "Enter"], [" ", "Space"]]) {
    await ev(`document.querySelector("#chips .chip .open").focus()`);
    await key(k, code);
    assert.ok(await shown(ev), `${tag}: ${code} opens the viewer`);
    await key("Escape", "Escape");
    await gone(ev, `${tag}: Escape after ${code}`);
    await clean(c, `${tag}: Escape after ${code}`);
  }

  // Back closes the viewer, stays in the chat and leaves taps working.
  const hash = await ev(`location.hash`);
  await press("#chips .chip img");
  assert.ok(await shown(ev), tag + ": opens before Back");
  await ev(`history.back()`);
  await gone(ev, tag + ": Back");
  await clean(c, tag + ": Back");
  assert.strictEqual(await ev(`location.hash`), hash, tag + ": Back stays in the chat");
  assert.strictEqual(await ev(`document.querySelectorAll("#chips .chip").length`), 1, tag + ": Back keeps the attachment");
  // Closing by a tap on the viewer pops its history entry too.
  const depth = await ev(`history.length`);
  await press("#chips .chip img");
  await shown(ev);
  await press(".lightbox img");
  await gone(ev, tag + ": a tap");
  assert.strictEqual(await ev(`document.querySelectorAll(".lightbox").length`), 0, tag + ": a tap closes the viewer");
  assert.strictEqual(await ev(`location.hash`), hash, tag + ": a tap close stays in the chat");
  assert.ok(await ev(`history.length`) <= depth + 1, tag + ": history does not pile up");

  // The x only removes.
  await press("#chips .chip > button:not(.open)");
  await until(ev, `document.querySelectorAll("#chips .chip").length === 0`, tag + ": the x to remove the chip");
  assert.strictEqual(await ev(`document.querySelectorAll(".lightbox").length`), 0, tag + ": x does not open the viewer");
  assert.strictEqual(await ev(`document.querySelectorAll("#chips .chip").length`), 0, tag + ": x removes the chip");

  // An image in a queued message opens the viewer.
  await addFile(ev, "queued.png");
  await until(ev, `document.querySelectorAll("#chips .chip").length === 1`, tag + ": the second chip to show");
  await ev(`(() => { const i = document.getElementById("input"); i.value = "look at this"; i.dispatchEvent(new Event("input", { bubbles: true })); })()`);
  await press("#send");
  await until(ev, `!!document.querySelector(".msg.user .att.image img")`, tag + ": the sent message to show its image");
  assert.ok(await ev(`!!document.querySelector(".msg.user .att.image img")`), tag + ": the sent message shows its image");
  await shot(c, tag + "-queued");
  await press(".msg.user .att.image img");
  assert.ok(await shown(ev), tag + ": the queued message's image opens the viewer");
  await key("Escape", "Escape");
  await gone(ev, tag + ": Escape on the queued message's viewer");
  assert.strictEqual(await ev(`document.querySelectorAll(".lightbox").length`), 0, tag + ": queued viewer closes");
}

(async () => {
  const c = await connect();
  await c.send("Page.enable");
  await run(c, 412, 915, true, false);
  await run(c, 412, 915, true, true);
  await run(c, 1280, 800, false, false);
  await run(c, 1280, 800, false, true);
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
