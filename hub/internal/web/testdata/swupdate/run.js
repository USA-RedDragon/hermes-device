"use strict";
// Drives headless Chrome. usage: node run.js CHROME HUB_URL PROFILE_DIR MODE
// The origin serves the previous release (page, script, service worker);
// the browser loads it, keeps its script and registers its worker. The
// origin then switches to the new release (GET /__new). MODE "prefetch"
// asks the old worker for the new files first. One reload later the page
// must be the new release whole: every script versioned, none of the old
// script, the app drawn.
const assert = require("assert");
const [bin, url, dir, mode] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");

(async () => {
  const t0 = Date.now(); const lap = (w) => console.log(w, Date.now() - t0);
  const c = await connect();
  const { ev } = c;
  await c.send("Page.enable");
  // Logged in, and landing on the old release's page.
  await c.load(url + "/auth/login?next=/", `window.__release === "old"`, "the old release's page");
  // The worker claims the page once it is active; wait for the claim.
  await until(ev, `!!navigator.serviceWorker.controller`, "the old worker to control the page");
  lap("controlled");
  assert.strictEqual(await ev(`document.title`), "old release");

  await fetch(url + "/__new");
  if (mode === "prefetch") {
    const r = await ev(`(async () => {
      const sw = navigator.serviceWorker.controller;
      return await new Promise((done) => {
        const ch = new MessageChannel();
        const t = setTimeout(() => done("timeout"), 8000);
        ch.port1.onmessage = (m) => { clearTimeout(t); done(m.data); };
        sw.postMessage({ type: "prefetch" }, [ch.port2]);
      });
    })()`);
    assert.strictEqual(r, "ready", "the old worker answered the prefetch");
  }
  lap("prefetched");
  await ev(`window.__gone = true; location.reload()`);
  await until(ev, `!window.__gone`, "the page to reload");
  await until(ev, `document.body.innerText.includes("Pick a chat")`, "the new release to draw after one reload").catch(async (e) => { console.error(await ev(`document.title + " " + document.body.innerText.slice(0, 300) + " " + [...document.scripts].map((s) => s.src).join()`)); throw e; });

  lap("drawn");
  const st = await ev(`({
    title: document.title,
    old: window.__release,
    app: typeof window.hubMarkdown,
    scripts: [...document.scripts].filter((s) => s.src).map((s) => s.getAttribute("src")),
    sheets: [...document.querySelectorAll("link[rel=stylesheet]")].map((l) => l.getAttribute("href")),
    loaded: performance.getEntriesByType("resource").filter((r) => r.name.includes("/static/")).map((r) => r.name.replace(location.origin, "")),
  })`);
  assert.notStrictEqual(st.title, "old release", "the old page is gone");
  assert.strictEqual(st.old, undefined, "the old script did not run");
  assert.ok(st.app.includes("object"), "the new scripts ran: " + st.app);
  assert.ok(st.scripts.length > 10, "scripts: " + st.scripts.length);
  for (const s of [...st.scripts, ...st.sheets].filter((x) => x.startsWith("/static/"))) assert.ok(/\?v=[0-9a-f]+$/.test(s), "unversioned reference " + s);
  assert.ok(!st.loaded.includes("/static/app.js"), "the old unversioned script was fetched or used");
  c.close();
  await new Promise((r) => { if (proc.exitCode !== null) r(); else proc.once("exit", r); });
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
