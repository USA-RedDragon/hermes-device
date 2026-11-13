"use strict";
// Shared DevTools helper for the browser-driven run.js scripts: starts
// headless Chrome and talks to one page over the DevTools protocol.
//
// Every round trip has a bounded wait. A call that gets no answer in
// CDP_TIMEOUT_MS (default 30000) throws an error naming the method, the
// run.js line that made the call, the expression, and the tail of Chrome's
// stderr. A renderer crash, a detach, a closed socket or a Chrome exit
// fails the pending and later calls at once with the reason. This reports
// a wedge; it does not retry or skip anything.
const { spawn } = require("child_process");

const TIMEOUT = Number(process.env.CDP_TIMEOUT_MS) || 30000;

// The first stack frame outside this file: the step in run.js that asked.
const callSite = () => {
  for (const l of String(new Error().stack).split("\n").slice(1)) {
    if (!l.includes("cdp.js")) return l.trim().replace(/^at\s+/, "");
  }
  return "unknown step";
};

const brief = (v, n = 100) => {
  const s = typeof v === "string" ? v : JSON.stringify(v);
  const one = String(s).replace(/\s+/g, " ").trim();
  return one.length > n ? one.slice(0, n) + "..." : one;
};

// launch starts Chrome and returns { proc, connect }.
exports.launch = (bin, dir) => {
  const proc = spawn(bin, ["--headless=new", "--no-sandbox", "--disable-gpu", "--remote-debugging-port=0", "--user-data-dir=" + dir, "--hide-scrollbars", "about:blank"], { stdio: ["ignore", "ignore", "pipe"] });
  let stderr = "";
  const tail = () => stderr.slice(-1500).trim() || "(empty)";
  const wsWaiters = [];
  proc.stderr.on("data", (d) => {
    stderr += d;
    const m = /ws:\/\/\S+/.exec(stderr);
    if (m) wsWaiters.splice(0).forEach((w) => w.res(m[0]));
  });
  const exited = new Promise((res) => proc.on("exit", (code, sig) => res(`chrome exited (code ${code}, signal ${sig})`)));
  const failWaiters = (e) => wsWaiters.splice(0).forEach((w) => w.rej(e));
  exited.then((why) => failWaiters(new Error(why + "; stderr: " + tail())));

  // within bounds a promise by ms and names what it was waiting for.
  const within = (p, ms, what) => new Promise((res, rej) => {
    const t = setTimeout(() => rej(new Error(`timed out after ${ms} ms: ${what}; chrome stderr: ${tail()}`)), ms);
    p.then((v) => { clearTimeout(t); res(v); }, (e) => { clearTimeout(t); rej(e); });
  });

  async function connect() {
    const ws = await within(new Promise((res, rej) => {
      const m = /ws:\/\/\S+/.exec(stderr);
      if (m) res(m[0]); else wsWaiters.push({ res, rej });
    }), TIMEOUT, "Chrome to print its DevTools address");
    const t = await within(fetch(`http://127.0.0.1:${new URL(ws).port}/json/new?about:blank`, { method: "PUT" }).then((r) => r.json()), TIMEOUT, "DevTools /json/new (open the page)");
    const sock = new WebSocket(t.webSocketDebuggerUrl);
    await within(new Promise((res, rej) => { sock.onopen = res; sock.onerror = () => rej(new Error("DevTools socket error while connecting")); }), TIMEOUT, "the DevTools socket to open");

    let id = 0;
    let last = "none"; // the latest call made
    let dead = null; // Error once the page can no longer answer
    const pend = new Map();
    const fail = (why) => {
      if (dead) return;
      dead = new Error(`${why}; pending: ${[...pend.values()].map((p) => p.what).join(", ") || "none"}; last call: ${last}; chrome stderr: ${tail()}`);
      for (const p of pend.values()) { clearTimeout(p.timer); p.rej(dead); }
      pend.clear();
    };
    sock.onmessage = (m) => {
      const d = JSON.parse(m.data);
      if (d.id && pend.has(d.id)) {
        const p = pend.get(d.id);
        pend.delete(d.id);
        clearTimeout(p.timer);
        if (d.error) p.rej(new Error(`${p.method} failed: ${JSON.stringify(d.error)} (at ${p.site})`)); else p.res(d.result);
      } else if (d.method === "Inspector.targetCrashed") fail("the page renderer crashed (Inspector.targetCrashed)");
      else if (d.method === "Inspector.detached") fail("DevTools detached from the page: " + brief(d.params));
    };
    sock.onclose = () => fail("the DevTools socket closed");
    sock.onerror = () => fail("the DevTools socket errored");
    exited.then((why) => fail(why));

    const send = (method, params = {}, site = callSite()) => new Promise((res, rej) => {
      if (dead) return rej(dead);
      const i = ++id;
      const what = `${method} (at ${site})`;
      last = what;
      const timer = setTimeout(() => {
        pend.delete(i);
        rej(new Error(`no answer to ${method} within ${TIMEOUT} ms, at ${site}, params ${brief(params)}; chrome stderr: ${tail()}`));
      }, TIMEOUT);
      pend.set(i, { res, rej, timer, what, method, site });
      try { sock.send(JSON.stringify({ id: i, method, params })); } catch (e) { pend.delete(i); clearTimeout(timer); rej(e); }
    });
    const ev = async (expr) => {
      const site = callSite();
      const r = await send("Runtime.evaluate", { expression: expr, awaitPromise: true, returnByValue: true }, site);
      if (r.exceptionDetails) throw new Error(`Runtime.evaluate threw (at ${site}): ${JSON.stringify(r.exceptionDetails)}`);
      return r.result.value;
    };
    await send("Inspector.enable"); // so a renderer crash is reported
    return { send, ev, close: () => { sock.close(); proc.kill(); } };
  }
  return { proc, connect };
};
