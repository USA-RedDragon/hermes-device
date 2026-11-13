"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Opens the chat's quick panel and the Settings tabs that list tools,
// skills and connections, on a phone and on a desktop, and asserts that no
// snake_case or dotted id shows in the text a person reads.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
// until polls a page expression until it is truthy, so a step goes on when
// the page is ready and not after a guessed delay. A page that never gets
// there fails with what was awaited and the last value seen.
const WAIT_MS = Number(process.env.CDP_TIMEOUT_MS) || 30000;
const until = async (ev, expr, what) => {
  const end = Date.now() + WAIT_MS;
  let last;
  for (;;) {
    last = await ev(`(() => { try { return ${expr}; } catch (e) { return false; } })()`);
    if (last) return last;
    if (Date.now() > end) throw new Error(`gave up after ${WAIT_MS} ms waiting for ${what}; last value ${JSON.stringify(last)}`);
    await new Promise((r) => setTimeout(r, 25));
  }
};


// Visible text of a node (what innerText gives: hidden parts are left out).
const ids = /[a-z]+_[a-z_]+|[a-z]+\.[a-z]+_[a-z_]+/;
// Time zone names (Africa/Addis_Ababa) are place names, not ids: option lists are left out.
const text = (ev, sel) => ev(`(() => { const n = document.querySelector(${JSON.stringify(sel)}); if (!n) return ""; const h = [...n.querySelectorAll("select")]; h.forEach((x) => { x.dataset.was = x.style.display; x.style.display = "none"; }); const t = n.innerText; h.forEach((x) => { x.style.display = x.dataset.was; }); return t; })()`);
const click = (ev, sel) => ev(`(() => { const n = document.querySelector(${JSON.stringify(sel)}); if (!n) return false; n.click(); return true; })()`);

(async () => {
  const c = await connect();
  const { ev, send } = c;
  const el = (sel) => `document.querySelector(${JSON.stringify(sel)})`;
  await send("Page.enable");
  for (const [w, h, mobile] of [[360, 780, true], [1280, 800, false]]) {
    await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile });
    await ev(`window.__old = true`);
    await send("Page.navigate", { url: url + "/auth/login?next=/" });
    await until(ev, `!window.__old && ${el("#qpbtn")} && ${el("#gearbtn")}`, "the hub page to load");
    const where = w + "px ";
    // The quick panel of a chat: the button shows once the chat is open.
    await ev(`location.hash = "#/s/pc/s1"`);
    await until(ev, `${el("#qpbtn")}.hidden === false`, "the chat to open");
    assert.ok(await click(ev, "#qpbtn"), where + "the panel button is there");
    await until(ev, `!${el("#qpanel")}.hidden && /Brand new set/.test(${el("#qpanel")}.innerText)`, "the quick panel to list the tools");
    const quick = await text(ev, "#qpanel");
    assert.ok(/Web search/.test(quick), where + "quick panel lists tools: " + quick);
    assert.ok(/Search the web and read pages/.test(quick), where + "plain description");
    assert.ok(/Brand new set/.test(quick), where + "unknown id is humanized");
    assert.ok(!ids.test(quick), where + "quick panel shows an id: " + (ids.exec(quick) || [])[0] + "\n" + quick);
    await click(ev, "#qpscrim");
    await until(ev, `${el("#qpanel")}.hidden`, "the quick panel to close");
    // Settings.
    await click(ev, "#gearbtn");
    await until(ev, `${el("#settab-agent")}`, "the Settings tabs");
    // loaded: what each pane shows once its data has come.
    const loaded = {
      agent: `${el("#set-agent")}.innerText.includes("Personality") && !${el("#set-agent .wait")}`,
      diagnostics: `${el("#set-diagnostics .lst b")} && !${el("#set-diagnostics .wait")}`,
    };
    const part = { Skills: "Weather brief daily", Tools: "Web search", Connections: "Home assistant" };
    for (const tab of ["agent", "tools", "diagnostics"]) {
      assert.ok(await click(ev, "#settab-" + tab), where + "tab " + tab);
      if (tab === "tools") {
        for (const p of ["Skills", "Tools", "Connections"]) {
          await until(ev, `[...document.querySelectorAll("#set-tools button")].some((x) => x.textContent.trim() === ${JSON.stringify(p)})`, "the " + p + " button");
          await ev(`[...document.querySelectorAll("#set-tools button")].find((x) => x.textContent.trim() === ${JSON.stringify(p)}).click()`);
          await until(ev, `${el("#set-tools .tpart .lst > div")} && !${el("#set-tools .wait")}`, "the " + p + " list");
          const t = await text(ev, "#set-tools");
          assert.ok(new RegExp(part[p], "i").test(t), where + "settings " + p + " lists " + part[p] + ": " + t);
          assert.ok(!ids.test(t), where + "settings " + p + " shows an id: " + (ids.exec(t) || [])[0] + "\n" + t);
        }
      } else {
        await until(ev, loaded[tab], "the " + tab + " pane to load");
        const t = await text(ev, "#set-" + tab);
        assert.ok(!ids.test(t), where + "settings " + tab + " shows an id: " + (ids.exec(t) || [])[0] + "\n" + t);
      }
    }
  }
  c.close();
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
