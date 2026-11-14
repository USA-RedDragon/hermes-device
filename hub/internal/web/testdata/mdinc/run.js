"use strict";
// usage: node run.js CHROME HUB_URL PROFILE_DIR. Streams random markdown
// into one node in 1 to 12 character steps, rendering incrementally after
// each, and compares the node with a full render of the same text each time.
const assert = require("assert");
const [bin, url, dir] = process.argv.slice(2);
const { proc, connect } = require("../cdp.js").launch(bin, dir);

(async () => {
  const c = await connect();
  const { ev } = c;
  await c.load(url + "/auth/login?next=/", `document.getElementById("sessions").children.length > 0`, "the page");
  const r = await ev(`(() => {
    let seed = 12345;
    const rnd = (n) => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed % n; };
    const parts = ["Some words here.", "A line with **bold** and \`code\` and [a link](https://example.com).", "# A heading", "## Another", "- item one", "- item two", "  - nested item", "1. first", "2. second", "> a quote", "> more quote", "| a | b |", "|---|---|", "| 1 | 2 |", "\`\`\`js", "let x = 1;", "", "x++;", "\`\`\`", "~~~", "---", "    indented code", "text right after", "* star item", "Setext", "===", "-----", "1) paren item", "", "", ""];
    const fails = [];
    let renders = 0, kept = 0;
    for (let t = 0; t < 120; t++) {
      const lines = [];
      const n = 5 + rnd(40);
      for (let i = 0; i < n; i++) lines.push(parts[rnd(parts.length)]);
      const text = lines.join("\\n") + (rnd(2) ? "\\n" : "");
      const node = document.createElement("div");
      let at = 0;
      while (at < text.length) {
        at = Math.min(text.length, at + 1 + rnd(12));
        const part = text.slice(0, at);
        window.hubMarkdown.render(node, part, null, true);
        const full = document.createElement("div");
        window.hubMarkdown.render(full, part, null, false);
        renders++;
        if (node.mdInc && node.mdInc.lines > 0) kept++;
        if (node.innerHTML !== full.innerHTML) { fails.push({ text: part, inc: node.innerHTML, full: full.innerHTML }); break; }
      }
      if (fails.length) break;
      window.hubMarkdown.render(node, text, null, false);
      const full = document.createElement("div");
      window.hubMarkdown.render(full, text, null, false);
      if (node.innerHTML !== full.innerHTML) { fails.push({ text, inc: node.innerHTML, full: full.innerHTML }); break; }
    }
    return { fails: fails.slice(0, 1), renders, kept };
  })()`);
  assert.deepStrictEqual(r.fails, [], JSON.stringify(r.fails));
  assert.ok(r.kept > r.renders / 10, "the render kept blocks: " + JSON.stringify([r.kept, r.renders]));
  console.log("ok", r.renders, r.kept);
  c.close();
  process.exit(0);
})().catch((e) => { console.error(e); proc.kill(); process.exit(1); });
