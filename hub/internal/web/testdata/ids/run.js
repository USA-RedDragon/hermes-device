"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR
// Opens the chat's quick panel and the Settings tabs that list tools,
// skills and connections, on a phone and on a desktop, and asserts that no
// snake_case or dotted id shows in the text a person reads.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));


// Visible text of a node (what innerText gives: hidden parts are left out).
const ids = /[a-z]+_[a-z_]+|[a-z]+\.[a-z]+_[a-z_]+/;
// Time zone names (Africa/Addis_Ababa) are place names, not ids: option lists are left out.
const text = (ev, sel) => ev(`(() => { const n = document.querySelector(${JSON.stringify(sel)}); if (!n) return ""; const h = [...n.querySelectorAll("select")]; h.forEach((x) => { x.dataset.was = x.style.display; x.style.display = "none"; }); const t = n.innerText; h.forEach((x) => { x.style.display = x.dataset.was; }); return t; })()`);
const click = (ev, sel) => ev(`(() => { const n = document.querySelector(${JSON.stringify(sel)}); if (!n) return false; n.click(); return true; })()`);

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  for (const [w, h, mobile] of [[360, 780, true], [1280, 800, false]]) {
    await send("Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile });
    await send("Page.navigate", { url: url + "/auth/login?next=/" });
    await sleep(1500);
    const where = w + "px ";
    // The quick panel of a chat.
    await ev(`location.hash = "#/s/pc/s1"`);
    await sleep(1200);
    assert.ok(await click(ev, "#qpbtn"), where + "the panel button is there");
    await sleep(1500);
    const quick = await text(ev, "#qpanel");
    if (!quick) console.log(await ev(`JSON.stringify({b: document.getElementById("qpbtn").hidden, p: document.getElementById("qpanel").hidden, h: document.querySelector("#qpanel").innerHTML.slice(0,300), t: document.title})`));
    assert.ok(/Web search/.test(quick), where + "quick panel lists tools: " + quick);
    assert.ok(/Search the web and read pages/.test(quick), where + "plain description");
    assert.ok(/Brand new set/.test(quick), where + "unknown id is humanized");
    assert.ok(!ids.test(quick), where + "quick panel shows an id: " + (ids.exec(quick) || [])[0] + "\n" + quick);
    await click(ev, "#qpscrim");
    await sleep(300);
    // Settings.
    await click(ev, "#gearbtn");
    await sleep(800);
    for (const tab of ["agent", "tools", "diagnostics"]) {
      assert.ok(await click(ev, "#settab-" + tab), where + "tab " + tab);
      await sleep(1500);
      if (tab === "tools") {
        for (const part of ["Skills", "Tools", "Connections"]) {
          await ev(`(() => { const b = [...document.querySelectorAll("#set-tools button")].find((x) => x.textContent.trim() === ${JSON.stringify(part)}); if (b) b.click(); })()`);
          await sleep(1200);
          const t = await text(ev, "#set-tools");
          assert.ok(!ids.test(t), where + "settings " + part + " shows an id: " + (ids.exec(t) || [])[0] + "\n" + t);
        }
      } else {
        const t = await text(ev, "#set-" + tab);
        assert.ok(!ids.test(t), where + "settings " + tab + " shows an id: " + (ids.exec(t) || [])[0] + "\n" + t);
      }
    }
  }
  c.close();
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
