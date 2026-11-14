"use strict";
// Drives the real hub page in headless Chrome over the DevTools protocol.
// usage: node run.js CHROME HUB_URL PROFILE_DIR DEMO_PROJECT_ID
// Project skills in two places, at 412 and 1280 px, light and dark:
//  - Memory > Notes > a project: a Skills tab beside Notes, with each skill's name, a project pill, its
//    description, and View and Delete on the name's line; View reads the SKILL.md in place; Delete asks first
//    and removes only the skill chosen.
//  - Settings > Skills and tools: Global first, then one group per project, each with its count; the filter
//    narrows every group; turning a project skill off goes through the agent's switch.
// Nothing overflows sideways. PROJSKILLS_SHOT=dir writes project-skills-build-*.png.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, demo] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);
const { until } = require("../cdp.js");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const q = (sel) => `document.querySelector(${JSON.stringify(sel)})`;
const LONG = "demo-app-people-and-owners-of-every-part-of-the-code-base";

async function open(c, w, dark, tag) {
  const { ev, send } = c;
  await send("Emulation.setDeviceMetricsOverride", { width: w, height: 900, deviceScaleFactor: 1, mobile: w < 600 });
  await send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }] });
  await ev(`window.__old = true`);
  await send("Page.navigate", { url: url + "/auth/login?next=/" });
  await until(ev, `!window.__old && ${q("#gearbtn")} && document.getElementById("sessions") && document.getElementById("sessions").children.length > 0`, tag + ": the hub page");
}

async function shot(c, name) {
  if (!process.env.PROJSKILLS_SHOT) return;
  await sleep(250);
  fs.writeFileSync(`${process.env.PROJSKILLS_SHOT}/project-skills-build-${name}.png`, Buffer.from((await c.send("Page.captureScreenshot", { format: "png" })).data, "base64"));
}

const noSideways = async (ev, tag) => assert.ok(await ev(`document.documentElement.scrollWidth <= innerWidth && document.body.scrollWidth <= innerWidth`), tag + ": nothing overflows sideways");

async function memoryProject(c, tag) {
  const { ev } = c;
  await ev(`location.hash = "#/memory/pc/notes"`);
  await until(ev, `${q('#memory select[data-k="project"]')} && ${q('#memory select[data-k="project"]')}.options.length > 2`, tag + ": the project picker");
  await ev(`(() => { const s = ${q('#memory select[data-k="project"]')}; s.value = ${JSON.stringify(demo)}; s.dispatchEvent(new Event("change")); })()`);
  await until(ev, `${q('#memory [data-k="sub-skills"]')}`, tag + ": Notes and Skills tabs");
  const labels = await ev(`[...document.querySelectorAll("#memory .mem-subtab")].map((b) => b.textContent.replace(/\\s+/g, " ").trim())`);
  assert.deepStrictEqual(labels, ["Notes (3)", "Skills (3)"], tag + ": tab labels from the real counts");
  await ev(`${q('#memory [data-k="sub-skills"]')}.click()`);
  await until(ev, `document.querySelectorAll("#memory .mem-skill").length === 3`, tag + ": three skills");
  assert.ok((await ev(`location.hash`)).endsWith("/" + demo + "/skills"), tag + ": the address keeps the Skills tab");
}

async function memoryChecks(c, w, tag) {
  const { ev } = c;
  const rows = await ev(`[...document.querySelectorAll("#memory .mem-skill")].map((r) => {
    const name = r.querySelector(".mem-skill-name").getBoundingClientRect(), acts = r.querySelector(".mem-skill-acts").getBoundingClientRect(), pill = r.querySelector(".mem-pill.proj");
    return { name: r.querySelector(".mem-skill-name").textContent, pill: pill && pill.textContent, desc: r.querySelector(".mem-skill-desc").textContent,
      btns: [...r.querySelectorAll(".mem-skill-acts button")].map((b) => b.textContent), onLine: acts.top < name.bottom - 1 && acts.bottom > name.top + 1, right: acts.right, off: !!r.querySelector(".mem-pill:not(.proj)") };
  })`);
  assert.deepStrictEqual(rows.map((r) => r.name), ["demo-app-layout", "demo-app-release", LONG], tag + ": names");
  for (const r of rows) {
    assert.strictEqual(r.pill, "project", tag + ": project pill on " + r.name);
    assert.ok(r.desc.startsWith("Use when"), tag + ": description of " + r.name);
    assert.deepStrictEqual(r.btns, ["View", "Delete"], tag + ": buttons of " + r.name);
    assert.ok(r.onLine, tag + ": View and Delete sit on the name's line for " + r.name);
    assert.ok(r.right <= w, tag + ": buttons inside the window for " + r.name);
  }
  assert.deepStrictEqual(rows.map((r) => r.off), [false, false, true], tag + ": the skill that is switched off says so");
  await noSideways(ev, tag);
}

