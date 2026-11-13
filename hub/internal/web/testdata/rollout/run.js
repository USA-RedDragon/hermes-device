"use strict";
// Drives the real hub page in headless Chrome through a Hermes rollout.
// usage: node run.js CHROME HUB_URL PROFILE_DIR POKE_URL (env SHOT_PREFIX
// writes PREFIX-NN.png every second; with ROLLOUT_SOFT=1 it reports every failed
// check and goes on, to record the whole run)
// POKE_URL/silence makes the gateway answer nothing, /other sends the
// hub's traffic to the other replica, which has seen nothing of the
// backend, and /release makes the gateway answer again. For 45 seconds of
// silence the page, open and then loaded fresh on the other replica, keeps
// the chat list, the title, the model line and the messages; no banner
// shows in the first seconds and only plain words after; a message sent
// meanwhile shows at once and its reply comes once the gateway answers.
const assert = require("assert");
const fs = require("fs");
const [bin, url, dir, poke] = process.argv.slice(2);
const shot = process.env.SHOT_PREFIX;
const soft = process.env.ROLLOUT_SOFT === "1";
const failed = [];
const check = (ok, msg) => {
  if (ok) return;
  if (!soft) assert.fail(msg);
  failed.push(msg);
  console.log("FAIL " + msg);
};

const { proc, connect } = require("../cdp.js").launch(bin, dir);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const TITLE = "Rollout chat";
const MODEL = "big";
const SENT = "sent during the switch";
const SILENCE = 45;
const OTHER_AT = 5;
const SEND_AT = 9;
const QUIET = 9;

const state = `(() => {
  const $ = (id) => document.getElementById(id);
  const who = document.querySelector("#infoline .who1");
  const bars = document.querySelector("#log .loading");
  const b = $("banner");
  return {
    title: $("title").textContent,
    model: who ? who.textContent : "",
    chats: document.querySelectorAll("#sessions a.go").length,
    loadingChats: $("sessions").textContent.includes("Loading chats"),
    msgs: document.querySelectorAll("#log .msg").length,
    bars: !!bars && getComputedStyle(bars).visibility !== "hidden",
    banner: b && !b.hidden ? b.textContent.replace(/\\u00d7$/, "") : "",
    sent: Array.from(document.querySelectorAll("#log .msg.user")).some((n) => n.innerText.includes("${SENT}")),
    notSent: /Not sent|not sent|Could not send/.test($("log").innerText),
    reply: $("log").innerText.includes("echo: ${SENT}"),
  };
})()`;

(async () => {
  const c = await connect();
  const { ev, send } = c;
  await send("Page.enable");
  await send("Emulation.setDeviceMetricsOverride", { width: 1280, height: 800, deviceScaleFactor: 1, mobile: false });
  await send("Page.navigate", { url: url + "/auth/login?next=/" });
  await sleep(1500);
  await ev(`location.hash = "#/s/pc/s9"; 1`);
  let s = null;
  for (let i = 0; i < 100; i++) {
    s = await ev(state);
    if (s.title === TITLE && s.model === MODEL && s.msgs >= 4 && s.chats > 0) break;
    await sleep(100);
  }
  assert.ok(s.title === TITLE && s.model === MODEL && s.msgs >= 4 && s.chats > 0, "before the rollout: " + JSON.stringify(s));
  // Let the hub keep its copy of the chat.
  await sleep(500);

  await fetch(poke + "/silence");
  const t0 = Date.now();
  let reloadAt = 0;
  let sentAt = 0;
  const seen = [];
  for (let sec = 0; sec <= SILENCE; sec++) {
    const wait = t0 + sec * 1000 - Date.now();
    if (wait > 0) await sleep(wait);
    if (sec === OTHER_AT) {
      // A fresh load, on the replica that has seen nothing.
      await fetch(poke + "/other");
      await send("Page.reload", { ignoreCache: true });
      reloadAt = Date.now();
      let first = null;
      for (let i = 0; i < 60; i++) {
        await sleep(50);
        try { first = await ev(state); } catch (_) { continue; }
        if (first.title === TITLE && first.model === MODEL && first.msgs >= 4 && first.chats > 0) break;
      }
      console.log(`fresh load during the silence: chat shown after ${Date.now() - reloadAt} ms`);
      check(first && first.title === TITLE && first.model === MODEL && first.msgs >= 4 && first.chats > 0,
        "fresh load during the silence: " + JSON.stringify(first));
    }
    if (sec === SEND_AT) {
      await ev(`(() => { const i = document.getElementById("input"); i.value = "${SENT}"; i.dispatchEvent(new Event("input")); document.getElementById("composer").requestSubmit(); return 1; })()`);
      sentAt = Date.now();
      await sleep(150);
    }
    s = await ev(state);
    s.t = sec;
    seen.push(s);
    if (shot) {
      const r = await send("Page.captureScreenshot", {});
      fs.writeFileSync(`${shot}-${String(sec).padStart(2, "0")}.png`, Buffer.from(r.data, "base64"));
    }
    const where = `at ${sec} s of silence: ${JSON.stringify(s)}`;
    check(s.title === TITLE, "title " + where);
    check(s.model === MODEL, "model line " + where);
    check(s.chats > 0 && !s.loadingChats, "chat list " + where);
    check(s.msgs >= 4 && !s.bars, "messages " + where);
    if (sec < QUIET) check(s.banner === "", "a banner this soon " + where);
    else if (s.banner) check(/^(Cannot reach the agent|Lost the connection to the agent)/.test(s.banner), "banner words " + where);
    if (sentAt) check(s.sent && !s.notSent, "the message sent " + where);
  }
  console.log("banners: " + JSON.stringify([...new Set(seen.map((x) => x.t + ": " + x.banner).filter((x) => !x.endsWith(": ")))].slice(0, 3)));

  await fetch(poke + "/release");
  const back = Date.now();
  for (let i = 0; i < 300; i++) {
    s = await ev(state);
    if (s.reply) break;
    await sleep(100);
  }
  console.log(`after the gateway answered: reply shown after ${Date.now() - back} ms`);
  check(s.reply && s.sent && !s.notSent && s.title === TITLE && s.model === MODEL, "after the rollout: " + JSON.stringify(s));
  if (shot) {
    const r = await send("Page.captureScreenshot", {});
    fs.writeFileSync(`${shot}-after.png`, Buffer.from(r.data, "base64"));
  }
  if (failed.length) throw new Error(failed.length + " checks failed");
  console.log("ok");
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
