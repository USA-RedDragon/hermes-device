"use strict";
// Drives the real hub page in headless Chrome (DevTools protocol) or headless
// Firefox (WebDriver BiDi; picked when the path has "firefox" in it) with a
// mouse press that is held while the chat list is drawn again.
// usage: node run.js BROWSER_BIN HUB_URL PROFILE_DIR
// The chat list is fetched again every few seconds and after every chat
// event, and the answer can arrive between the press and the release of a
// click on a chat: a list that replaces its rows then loses the click (the
// browser sends it to the list, not to the row that was pressed), so the
// reader has to click twice. Asserts that a click still opens the chat when
// the list is drawn again, unchanged or changed, between press and release;
// that an unchanged list leaves its rows alone; and that a change held back
// for a press shows once the press is over.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);
const kind = /firefox/.test(bin) ? "firefox" : "chrome";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// The page counts the lists it has fetched, and when asked changes the
// list: the first chat other than the open one then works, which moves it
// to the top of Recent.
const preload = `() => {
  window.__lists = 0;
  window.__flip = "";
  const f0 = window.fetch;
  window.fetch = async function (u, o) {
    const r = await f0.apply(this, arguments);
    if (!/\\/api\\/sessions(\\?|$)/.test(String(u)) || (o && o.method && o.method !== "GET")) return r;
    window.__lists++;
    if (!window.__flip) return r;
    const j = await r.clone().json();
    for (const s of j) if (s.id === window.__flip) s.running = true;
    const h = new Headers(r.headers);
    h.delete("content-length");
    return new Response(JSON.stringify(j), { status: r.status, headers: h });
  };
}`;

function driver() {
  if (kind === "firefox") {
    const { proc, connect } = require("../bidi.js").launch(bin, dir);
    return {
      proc,
      connect: async () => {
        const c = await connect();
        await c.send("script.addPreloadScript", { functionDeclaration: preload });
        await c.send("browsingContext.setViewport", { context: c.context, viewport: { width: 1504, height: 1002 } });
        const act = (actions) => c.send("input.performActions", { context: c.context, actions: [{ type: "pointer", id: "m", parameters: { pointerType: "mouse" }, actions }] });
        return {
          ev: c.ev,
          nav: (u) => c.goto(u),
          move: (x, y) => act([{ type: "pointerMove", x: Math.round(x), y: Math.round(y) }]),
          down: () => act([{ type: "pointerDown", button: 0 }]),
          up: () => act([{ type: "pointerUp", button: 0 }]),
          close: c.close,
        };
      },
    };
  }
  const { proc, connect } = require("../cdp.js").launch(bin, dir);
  return {
    proc,
    connect: async () => {
      const c = await connect();
      await c.send("Page.enable");
      await c.send("Page.addScriptToEvaluateOnNewDocument", { source: "(" + preload + ")()" });
      await c.send("Emulation.setDeviceMetricsOverride", { width: 1504, height: 1002, deviceScaleFactor: 1, mobile: false });
      let at = { x: 0, y: 0 };
      const mouse = (type, x, y) => c.send("Input.dispatchMouseEvent", { type, x, y, button: "left", buttons: type === "mousePressed" ? 1 : 0, clickCount: 1 });
      return {
        ev: c.ev,
        nav: (u) => c.send("Page.navigate", { url: u }),
        move: async (x, y) => { at = { x, y }; await mouse("mouseMoved", x, y); },
        down: () => mouse("mousePressed", at.x, at.y),
        up: () => mouse("mouseReleased", at.x, at.y),
        close: c.close,
      };
    },
  };
}

