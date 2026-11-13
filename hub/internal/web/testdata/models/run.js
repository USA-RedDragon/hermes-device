"use strict";
// Runs models.js's pure helpers and the Connect a model dialog's flow:
// node run.js models.js
const fs = require("fs");
const vm = require("vm");
const assert = require("assert");

const sandbox = { window: {}, URL, Object, Math, Date, Number, String, Array, Promise };
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), sandbox);
const M = sandbox.window.HubModels;
const eq = (a, b, msg) => assert.deepStrictEqual(JSON.parse(JSON.stringify(a)), b, msg);

// The localhost hint: only for addresses that name the Hermes container.
for (const a of ["http://localhost:11434/v1", "http://127.0.0.1:1234", "http://[::1]:8000/v1", "http://0.0.0.0:11434"]) assert.ok(M.isLocal(a), a);
for (const a of ["http://192.168.1.10:11434/v1", "http://ollama:11434/v1", "not a url", ""]) assert.ok(!M.isLocal(a), a);

// Address tests, with Hermes's own answers (v0.23.1, read live).
let v = M.serverVerdict({ ok: true, reachable: true, message: "", models: ["qwen2.5:0.5b"], resolved_base_url: "http://ollama:11434/v1" }, "http://ollama:11434");
eq([v.tone, v.title, v.at, v.models], ["good", "Found 1 model", "http://ollama:11434/v1", ["qwen2.5:0.5b"]]);
v = M.serverVerdict({ ok: false, reachable: false, message: "Could not reach http://localhost:11434/v1/models.", models: [] }, "http://localhost:11434/v1");
assert.strictEqual(v.tone, "bad");
assert.strictEqual(v.title, "Could not reach http://localhost:11434/v1/models.", "Hermes's words stay whole");
assert.ok(v.hint.startsWith("Hermes runs in its own container, so localhost is that container"), "Talaria's localhost note");
v = M.serverVerdict({ ok: false, reachable: false, message: "Could not reach http://192.168.1.9:11434/v1/models.", models: [] }, "http://192.168.1.9:11434/v1");
assert.strictEqual(v.hint, "", "no localhost note for a LAN address");
v = M.serverVerdict({ ok: false, reachable: true, message: "Connected to http://192.168.1.10:11434/v1/models, but it advertised no models. Start a model on that endpoint and try again.",
  models: [] }, "http://192.168.1.10:11434/v1");
eq([v.tone, v.noModels, v.title, v.text], ["warn", true, "Connected to http://192.168.1.10:11434/v1/models, but it advertised no models.", "Start a model on that endpoint and try again."]);
v = M.serverVerdict({ ok: false, reachable: true, message: "http://ollama:11434/v1/models answered, but POST /chat/completions returned HTTP 404: this host does not serve the Chat Completions API. Pick the API mode it does serve.",
  models: ["x"] }, "http://ollama:11434/v1");
eq([v.tone, v.needMode, v.noModels], ["bad", true, false]);
v = M.serverVerdict({ ok: false, reachable: true, message: "The endpoint rejected the API key.", models: [] }, "http://h/v1");
eq([v.tone, v.noModels, v.title], ["bad", false, "The endpoint rejected the API key."]);
v = M.serverVerdict({ ok: false, reachable: true, message: "Endpoint returned HTTP 302.", models: [] }, "http://h/v1");
eq([v.tone, v.noModels], ["bad", false]);

// Key checks.
eq(M.keyVerdict({ ok: true, reachable: true, message: "" }, "OpenRouter"), { kind: "accepted", save: true, tone: "good", title: "OpenRouter accepted the key." });
v = M.keyVerdict({ ok: false, reachable: true, message: "That API key was rejected. Double-check it and try again." }, "OpenRouter");
eq([v.kind, v.save, v.title, v.lead, v.text], ["rejected", false, "That API key was rejected.", "Double-check it and try again.", "Nothing was saved."]);
v = M.keyVerdict({ ok: true, reachable: false, message: "" }, "DeepSeek");
eq([v.kind, v.save, v.title], ["unchecked", true, "DeepSeek key saved, but not checked."]);
v = M.keyVerdict({ ok: false, reachable: false, message: "Could not reach the provider to verify the key." }, "OpenAI API");
eq([v.kind, v.save, v.title], ["offline", true, "Could not reach the provider to verify the key."]);

// Sign-in polls.
eq(M.signInState({ status: "pending" }), { kind: "pending" });
eq(M.signInState({ status: "approved" }), { kind: "approved" });
eq(M.signInState({ status: "expired" }), { kind: "expired" });
eq(M.signInState({ status: "error", error_message: "OpenAI rejected the device-code login request." }), { kind: "failed", message: "OpenAI rejected the device-code login request." });
eq(M.signInState({ status: "denied" }), { kind: "failed", message: "The sign-in was turned down." });
assert.strictEqual(M.minutesLeft(15 * 60000, 61000), 14);
assert.strictEqual(M.minutesLeft(0, 5), 0);

