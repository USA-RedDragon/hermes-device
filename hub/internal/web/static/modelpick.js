"use strict";
// HubModelPick reads what Hermes says about a model pick that waited for
// the end of a turn. At that point Hermes may ask for a confirmation (the
// model costs more, or the chat is large) or drop the pick; the page shows
// either, so the pick never waits for an answer nobody was asked for.
window.HubModelPick = (() => {
  const isModel = (p) => !!p && p.operation === "model";

  // ask returns the confirmation Hermes wants for a deferred pick, or null.
  function ask(p) {
    if (!isModel(p) || p.status !== "confirmation_required" || typeof p.confirm !== "string" || !p.confirm) return null;
    const pick = p.payload || {};
    const model = p.target_model || pick.model || "";
    if (!model) return null;
    return {
      provider: p.target_provider || pick.provider || "",
      model,
      token: p.confirm,
      message: p.confirm_message || "This model needs a confirmation.",
    };
  }

  // refusal returns the text for a deferred pick that Hermes dropped, or "".
  function refusal(p) {
    if (!isModel(p) || p.status !== "refused") return "";
    const now = p.model ? p.model + (p.provider ? " (" + (window.HubPlain ? window.HubPlain.text(p.provider) : p.provider) + ")" : "") : "the current model";
    return "Model switch to " + (p.target_model || "the picked model") + " was not applied (" + (window.HubPlain ? window.HubPlain.clean(p.reason) : "") || "refused" + "); still using " + now + ".";
  }

  return { ask, refusal };
})();
