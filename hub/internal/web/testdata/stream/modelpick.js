"use strict";
const assert = require("assert");
const { load } = require("./harness.js");

const w = load([process.argv[2]]);
const M = w.win.HubModelPick;

// Hermes asks for a confirmation of a pick that waited for the end of a turn.
const asked = {
  session_id: "s", operation: "model", status: "confirmation_required", confirm: "tok",
  target_model: "claude-opus-5-5[1m]", target_provider: "claude-pool",
  confirm_message: "This chat is large.", payload: { model: "other", provider: "x" },
  committed_model: "claude-sonnet-5-5[1m]",
};
assert.deepStrictEqual(JSON.parse(JSON.stringify(M.ask(asked))),
  { provider: "claude-pool", model: "claude-opus-5-5[1m]", token: "tok", message: "This chat is large." });

// Without the target fields, the stored pick names it.
const bare = { operation: "model", status: "confirmation_required", confirm: "t", payload: { model: "m", provider: "p" } };
assert.deepStrictEqual(JSON.parse(JSON.stringify(M.ask(bare))),
  { provider: "p", model: "m", token: "t", message: "This model needs a confirmation." });

// Other updates are not questions.
assert.strictEqual(M.ask({ operation: "model", model: "m", provider: "p" }), null);
assert.strictEqual(M.ask({ operation: "title", status: "confirmation_required", confirm: "t" }), null);
assert.strictEqual(M.ask({ operation: "model", status: "confirmation_required" }), null);
assert.strictEqual(M.ask({ operation: "model", status: "confirmation_required", confirm: "t" }), null);
assert.strictEqual(M.ask(null), null);

// A dropped pick names the target and what stays.
assert.strictEqual(M.refusal({ operation: "model", status: "refused", target_model: "a", reason: "gone", model: "b", provider: "c" }),
  "Model switch to a was not applied (gone); still using b (c).");
assert.strictEqual(M.refusal({ operation: "model", status: "refused" }),
  "Model switch to the picked model was not applied (refused); still using the current model.");
assert.strictEqual(M.refusal({ operation: "model", model: "m" }), "");
assert.strictEqual(M.refusal({ operation: "reasoning", status: "refused" }), "");