// The window check after a server is saved.
assert.strictEqual(M.windowWarning({ model: "a", effective_context_length: 131072 }), null);
assert.strictEqual(M.windowWarning({ model: "a", effective_context_length: 0 }), null);
eq(M.windowWarning({ model: "qwen3:8b", effective_context_length: 40960 }),
  { title: "qwen3:8b has a window of 40,960 tokens.", text: "Hermes needs at least 64,000, so chats with it will fail or lose context early." });

// Provider search.
const keys = [{ provider: "openrouter", name: "OpenRouter" }, { provider: "deepseek", name: "DeepSeek" }, { provider: "gemini", name: "Google AI Studio" }];
eq(M.findProviders(keys, "goo").map((k) => k.provider), ["gemini"]);
eq(M.findProviders(keys, "").length, 3);
eq(M.findProviders(keys, "deeps").map((k) => k.provider), ["deepseek"]);

// The dialog's flow.
let st = { step: "kind" };
st = M.next(st, { type: "kind", kind: "server" });
eq(st, { step: "server" });
st = M.next(st, { type: "tested", verdict: { tone: "good", models: ["a", "b"] } });
eq([st.step, st.model, st.models], ["server", "a", ["a", "b"]], "the first model is picked");
assert.strictEqual(M.next(st, { type: "saved-server", warning: null }).step, "done");
assert.strictEqual(M.next(st, { type: "saved-server", warning: { title: "w" } }).step, "window");
assert.strictEqual(M.next(st, { type: "back" }).step, "kind");

st = M.next({ step: "kind" }, { type: "kind", kind: "key" });
const rejected = M.keyVerdict({ ok: false, reachable: true, message: "That API key was rejected. Double-check it and try again." }, "OpenRouter");
st = M.next(st, { type: "key-checked", verdict: rejected });
eq([st.step, st.verdict.kind], ["key", "rejected"], "a rejected key stays on the form");
st = M.next(st, { type: "key-checked", verdict: M.keyVerdict({ ok: true, reachable: false }, "DeepSeek"), provider: "deepseek", name: "DeepSeek" });
eq([st.step, st.provider, st.verdict.kind], ["pick", "deepseek", "unchecked"], "a key Hermes cannot check goes on to the pick, with the note");
st = M.next(st, { type: "confirm", message: "costly", model: "big" });
eq([st.step, st.confirm, st.model], ["confirm", "costly", "big"]);
st = M.next(st, { type: "default-set", model: "big" });
eq([st.step, st.model, st.skipped], ["done", "big", false]);
assert.ok(M.next({ step: "pick" }, { type: "default-set", model: "", skipped: true }).skipped);

st = M.next({ step: "kind" }, { type: "kind", kind: "signin" });
st = M.next(st, { type: "code", code: { user_code: "WXYZ-1234" } });
eq([st.signin, st.code.user_code], ["pending", "WXYZ-1234"]);
const still = M.next(st, { type: "polled", state: { kind: "pending" } });
eq([still.step, still.signin], ["signin", "pending"], "a pending poll changes nothing");
eq(M.next(st, { type: "polled", state: { kind: "expired" } }).signin, "expired");
const failed = M.next(st, { type: "polled", state: { kind: "failed", message: "OpenAI rejected the device-code login request." } });
eq([failed.signin, failed.message], ["failed", "OpenAI rejected the device-code login request."]);
const ok = M.next(st, { type: "polled", state: { kind: "approved" }, provider: "nous", name: "Nous Portal" });
eq([ok.step, ok.provider, ok.verdict.title], ["pick", "nous", "Signed in to Nous Portal."]);

// A default the deployment sets: no pick after a key or a sign-in, and a
// default Hermes kept is reported where it was picked, never as done.
eq(M.next(st, { type: "polled", state: { kind: "approved" }, provider: "nous", name: "Nous Portal", pinned: true }).step, "done");
st = M.next({ step: "key" }, { type: "key-checked", verdict: M.keyVerdict({ ok: true, reachable: true }, "OpenRouter"), provider: "openrouter", name: "OpenRouter", pinned: true });
eq([st.step, st.provider], ["done", "openrouter"]);
st = M.next({ step: "pick", provider: "deepseek", model: "deepseek-chat" }, { type: "default-kept", message: "Hermes kept claude-sonnet-5-5 as the model new chats start with." });
eq([st.step, st.kept], ["pick", "Hermes kept claude-sonnet-5-5 as the model new chats start with."]);

console.log("models.js: ok");
