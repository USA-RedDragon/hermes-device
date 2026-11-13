"use strict";
// Drives the real hub page in headless Chrome (DevTools protocol) or headless
// Firefox (WebDriver BiDi) with real mouse and key input.
// usage: node run.js BROWSER_BIN HUB_URL PROFILE_DIR (Chrome, or Firefox when
// the path has "firefox" in it)
// Asserts that a page's hermes.send, data-hermes-send and hermes.draft go
// through only right after the user's own click or key press in the page,
// once per activation, and that nothing the page's script does by itself can
// stand in for one.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);
const kind = /firefox/.test(bin) ? "firefox" : "chrome";
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function driver() {
  if (kind === "firefox") {
    const { proc, connect } = require("../bidi.js").launch(bin, dir);
    return { proc, connect: async () => { const c = await connect(); return { ...c, nav: (u) => c.goto(u) }; } };
  }
  const { proc, connect } = require("../cdp.js").launch(bin, dir);
  return {
    proc,
    connect: async () => {
      const c = await connect();
      await c.send("Page.enable");
      const mouse = (type, x, y) => c.send("Input.dispatchMouseEvent", { type, x, y, button: "left", buttons: type === "mousePressed" ? 1 : 0, clickCount: 1 });
      return {
        ev: c.ev,
        nav: (u) => c.send("Page.navigate", { url: u }),
        click: async (x, y) => { await mouse("mouseMoved", x, y); await mouse("mousePressed", x, y); await mouse("mouseReleased", x, y); },
        enter: async () => {
          const k = { key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13 };
          await c.send("Input.dispatchKeyEvent", { type: "keyDown", text: "\r", ...k });
          await c.send("Input.dispatchKeyEvent", { type: "keyUp", ...k });
        },
        shot: async () => (await c.send("Page.captureScreenshot", { format: "png" })).data,
        close: c.close,
      };
    },
  };
}

const { proc, connect } = driver();
let finished = false;
process.on("exit", (code) => { if (!finished && code === 0) { console.error("a test never finished"); process.exitCode = 1; } });