const { proc, connect } = driver();
(async () => {
  const c = await connect();
  const { ev } = c;
  const until = async (expr, what, ms = 15000) => {
    const end = Date.now() + ms;
    for (;;) {
      const v = await ev(`(() => { try { return ${expr}; } catch (e) { return false; } })()`);
      if (v) return v;
      if (Date.now() > end) throw new Error("timed out waiting for " + what);
      await sleep(25);
    }
  };
  const row = (id) => `document.querySelector('#sessions a.go[href="#/s/pc/${id}"]')`;
  const box = (id) => ev(`(() => { const a = ${row(id)}; if (!a) return null; const r = a.getBoundingClientRect(); return { x: r.x + Math.min(40, r.width / 2), y: r.y + r.height / 2 }; })()`);
  await c.nav(url + "/auth/login?next=/");
  await until(`document.querySelectorAll('#sessions a.go[href^="#/s/pc/"]').length >= 3`, "the chats in the list");
  await until(`innerWidth > 1400`, "the desktop size");

  // refetch asks for the list again, as the page does on a chat event, and
  // waits until the answer is in and drawn.
  const refetch = async () => {
    const n = await ev(`window.__lists`);
    await ev(`(() => { document.dispatchEvent(new Event("visibilitychange")); return 1; })()`);
    await until(`window.__lists > ${n}`, "the list fetched again");
    await sleep(150);
  };
  const hashNow = () => ev(`location.hash`);
  const home = async () => { await ev(`(() => { location.hash = "#/"; return 1; })()`); await until(`location.hash === "#/" || location.hash === ""`, "home"); await sleep(100); };
  const tryClick = async (id, between, what) => {
    const p = await box(id);
    assert.ok(p, "no row for " + id);
    await ev(`(() => { window.__pressed = ${row(id)}; return 1; })()`);
    await c.move(p.x, p.y);
    await c.down();
    await between();
    await c.up();
    try { await until(`location.hash === "#/s/pc/${id}"`, "chat " + id, 2500); } catch (_) {
      assert.fail(`${what}: the click on ${id} did nothing, the page stayed on "${await hashNow()}"`);
    }
  };

  // A click with no redraw opens the chat.
  await tryClick("s2", async () => {}, "a plain click");
  await home();

  // The list comes again, unchanged, between press and release.
  await tryClick("s3", async () => { await refetch(); }, "an unchanged list drawn between press and release");
  await home();

  // The list comes again with the pressed chat working, which moves it.
  await ev(`(() => { window.__flip = "s4"; return 1; })()`);
  await tryClick("s4", async () => { await refetch(); }, "a changed list drawn between press and release");
  await home();
  await ev(`(() => { window.__flip = ""; return 1; })()`);

  // An unchanged list leaves the rows as they are, so a press, a hover and
  // a focus on one stay on it (the first list after a chat opens may still
  // change what it shows, as the chat is read).
  await refetch();
  await ev(`(() => { window.__rowA = ${row("s2")}; return 1; })()`);
  await refetch();
  await refetch();
  assert.ok(await ev(`window.__rowA.isConnected && window.__rowA === ${row("s2")}`), "an unchanged chat list replaced its rows");

  // A change held back for a press shows once the press is over.
  await ev(`(() => { window.__flip = "s4"; return 1; })()`);
  const p5 = await box("s3");
  await c.move(p5.x, p5.y);
  await c.down();
  await refetch();
  await c.up();
  await until(`location.hash === "#/s/pc/s3"`, "chat s3 to open", 2500);
  await until(`document.querySelector('#sessions a.go[href="#/s/pc/s4"] .mark.running')`, "the working mark of s4 after the press");
  await ev(`(() => { window.__flip = ""; return 1; })()`);
  await home();

  // Many clicks, the list drawn again at a different moment in each.
  const ids = ["s2", "s3", "s4"];
  for (let i = 0; i < 12; i++) {
    const id = ids[i % ids.length];
    await ev(`(() => { window.__flip = ${i % 2 ? JSON.stringify(ids[(i + 1) % ids.length]) : '""'}; return 1; })()`);
    await tryClick(id, async () => { if (i % 3) await sleep(i * 7); await refetch(); }, `click ${i}`);
    await home();
  }
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
