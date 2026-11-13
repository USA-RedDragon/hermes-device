"use strict";
// HubPlain turns an internal id (a toolset, a provider, a config key) into
// words for the screen: "web_search" becomes "Web search". The hub sends
// labels for tools, skills and connections; this is the fallback for any
// other id that reaches the page, so no snake_case or dotted key is shown.
window.HubPlain = (() => {
  const acronyms = { api: "API", mcp: "MCP", url: "URL", oauth: "OAuth", ssh: "SSH", ai: "AI", ascii: "ASCII", svg: "SVG", html: "HTML", pcb: "PCB", rf: "RF", id: "ID", tts: "TTS", stt: "STT", pdf: "PDF", gpu: "GPU", cli: "CLI", llm: "LLM", openai: "OpenAI", github: "GitHub" };
  const text = (id) => {
    const s = String(id == null ? "" : id).trim();
    if (!s || /[\s]/.test(s) || /[A-Z]/.test(s)) return s;
    const w = s.split(/[_\-.:/]+/).filter(Boolean).map((x) => acronyms[x] || x);
    if (w.length && w[0] === w[0].toLowerCase()) w[0] = w[0][0].toUpperCase() + w[0].slice(1);
    return w.join(" ");
  };
  // clean drops ids (snake_case or dotted keys) from a sentence that came
  // from somewhere else; it answers "" when one shows.
  const clean = (s) => (/[A-Za-z0-9]+(?:[_.][A-Za-z0-9]+)+/.test(String(s || "")) ? "" : String(s || ""));
  return { text, clean };
})();
