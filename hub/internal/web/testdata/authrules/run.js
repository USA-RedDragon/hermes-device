"use strict";
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {}, TextEncoder };
vm.runInNewContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const A = sandbox.window.HubAuth;

// Names: the hub lowercases and trims, then wants 1 to 64 of a-z 0-9 . _ -.
assert.strictEqual(A.cleanName("  Jordan "), "jordan");
assert.strictEqual(A.nameProblem("Jordan"), "");
assert.strictEqual(A.nameProblem("a.b_c-d9"), "");
assert.notStrictEqual(A.nameProblem(""), "");
assert.notStrictEqual(A.nameProblem("-lead"), "");
assert.notStrictEqual(A.nameProblem("has space"), "");
assert.notStrictEqual(A.nameProblem("x".repeat(65)), "");
assert.strictEqual(A.nameProblem("x".repeat(64)), "");

// Passwords: at least 12 characters (not bytes), at most 1024 bytes, not the name.
assert.strictEqual(A.passwordProblem("correct horse battery", "jordan"), "");
assert.strictEqual(A.passwordProblem("a".repeat(12), "jordan"), "");
assert.match(A.passwordProblem("a".repeat(11), "jordan"), /at least 12 characters/);
assert.match(A.passwordProblem("", "jordan"), /at least 12/);
assert.match(A.passwordProblem("Averylongname", "averylongname"), /at least 12/);
assert.strictEqual(A.passwordProblem("é".repeat(12), "jordan"), "", "12 characters of 2 bytes pass");
assert.match(A.passwordProblem("é".repeat(513), "jordan"), /at most 1024 bytes/, "bytes, not characters, bound the top");
assert.strictEqual(A.passwordProblem("é".repeat(512), "jordan"), "");

// The meter follows length: red under 12, amber under 16, then green.
assert.deepEqual(A.strength(""), { pct: 0, level: "bad" });
assert.strictEqual(A.strength("a".repeat(11)).level, "bad");
assert.strictEqual(A.strength("a".repeat(12)).level, "warn");
assert.strictEqual(A.strength("a".repeat(16)).level, "ok");
assert.strictEqual(A.strength("correct horse battery").pct, 70);
assert.strictEqual(A.strength("a".repeat(100)).pct, 100);

// next: only a path on this site.
assert.strictEqual(A.cleanNext("/#/scheduled"), "/#/scheduled");
assert.strictEqual(A.cleanNext("//evil.example"), "/");
assert.strictEqual(A.cleanNext("/\\evil.example"), "/");
assert.strictEqual(A.cleanNext("https://evil.example"), "/");
assert.strictEqual(A.cleanNext(""), "/");
assert.strictEqual(A.cleanNext(null), "/");

assert.strictEqual(A.sentence("Wrong name"), "Wrong name.");
assert.strictEqual(A.sentence("Saved."), "Saved.");

// Guards: nobody demotes, disables or deletes themselves, or the last admin.
const people = [
  { name: "jordan", admin: true, source: "account" },
  { name: "sam", admin: false, source: "account" },
  { name: "riley", admin: true, source: "account" },
];
let g = A.guards("jordan", people[0], people);
assert.match(g.demote, /own admin/);
assert.match(g.disable, /yourself/);
assert.match(g.remove, /own account/);
g = A.guards("jordan", people[1], people);
assert.deepEqual([g.demote, g.disable, g.remove], ["", "", ""]);
g = A.guards("jordan", people[2], people);
assert.deepEqual([g.demote, g.disable, g.remove], ["", "", ""], "another admin may go while one is left");
const alone = [people[0], people[1]];
g = A.guards("sam", alone[0], alone);
assert.match(g.demote, /at least one admin/);
assert.match(g.remove, /at least one admin/);
const disabledOther = [people[0], { ...people[2], disabled: true }];
g = A.guards("riley", disabledOther[0], disabledOther);
assert.match(g.disable, /at least one admin/, "a disabled admin does not count");
const withConfigAdmin = [people[0], { name: "pat", admin: true, source: "config" }];
g = A.guards("pat", withConfigAdmin[0], withConfigAdmin);
assert.strictEqual(g.demote, "", "an admin from the config counts");

