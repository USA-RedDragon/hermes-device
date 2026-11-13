"use strict";
// HubPageBlocks finds the page blocks of a reply. It follows hub.PageBlocks
// in the hub. pageBlocks(text, streaming) answers { pages, text, writing }:
// every block whose closing fence is in text is a page, at once, even while
// the reply streams; writing is true only for a block still open.
window.HubPageBlocks = (() => {
  const pageOpen = /^ {0,3}(`{3,}|~{3,})[ \t]*(?:page|html[ \t]+page|hermes-page)[ \t]*$/i;
  const fenceStart = /^ {0,3}(`{3,}|~{3,})/;
  function fenceEnd(lines, from, marker) {
    for (let j = from; j < lines.length; j++) {
      const l = lines[j].replace(/\r$/, "").replace(/[ \t]+$/, "");
      const t = l.replace(/^ +/, "");
      if (l.length - t.length > 3 || t.length < marker.length) continue;
      if ([...t].every((c) => c === marker[0])) return j;
    }
    return -1;
  }

  // pageBlocks returns the page blocks of a reply and its text without
  // them. While a reply streams, a page block that is not closed yet is cut
  // too, and writing says so.
  function pageBlocks(text, streaming) {
    if (!text.includes("```") && !text.includes("~~~")) return { pages: [], text, writing: false };
    const lines = text.split("\n");
    const keep = [];
    const pages = [];
    let found = false;
    let writing = false;
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i].replace(/\r$/, "");
      const m = line.match(fenceStart);
      if (!m) { keep.push(lines[i]); continue; }
      let end = fenceEnd(lines, i + 1, m[1]);
      const page = pageOpen.test(line);
      if (page && end < 0 && streaming) { found = writing = true; break; }
      if (!page || end < 0) {
        if (end < 0) end = lines.length - 1;
        keep.push(...lines.slice(i, end + 1));
        i = end;
        continue;
      }
      const body = lines.slice(i + 1, end).join("\n");
      if (!body.trim()) {
        keep.push(...lines.slice(i, end + 1));
        i = end;
        continue;
      }
      i = end;
      found = true;
      if (!pages.includes(body)) pages.push(body);
    }
    if (!found) return { pages: [], text, writing: false };
    return { pages, text: keep.join("\n").replace(/\n{3,}/g, "\n\n").trim(), writing };
  }
  return { pageBlocks };
})();
