"use strict";
// Runs agent.js's pure helpers: node run.js agent.js
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {}, Object, Math, Date, Number, String, Array, Promise };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const A = sandbox.window.HubAgent;
const eq = (a, b, msg) => assert.deepStrictEqual(JSON.parse(JSON.stringify(a)), b, msg);

// The health check's report (the agent's doctor output).
eq(A.doctorSummary(["Checking...", "✓ config ok", "✓ model reachable", "⚠ data folder 91% full", "✗ honcho unreachable"]),
  { passed: 2, look: ["data folder 91% full", "honcho unreachable"], optional: 0 });
eq(A.doctorSummary([]), { passed: 0, look: [], optional: 0 });
// Providers and tools nobody set up are not problems: they are counted, not listed.
eq(A.doctorSummary([
  "✓ Python 3.14.9", "⚠ Nous Portal auth (not logged in)", "⚠ OpenAI Codex auth (not logged in)", "→ No Codex credentials stored.",
  "⚠ MiniMax OAuth (not logged in)", "⚠ xAI OAuth (not logged in)", "⚠ OpenRouter API (not configured)", "⚠ discord (missing DISCORD_BOT_TOKEN)",
  "⚠ vision (system dependency not met)", "⚠ image_gen (image generation unavailable — check the provider selection)",
  "⚠ Skills Hub directory not initialized (run: hermes skills list)", "⚠ No GITHUB_TOKEN (60 req/hr rate limit)", "✗ /opt/data/.env file missing",
  "⚠ config.yaml not found (using defaults)", "⚠ No API key found in ~/./.env",
]), { passed: 1, look: [], optional: 13 });
// A real failure or warning stays, and says nothing of the agent's maker or its commands.
eq(A.doctorSummary(["⚠ Nous Portal auth (not logged in)", "✗ /opt/data/state.db is corrupt (run: hermes doctor --fix)", "⚠ Hermes gateway is slow"]),
  { passed: 0, look: ["/opt/data/state.db is corrupt", "the agent gateway is slow"], optional: 1 });
assert.strictEqual(A.optionalText(1), "1 optional item is not set up. That is fine unless you want them.");
assert.ok(A.optionalText(12).startsWith("12 optional items are not set up"));
const report = A.reportText(["", "┌────┐", "│ 🩺 Hermes Doctor │", "└────┘", "", "◆ Python", "  ✓ Python 3.14.9", "", "◆ Auth Providers",
  "  ⚠ Nous Portal auth (not logged in)", "  ⚠ OpenAI Codex auth (not logged in)", "    → No Codex credentials stored. Run `hermes auth add`", "◆ Files",
  "  ✗ /opt/data/state.db is corrupt", "    → Run 'hermes doctor --fix' to repair", "  ⚠ config.yaml not found (using defaults)", "", "──────────────────",
  "  Found 2 issue(s) to address:", "  1. Run 'hermes setup'"]);
assert.strictEqual(report, "◆ Python\n  ✓ Python 3.14.9\n\n◆ Files\n  ✗ /opt/data/state.db is corrupt");
assert.ok(!/hermes|nous/i.test(report), "no commands or product names");

// Log line colors.
assert.strictEqual(A.logTone("2026-11-13 10:44:30 WARNING provider slow"), "w");
assert.strictEqual(A.logTone("2026-11-13 10:44:30 ERROR boom"), "e");
assert.strictEqual(A.logTone("2026-11-13 10:44:30 INFO fine"), "");

// The banner: admins only.
const st = { current: "0.26.70", latest: { version: "0.27.0" } };
assert.strictEqual(A.bannerText({ admin: true }, st), "Talaria 0.27.0 is out. This server runs 0.26.70.");
assert.strictEqual(A.bannerText({ admin: false }, st), "", "members never see it");
assert.strictEqual(A.bannerText({ admin: true }, { current: "0.27.0" }), "", "nothing behind");
assert.strictEqual(A.bannerText({ admin: true }, { current: "0.27.0", computers: [{ name: "laptop", version: "0.25.2", latest: "0.27.3" }] }),
  "laptop runs talaria 0.25.2, and 0.27.3 is out.");
assert.ok(A.bannerText({ admin: true }, { current: "0.27.0", agent: [{ backend: "Talaria", running: "0.23.1", latest: "0.24.0-p1" }] }).startsWith("A new agent release is out (0.24.0-p1)"));

// Update steps per install type.
assert.ok(A.updateSteps("compose")[1].includes("docker compose up -d --wait"));
assert.ok(A.updateSteps("helm")[1].startsWith("helm upgrade"));
assert.ok(A.updateSteps("unraid")[0].includes("Apply Update"));
assert.strictEqual(A.updateSteps("other")[1], "");

// When a change reaches chats.
assert.strictEqual(A.liveLine("Web search", false, true), "Web search is off. Open chats use it from their next message.");
assert.strictEqual(A.liveLine("Web search", true, false), "Web search is on. New chats use it.");

// Time zones: the browser's first, UTC kept.
eq(A.zones("America/Chicago", ["Europe/Berlin", "America/Chicago"]), ["America/Chicago", "UTC", "Europe/Berlin"]);
eq(A.zones("", []), ["UTC"]);

// Skills: on first, filter by name or description.
eq(A.splitSkills([{ name: "b", enabled: false }, { name: "a", enabled: false }, { name: "z", enabled: true, description: "pdf files" }]).map((s) => s.name), ["z", "a", "b"]);
eq(A.splitSkills([{ name: "b", enabled: false }, { name: "z", enabled: true, description: "pdf files" }], "PDF").map((s) => s.name), ["z"]);

assert.strictEqual(A.modeLabel("smart", true), "Ask when risky");
assert.strictEqual(A.modeLabel("manual"), "Ask every time");
console.log("ok");

// Ids in a line to look at show as words.
eq(A.doctorSummary(["⚠ image_gen provider failed"]).look, ["image gen provider failed"]);