(async () => {
  const c = await connect();
  const { ev } = c;
  const until = async (expr, what) => {
    for (let i = 0; i < 300; i++) {
      if (await ev(expr)) return;
      await sleep(50);
    }
    const seen = await ev(`JSON.stringify({ res: window.__res.filter((m) => m.t).slice(-6), posts: window.__posts.length, input: document.getElementById('input').value, note: (document.querySelector(".pageview-note") || {}).textContent, active: navigator.userActivation.isActive, focus: document.activeElement && document.activeElement.tagName })`).catch((e) => String(e));
    if (process.env.PAGEACT_SHOT && c.shot) require("fs").writeFileSync(process.env.PAGEACT_SHOT, Buffer.from(await c.shot(), "base64"));
    throw new Error("timed out waiting for " + what + "; hub saw " + seen);
  };
  // Settles long enough for a refused or accepted send to have been answered.
  const quiet = () => sleep(800);
  const posts = () => ev(`window.__posts.map((p) => p.text.replace(/^[^\\n]*\\n/, ""))`);
  const results = (t) => ev(`window.__res.filter((m) => m.t === ${JSON.stringify(t)}).map((m) => m.r)`);
  const note = () => ev(`(document.querySelector(".pageview-note") || {}).textContent || ""`);
  const reset = () => ev(`(window.__posts.length = 0, window.__res.length = 0, 0)`);
  // click is a real mouse click at the center of the page's button.
  const click = async (id) => {
    const n = await ev(`window.__res.filter((m) => m.rects).length`);
    await cmd({ cmd: "rects" });
    await until(`window.__res.filter((m) => m.rects).length > ${n}`, "the button positions");
    const p = await ev(`(() => { const r = window.__res.filter((m) => m.rects).pop().rects[${JSON.stringify(id)}]; const f = document.querySelector(".pageview iframe").getBoundingClientRect(); return { x: f.left + r[0], y: f.top + r[1] }; })()`);
    if (!(p.y > 0 && p.x > 0)) throw new Error("button " + id + " is off the screen: " + JSON.stringify(await ev(`({ rects: window.__res.filter((m) => m.rects).pop().rects, frame: document.querySelector(".pageview iframe").getBoundingClientRect().toJSON(), n: window.__res.length })`)));
    await frameClick(p.x, p.y);
  };
  // frameClick clicks inside the page's frame. A click that lands on the hub
  // itself (the browser has not yet routed mouse input to a frame it just
  // loaded) is a gesture of the hub's, not the page's: it is made again once
  // the hub has settled.
  const frameClick = async (x, y) => {
    for (let i = 0; i < 4; i++) {
      const hub = await ev(`window.__hub || 0`);
      await c.click(x, y);
      await sleep(250);
      if ((await ev(`window.__hub || 0`)) === hub) return;
      await sleep(400);
    }
    throw new Error("a click at " + x + "," + y + " never reached the page's frame");
  };
  const cmd = (m) => ev(`document.querySelector(".pageview iframe").contentWindow.postMessage(${JSON.stringify(m)}, "*")`);

  async function openPage() {
    await c.nav(url + "/auth/login?next=/");
    await until(`[...document.querySelectorAll("#sessions *")].some((x) => x.children.length < 4 && x.textContent.trim().startsWith("Pages"))`, "the Pages session is listed");
    await ev(`(() => {
      window.__posts = []; window.__res = [];
      addEventListener("pointerdown", () => { window.__hub = (window.__hub || 0) + 1; }, true);
      const f = window.fetch;
      window.fetch = (u, o) => { if (String(u).endsWith("/prompt") && o && typeof o.body === "string") window.__posts.push(JSON.parse(o.body)); return f.call(window, u, o); };
      addEventListener("message", (e) => { if (e.data && (e.data.t || e.data.rects)) window.__res.push(e.data); });
      [...document.querySelectorAll("#sessions *")].filter((x) => x.children.length < 4 && x.textContent.trim().startsWith("Pages")).pop().click();
    })()`);
    await until(`!!document.querySelector(".page-card button")`, "the page card");
    await openCard();
  }
  async function openCard() {
    await until(`!!document.querySelector(".page-card button")`, "the page card");
    // The user's own click in the hub opens the page. It is made again if the
    // layout moved under it.
    for (let i = 0; i < 5 && !(await ev(`!!document.querySelector(".pageview:not([hidden]) iframe")`)); i++) {
      await sleep(300);
      const b = await ev(`(() => { document.querySelector(".page-card button").scrollIntoView({ block: "center" }); const r = document.querySelector(".page-card button").getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
      await c.click(b.x, b.y);
      await sleep(500);
    }
    await until(`window.__res.some((m) => m.t === "onload")`, "the page's own send at load to be answered");
  }

  await openPage();

  // The page sends at load, right after the user's click that opened it. The
  // hub's own click must not count.
  assert.strictEqual((await results("onload"))[0].ok, false, "a send at load, right after the hub's click, is refused");
  assert.deepStrictEqual(await posts(), [], "nothing was sent at load");
  assert.match(await note(), /only right after your click or key press/);
  assert.strictEqual(await ev(`!!document.querySelector(".pageview-note .ask, .pageview-note q")`), false, "no confirm line");

  // A click on the page's button sends once, with no question first.
  await reset();
  await click("one");
  await until(`window.__posts.length >= 1`, "the click's send");
  await quiet();
  assert.deepStrictEqual(await posts(), ["one"]);
  assert.deepStrictEqual(await results("one"), [{ ok: true, error: "" }]);
  assert.match(await note(), /^Sent to the chat/);

  // A script alone cannot send, not even a moment after a real click.
  await reset();
  await sleep(300);
  await cmd({ cmd: "send" });
  await until(`window.__res.some((m) => m.t === "auto")`, "the script's send to be answered");
  assert.strictEqual((await results("auto"))[0].ok, false);
  assert.deepStrictEqual(await posts(), []);

  // A script that clicks the button itself is not a click of the user's.
  await reset();
  await cmd({ cmd: "click", id: "one" });
  await until(`window.__res.some((m) => m.t === "one")`, "the scripted click's send to be answered");
  assert.strictEqual((await results("one"))[0].ok, false, "a click event made by the script is refused");
  assert.deepStrictEqual(await posts(), []);

  // One click is one send: five sends off one click send one.
  await reset();
  await click("loop");
  await until(`window.__res.filter((m) => /^loop/.test(m.t)).length === 5`, "all five answers");
  await quiet();
  const loop = (await ev(`window.__res.filter((m) => /^loop/.test(m.t)).map((m) => m.r.ok)`));
  assert.strictEqual(loop.filter(Boolean).length, 1, "one of five sends off one click goes through: " + JSON.stringify(loop));
  assert.strictEqual((await posts()).length, 1);

  // A loop of sends that starts after one click sends at most that one.
  await reset();
  await click("one");
  await cmd({ cmd: "loopsend" });
  await until(`window.__posts.length >= 1`, "the click's send");
  await sleep(1500);
  const after = await posts();
  assert.ok(after.length <= 1, "a loop after one click sent " + after.length);
  await cmd({ cmd: "stop" });
  await sleep(500); // the page's loop is over, so it cannot take the next gesture

  // A key press counts: Enter on a focused button.
  await reset();
  // A click on the blank part of the page gives its window the keyboard
  // focus. The activation that click leaves is ended by hand, so that the
  // key press is the only gesture the hub can count.
  const fr = await ev(`(() => { const r = document.querySelector(".pageview iframe").getBoundingClientRect(); return { x: r.left + 300, y: r.top + 100 }; })()`);
  await frameClick(fr.x, fr.y);
  await ev(`(() => { const i = document.createElement("input"); i.setAttribute("list", "none"); try { i.showPicker(); } catch (_) {} return navigator.userActivation.isActive; })()`).then((a) => assert.strictEqual(a, false, "the test could not end the activation"));
  await cmd({ cmd: "focus", id: "key" });
  await until(`window.__res.some((m) => m.t === "focus" && m.r[0] === "key")`, "the key button to have focus in the page");
  await reset();
  await c.enter();
  await until(`window.__posts.length >= 1`, "the key press's send");
  await quiet();
  assert.deepStrictEqual(await posts(), ["key"]);

  // The hub's own click does not leave an activation for the page: Reload
  // starts the page again, which sends at load.
  await reset();
  const rb = await ev(`(() => { const r = [...document.querySelectorAll(".pageview-bar button")].find((b) => b.textContent === "Reload").getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
  await c.click(rb.x, rb.y);
  await sleep(1000);
  await until(`window.__res.some((m) => m.t === "onload")`, "the reloaded page's send at load");
  assert.strictEqual((await results("onload"))[0].ok, false, "a click on Reload does not let the page send");
  assert.deepStrictEqual(await posts(), []);

  // data-hermes-send follows the same rule.
  await reset();
  await click("ds");
  await until(`window.__posts.length >= 1`, "data-hermes-send's send");
  await quiet();
  assert.deepStrictEqual(await posts(), ["ds"]);
  await reset();
  await cmd({ cmd: "click", id: "ds" });
  await quiet();
  assert.deepStrictEqual(await posts(), [], "a scripted click on a data-hermes-send element sends nothing");

  // hermes.copy is not held to the rule.
  await reset();
  await cmd({ cmd: "copy" });
  await until(`window.__res.some((m) => m.t === "copy")`, "the copy call");
  // Copy goes through with no activation: the bar says Copied, or, where the
  // browser will not let the hub write the clipboard, the copy box opens.
  await until(`/Copied/.test(document.querySelector(".pageview-bar .status").textContent) || !!document.querySelector("dialog.copybox")`, "the copy to show");
  await ev(`(document.querySelectorAll("dialog.copybox").forEach((d) => d.close()), 0)`);

  // hermes.draft: refused alone, accepted on a click (and the page closes after it).
  await reset();
  await cmd({ cmd: "draft" });
  await until(`window.__res.some((m) => m.t === "auto-draft")`, "the script's draft to be answered");
  assert.strictEqual((await results("auto-draft"))[0].ok, false);
  assert.strictEqual(await ev(`document.getElementById("input").value`), "", "a script's draft is not put in the message box");
  await cmd({ cmd: "click", id: "dd" });
  await quiet();
  assert.strictEqual(await ev(`document.getElementById("input").value`), "", "a scripted click on a data-hermes-draft element drafts nothing");
  await click("hd");
  await until(`document.getElementById("input").value.includes("hd")`, "the click's draft");
  // A draft leaves the page; open it again from its card.
  await sleep(500);
  if (!(await ev(`!!document.querySelector(".pageview:not([hidden]) iframe")`))) { await ev(`(window.__res.length = 0, 0)`); await openCard(); await sleep(800); }
  // The hub shows the same frame again, which has left its page: Reload, as
  // the user would, starts the page again.
  if (/page left/.test(await ev(`document.querySelector(".pageview-bar .status").textContent`))) {
    await ev(`(window.__res.length = 0, 0)`);
    const rb2 = await ev(`(() => { const r = [...document.querySelectorAll(".pageview-bar button")].find((b) => b.textContent === "Reload").getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
    await c.click(rb2.x, rb2.y);
    await until(`window.__res.some((m) => m.t === "onload")`, "the page to start again");
    await sleep(1000);
  }
  await reset();
  await click("dd");
  await until(`document.getElementById("input").value.includes("dd")`, "data-hermes-draft's draft");

  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
  finished = true;
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
