"use strict";
// Shared WebDriver BiDi helper for the browser-driven run.js scripts: starts
// headless Firefox and drives one page with real input events. Every round
// trip has a bounded wait; a Firefox exit or a closed socket fails the
// pending calls at once with the reason and the tail of Firefox's stderr.
const { spawn } = require("child_process");

const TIMEOUT = Number(process.env.CDP_TIMEOUT_MS) || 30000;

exports.launch = (bin, dir) => {
  const proc = spawn(bin, ["--headless", "--no-remote", "--profile", dir, "--remote-debugging-port=0", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] });
  let stderr = "";
  const tail = () => stderr.slice(-1500).trim() || "(empty)";
  const exited = new Promise((res) => proc.on("exit", (code, sig) => res(`firefox exited (code ${code}, signal ${sig})`)));

  async function connect() {
    const ws = await new Promise((res, rej) => {
      const t = setTimeout(() => rej(new Error("Firefox did not print its BiDi address; stderr: " + tail())), TIMEOUT);
      proc.stderr.on("data", (d) => {
        stderr += d;
        const m = /WebDriver BiDi listening on (ws:\/\/\S+)/.exec(stderr);
        if (m) { clearTimeout(t); res(m[1]); }
      });
      exited.then((why) => { clearTimeout(t); rej(new Error(why + "; stderr: " + tail())); });
    });
    const sock = new WebSocket(ws + "/session");
    await new Promise((res, rej) => { sock.onopen = res; sock.onerror = () => rej(new Error("BiDi socket error while connecting")); });
    let id = 0;
    let dead = null;
    const pend = new Map();
    const fail = (why) => {
      if (dead) return;
      dead = new Error(`${why}; firefox stderr: ${tail()}`);
      for (const p of pend.values()) { clearTimeout(p.timer); p.rej(dead); }
      pend.clear();
    };
    sock.onmessage = (m) => {
      const d = JSON.parse(m.data);
      if (!d.id || !pend.has(d.id)) return;
      const p = pend.get(d.id);
      pend.delete(d.id);
      clearTimeout(p.timer);
      if (d.type === "error") p.rej(new Error(`${p.method} failed: ${d.error}: ${d.message}`)); else p.res(d.result);
    };
    sock.onclose = () => fail("the BiDi socket closed");
    exited.then(fail);
    const send = (method, params = {}) => new Promise((res, rej) => {
      if (dead) return rej(dead);
      const i = ++id;
      const timer = setTimeout(() => { pend.delete(i); rej(new Error(`no answer to ${method} within ${TIMEOUT} ms; firefox stderr: ${tail()}`)); }, TIMEOUT);
      pend.set(i, { res, rej, timer, method });
      sock.send(JSON.stringify({ id: i, method, params }));
    });
    await send("session.new", { capabilities: {} });
    const context = (await send("browsingContext.getTree", {})).contexts[0].context;
    // Key presses reach the page only in the active window of the browser.
    await send("browsingContext.activate", { context });
    const ev = async (expr) => {
      const r = await send("script.evaluate", { expression: `(async () => JSON.stringify(await (${expr})))()`, target: { context }, awaitPromise: true });
      if (r.type === "exception") throw new Error("script threw: " + r.exceptionDetails.text + " in " + expr.slice(0, 100));
      return r.result.value === undefined ? undefined : JSON.parse(r.result.value);
    };
    return {
      ev,
      send,
      context,
      goto: (url) => send("browsingContext.navigate", { context, url, wait: "complete" }),
      click: (x, y) => send("input.performActions", { context, actions: [{ type: "pointer", id: "m", parameters: { pointerType: "mouse" }, actions: [{ type: "pointerMove", x: Math.round(x), y: Math.round(y) }, { type: "pointerDown", button: 0 }, { type: "pointerUp", button: 0 }] }] }),
      enter: () => send("input.performActions", { context, actions: [{ type: "key", id: "k", actions: [{ type: "keyDown", value: "\uE006" }, { type: "keyUp", value: "\uE006" }] }] }),
      close: () => { try { sock.close(); } catch (_) { /* already closed */ } proc.kill(); },
    };
  }
  return { proc, connect };
};