async function settingsOpen(c, tag) {
  const { ev } = c;
  await until(ev, `(() => { if (!document.getElementById("settings").open) ${q("#gearbtn")}.click(); return document.getElementById("settings").open; })()`, tag + ": Settings to open");
  await until(ev, q("#settab-tools"), tag + ": Settings tabs");
  await ev(`${q("#settab-tools")}.click()`);
  await until(ev, `document.querySelectorAll("#set-tools .skgroup").length >= 3`, tag + ": the skill groups");
}

async function settingsChecks(c, tag) {
  const { ev } = c;
  const gs = await ev(`[...document.querySelectorAll("#set-tools .skgroup")].map((g) => ({ title: g.querySelector("h3").textContent, count: g.querySelector(".skpill").textContent,
    hint: g.querySelector(".hint").textContent, rows: [...g.querySelectorAll(".lst > div")].map((r) => r.querySelector("b").textContent), switches: g.querySelectorAll("input[role=switch]").length }))`);
  assert.deepStrictEqual(gs.map((g) => [g.title, g.count]), [["Global", "2"], ["Project: demo-app", "3"], ["Project: other-tool", "1"]], tag + ": groups and counts");
  assert.deepStrictEqual(gs[0].rows.sort(), ["GitHub", "Weather brief"].sort(), tag + ": global skills");
  assert.deepStrictEqual(gs[1].rows.sort(), ["demo-app-layout", "demo-app-release", LONG].sort(), tag + ": demo-app skills");
  assert.deepStrictEqual(gs[2].rows, ["other-tool-ci"], tag + ": other-tool skills");
  assert.ok(gs[1].hint.includes("github.com/alice/demo-app"), tag + ": says which project");
  assert.deepStrictEqual(gs.map((g) => g.switches), [2, 3, 1], tag + ": every row has its switch");
  const text = await ev(`document.getElementById("set-tools").innerText`);
  assert.ok(!/Move to project|looks project-specific|Review candidates/.test(text), tag + ": nothing that is not built");
  await noSideways(ev, tag);
}