// Backends: the config's grants are locked on, an admin adds the rest.
const hermes = { id: "hermes", name: "Hermes", everyone: true, users: [], groups: [] };
const desk = { id: "desktop", name: "Desktop", everyone: false, users: ["sam"], groups: [] };
const lab = { id: "lab", name: "Lab server", everyone: false, users: [], groups: ["family"] };
assert.strictEqual(A.lock(hermes, { name: "riley" }), "everyone (hermes.url)");
assert.strictEqual(A.lock({ ...hermes, id: "other" }, { name: "riley" }), "everyone (config)");
assert.match(A.lock(desk, { name: "sam" }), /config/);
assert.strictEqual(A.lock(desk, { name: "riley" }), "");
assert.strictEqual(A.lock(lab, { name: "riley", groups: ["family"] }), "by group family (config)");
assert.strictEqual(A.lock(lab, { name: "riley", groups: ["other"] }), "");
assert.strictEqual(A.lock(lab, { name: "riley" }), "");
assert.deepEqual(A.backendNames([hermes, desk, lab], { name: "riley", backends: ["lab"] }), ["Hermes", "Lab server"]);
assert.deepEqual(A.backendNames([hermes, desk, lab], { name: "sam", backends: [] }), ["Hermes", "Desktop"]);

// Rows: dot, pills and the meta line.
const fmt = { day: () => "Nov 2", time: () => "3:40 PM" };
const ctx = (mode) => ({ me: "jordan", mode, backends: [hermes, desk, lab], fmt });
let r = A.row({ name: "jordan", display: "Jordan", admin: true, source: "account", backends: ["lab"], created: 1, password: true }, ctx("local"));
assert.strictEqual(r.display, "Jordan (you)");
assert.deepEqual(r.pills, [["admin", "Admin"]]);
assert.strictEqual(r.state, "on");
assert.deepEqual(r.meta, ["Hermes, Lab server", "since Nov 2"]);
r = A.row({ name: "riley", display: "Riley", source: "account", locked: true, failures: 5, locked_until: 1, backends: [], password: true }, ctx("local"));
assert.deepEqual(r.pills, [["warn", "Locked"]]);
assert.strictEqual(r.state, "lock");
assert.ok(r.meta.includes("5 wrong passwords, unlocks at 3:40 PM"));
r = A.row({ name: "riley", display: "Riley", source: "account", locked: true, locked_until: 1, backends: [], password: true }, ctx("local"));
assert.ok(r.meta.includes("too many wrong passwords, unlocks at 3:40 PM"), "the hub resets the count when it locks");
r = A.row({ name: "casey", display: "Casey", source: "account", disabled: true, backends: [], password: true }, ctx("local"));
assert.deepEqual(r.pills, [["", "Disabled"]]);
assert.strictEqual(r.state, "off");
r = A.row({ name: "sam", display: "", source: "config", identity: "proxy:sam@example.com", backends: [] }, ctx("proxy"));
assert.deepEqual(r.pills, [["", "From config"]]);
assert.deepEqual(r.meta, ["proxy: sam@example.com", "users[] in the config; change it there"]);
assert.strictEqual(r.display, "");
r = A.row({ name: "alex", display: "Alex", email: "alex@example.com", source: "account", identity: "", backends: [] }, ctx("proxy"));
assert.deepEqual(r.pills, [["warn", "Not logged in yet"]]);
assert.strictEqual(r.waiting, true);
assert.strictEqual(r.meta[0], "waits for alex@example.com");
r = A.row({ name: "jo", display: "Jo", email: "jo@example.com", source: "account", identity: "proxy:jo@example.com", backends: [] }, ctx("proxy"));
assert.strictEqual(r.waiting, false);
assert.strictEqual(r.meta[0], "proxy: jo@example.com");

// Banners say how people get in.
const text = (b) => b.parts.map((p) => (typeof p === "string" ? p : p.code)).join("");
assert.strictEqual(A.banner("local", {}), null);
assert.strictEqual(text(A.banner("proxy", { header: "Remote-User", trusted: ["10.0.0.0/8"], auto_create: true })),
  "People log in through the proxy in front of this hub (Remote-User from 10.0.0.0/8). New people get an account at their first login (auto_create).");
assert.match(text(A.banner("proxy", { jwt: true })), /signed token/);
assert.match(text(A.banner("proxy", { header: "X" })), /Only people in the config or added here/);
const oidc = A.banner("oidc", { issuer: "auth.example.com", auto_create: true, domains: ["example.com"] });
assert.strictEqual(oidc.kind, "warn");
assert.strictEqual(text(oidc), "People log in with your identity provider (auth.example.com). New people with a verified email at example.com get an account.");
console.log("ok");
