"use strict";
// HubApprovals keeps the cards of the approvals and questions a chat waits
// on. Every pending prompt gets a card of its own with its own buttons, and
// a card says only what the page knows: "Sent" once the hub took the answer
// to Hermes, a settled state when Hermes says the prompt is over, and "Not
// answered" when Hermes dropped it (a restart) with no answer to it.
window.HubApprovals = (() => {
  // create makes the cards of one chat. deps:
  //   el(tag, cls, text), api(method, path, body), base (the chat's API path),
  //   words(key) for the fixed texts, banner(text, bad), add(node) to put a
  //   card in the chat, setTimeout/clearTimeout, ackMs (how long an answer
  //   may wait for Hermes to confirm it before the card offers a retry).
  function create(deps) {
    const { el, api, base, words, banner, add } = deps;
    const setT = deps.setTimeout || setTimeout;
    const clearT = deps.clearTimeout || clearTimeout;
    const ackMs = deps.ackMs || 15000;
    const cards = new Map();

    const done = (card) => card.state === "settled";

    function finish(id, text) {
      const card = cards.get(id);
      if (!card) return;
      if (card.timer) clearT(card.timer);
      card.timer = null;
      card.state = "settled";
      for (const extra of card.box.querySelectorAll(".picks, .typed, .sendnote")) extra.remove();
      card.actions.replaceChildren(el("span", "status", text));
    }

    // note shows a line under the buttons, such as why an answer was not
    // taken. An empty text removes it.
    function note(card, text, bad) {
      if (card.note) card.note.remove();
      card.note = null;
      if (!text) return;
      card.note = el("div", bad ? "sendnote bad" : "sendnote", text);
      card.box.append(card.note);
    }

    function offer(p) {
      const id = p && p.prompt_id;
      if (!id || cards.has(id)) return;
      const box = el("div", "approval");
      const clarify = p.kind === "clarify";
      box.append(el("b", "", clarify ? "Question" : words("approval")));
      if (p.description) box.append(el("div", "", p.description));
      if (p.command) box.append(el("pre", "", p.command));
      if (p.question) box.append(el("div", "", p.question));
      const actions = el("div", "actions");
      const buttons = [];
      const card = { id, box, actions, buttons, state: "open", timer: null, note: null, body: null };
      cards.set(id, card);
      const enable = (on) => { for (const b of buttons) b.disabled = !on; };

      const send = async (body) => {
        card.body = body;
        card.state = "sending";
        enable(false);
        note(card, "");
        try {
          if (body.allow !== undefined) await api("POST", base + "/approvals/" + encodeURIComponent(id), body);
          else await api("POST", base + "/prompts/" + encodeURIComponent(id) + "/answer", body);
        } catch (e) {
          if (done(card)) return;
          // 404: the prompt was settled where this page could not see it (another tab, Telegram, a timeout,
          // a restart of Hermes)
          if (e && e.status === 404) { finish(id, "No longer waiting: Hermes closed this before the answer arrived"); return; }
          card.state = "open";
          enable(true);
          note(card, "Not delivered: " + ((e && e.message) || "the hub did not answer") + ". Press a button to try again.", true);
          banner("Could not answer: " + ((e && e.message) || "the hub did not answer"), true);
          return;
        }
        if (done(card)) return;
        // The hub answers 202 once Hermes took the answer. The card is settled only when Hermes says so.
        card.state = "sent";
        note(card, "Sent. Waiting for Hermes to confirm.");
        card.timer = setT(() => {
          card.timer = null;
          if (card.state !== "sent") return;
          card.state = "open";
          enable(true);
          note(card, "Hermes has not confirmed this answer. Press a button to send it again.", true);
        }, ackMs);
      };

      const button = (cls, text, body) => {
        const b = el("button", cls, text);
        b.type = "button";
        b.onclick = () => send(typeof body === "function" ? body() : body);
        buttons.push(b);
        return b;
      };
      const choices = Array.isArray(p.choices) ? p.choices : [];
      if (!clarify && !choices.length) {
        actions.append(button("primary", words("allow"), { allow: true }), button("danger", words("deny"), { allow: false }));
      } else if (!clarify) {
        const names = { once: words("allow"), session: "Allow in this chat", always: "Always allow", deny: words("deny") };
        for (const c of choices) actions.append(button(c === "deny" ? "danger" : c === "once" ? "primary" : "", names[c] || c, { choice: c }));
      } else {
        const picks = [];
        if (p.multi_select) {
          const list = el("div", "picks");
          for (const c of choices) {
            const row = el("label", "pick");
            const cb = el("input");
            cb.type = "checkbox";
            cb.value = c;
            picks.push(cb);
            buttons.push(cb);
            row.append(cb, el("span", "", c));
            list.append(row);
          }
          box.append(list);
          actions.append(button("primary", "Send", () => ({ picks: picks.filter((b) => b.checked).map((b) => b.value) })));
        } else {
          for (const c of choices) actions.append(button("", c, { picks: [c] }));
        }
        const input = el("input", "answer");
        input.type = "text";
        input.placeholder = choices.length ? "Or type an answer" : "Type your answer";
        input.setAttribute("aria-label", "Answer");
        buttons.push(input);
        const sendBtn = button(choices.length ? "" : "primary", "Answer", () => ({ text: input.value }));
        input.onkeydown = (e) => { if (e.key === "Enter" && input.value.trim()) { e.preventDefault(); sendBtn.click(); } };
        const typed = el("div", "typed");
        typed.append(input, sendBtn);
        box.append(typed);
        actions.append(button("ghost", "Skip", { skip: true }));
      }
      box.append(actions);
      add(box);
    }

    // settled is Hermes's approval.settled: the prompt is over. The hub's
    // outcome event that follows says how, when the hub knows.
    function settled(id) {
      const card = cards.get(id);
      if (!card || done(card)) return;
      finish(id, card.state === "sent" || card.state === "sending" ? "Answered" : "Closed by Hermes, no answer was seen here");
    }

    // outcome is the hub's approval.outcome: what became of the approval.
    function outcome(id, text) {
      const card = cards.get(id);
      if (!card) return;
      finish(id, text);
    }

    // lost is the hub's word that Hermes no longer holds these prompts and
    // nobody answered them: a restart or a drop. They get no answer.
    function lost(ids) {
      for (const id of ids || []) {
        const card = cards.get(id);
        if (card && !done(card)) finish(id, "Closed: Hermes no longer waits for this. It was answered elsewhere, or Hermes restarted and dropped it. Nothing was sent from this page.");
      }
    }

    function cancelled(ids) {
      for (const id of ids || []) finish(id, "Cancelled");
    }

    function dispose() {
      for (const card of cards.values()) if (card.timer) clearT(card.timer);
      cards.clear();
    }

    return { offer, settled, outcome, lost, cancelled, dispose, has: (id) => cards.has(id), card: (id) => cards.get(id) };
  }
  return { create };
})();