(async () => {
  const c = await connect();
  const { ev, send } = c;
  try {
    await send("Page.enable");
    for (const [w, mobile] of [[412, true], [1280, false]]) {
      for (const dark of [false, true]) {
        const tag = `${w}-${dark ? "dark" : "light"}`;
        await open(c, w, dark, tag);
        await memoryProject(c, tag);
        await memoryChecks(c, w, tag);
        await shot(c, `memory-${dark ? "dark" : "light"}-${w}`);
        // View reads the SKILL.md in place and hides again.
        await ev(`document.querySelector("#memory .mem-skill .mem-skill-acts button").click()`);
        await until(ev, `${q("#memory .mem-skill-text")} && /Step 1: run make/.test(${q("#memory .mem-skill-text")}.textContent)`, tag + ": the skill's text");
        assert.ok(await ev(`${q("#memory .mem-skill .mem-skill-acts button")}.textContent === "Hide"`), tag + ": View turns into Hide");
        await noSideways(ev, tag + " viewing");
        await shot(c, `memory-view-${dark ? "dark" : "light"}-${w}`);
        await ev(`document.querySelector("#memory .mem-skill .mem-skill-acts button").click()`);
        await until(ev, `!${q("#memory .mem-skill-text")}`, tag + ": Hide");
        // Settings, in the same page.
        await settingsOpen(c, tag);
        await settingsChecks(c, tag);
        await ev(`${q("#set-tools .skgroup")}.scrollIntoView({ block: "start" })`);
        await shot(c, `settings-${dark ? "dark" : "light"}-${w}`);
      }
    }

    // Delete asks first, Cancel changes nothing, Delete removes only that skill.
    const tag = "delete";
    await open(c, 412, false, tag);
    await memoryProject(c, tag);
    await ev(`window.__calls = []; const f = window.fetch; window.fetch = function (u, o) { if (o && o.method && o.method !== "GET") window.__calls.push(o.method + " " + u); return f.apply(this, arguments); }`);
    const del = `[...document.querySelectorAll("#memory .mem-skill")].find((r) => r.dataset.skill === "demo-app-release")`;
    await ev(`${del}.querySelector(".mem-skill-acts button.danger-text").click()`);
    await until(ev, `/Delete this skill\\?/.test(${del}.textContent)`, tag + ": asks first");
    assert.strictEqual(await ev(`window.__calls.length`), 0, tag + ": nothing sent yet");
    assert.ok(await ev(`${del}.querySelector(".mem-skill-name").getBoundingClientRect().height < 30`), tag + ": the name stays on one line while asking");
    await shot(c, "memory-confirm-light-412");
    await ev(`[...${del}.querySelectorAll("button")].find((b) => b.textContent === "Cancel").click()`);
    await until(ev, `!/Delete this skill\\?/.test(${del}.textContent)`, tag + ": Cancel");
    assert.strictEqual(await ev(`window.__calls.length`), 0, tag + ": Cancel sends nothing");
    await ev(`${del}.querySelector(".mem-skill-acts button.danger-text").click()`);
    await until(ev, `/Delete this skill\\?/.test(${del}.textContent)`, tag + ": asks again");
    await ev(`[...${del}.querySelectorAll("button")].find((b) => b.textContent === "Delete").click()`);
    await until(ev, `document.querySelectorAll("#memory .mem-skill").length === 2`, tag + ": the skill is gone");
    assert.deepStrictEqual(await ev(`window.__calls.map((x) => x.replace(location.origin, ""))`), [`DELETE /api/memory/pc/projects/${demo}/skills/demo-app-release`], tag + ": one delete");
    assert.deepStrictEqual(await ev(`[...document.querySelectorAll("#memory .mem-skill-name")].map((n) => n.textContent)`), ["demo-app-layout", LONG], tag + ": the others stay");
    assert.ok(await ev(`/Skills \\(2\\)/.test(document.querySelector("#memory .mem-subtab[aria-selected=true]").textContent)`), tag + ": the count follows");
    assert.ok(await ev(`/demo-app-release is deleted/.test(document.getElementById("memory").innerText)`), tag + ": says what happened");
    await noSideways(ev, tag);
    await shot(c, "memory-deleted-light-412");

    // Settings: the filter narrows every group; a project skill's switch goes through the agent.
    await settingsOpen(c, tag);
    const sets = (sel) => `[...document.querySelectorAll("#set-tools ${sel}")]`;
    assert.deepStrictEqual(await ev(`${sets(".skgroup h3")}.map((h) => h.textContent)`), ["Global", "Project: demo-app", "Project: other-tool"], tag + ": groups");
    assert.strictEqual(await ev(`${sets(".skgroup[data-project] .skpill")}[0].textContent`), "2", tag + ": the count follows the deleted skill");
    await ev(`(() => { const i = ${q("#set-tools input[type=search]")}; i.value = "layout"; i.dispatchEvent(new Event("input")); })()`);
    assert.deepStrictEqual(await ev(`${sets(".skgroup h3")}.map((h) => h.textContent)`), ["Global", "Project: demo-app"], tag + ": the filter hides groups with no match");
    assert.strictEqual(await ev(`${sets(".skgroup[data-project] .skpill")}[0].textContent`), "1 of 2", tag + ": the filter shows its share");
    await ev(`(() => { const i = ${q("#set-tools input[type=search]")}; i.value = ""; i.dispatchEvent(new Event("input")); })()`);
    await until(ev, `${sets(".skgroup")}.length === 3`, tag + ": groups back");
    await ev(`${q('#set-tools .skgroup[data-project="github.com-alice-other-tool"] input[role=switch]')}.click()`);
    await until(ev, `/other-tool-ci is off/.test(document.getElementById("set-tools").innerText)`, tag + ": the switch's answer");
    assert.strictEqual(await ev(`${q('#set-tools .skgroup[data-project="github.com-alice-other-tool"] input[role=switch]')}.checked`), false, tag + ": the switch is off");
  } finally {
    c.close();
    proc.kill();
  }
})().catch((e) => { console.error(e.stack || e); process.exit(1); });
