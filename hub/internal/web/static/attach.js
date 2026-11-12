"use strict";
// HubAttach reads the lines Hermes and the hub keep in a user message for
// its attachments: the gateway's image hints and placeholders
// (agent/image_routing.py, agent/image_replay.py), @image refs, inline
// image data, and the hub's note for a file (hub.FileNote). split(text)
// returns the text without them and the attachments they name. An image
// the hub also copied to the chat's computer has a note and a hint; they
// give one attachment. The Go side reads the same lines in
// internal/hub/media.go (AttachedImages) and internal/telegram/echo.go.
window.HubAttach = (() => {
  const imageHint = /^\[Image attached(?: at)?: (.+)\]$/;
  const defaultCaption = "What do you see in this image?";
  const imageRef = /^@image:(?:`([^`]+)`|"([^"]+)"|'([^']+)'|(\S+))$/;
  const imageData = /^data:image\/(?:png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]+$/;
  const placeholder = /^\[screenshot(?: detail=\w+)?\]$/;
  const fileNote = /^\[The user attached a file: (.+)\. It is saved at (\/.*)\. Read it with your tools when the request needs its contents\.\]$/;
  // The hub's lines for a PDF it read (internal/service/pdf.go): a summary,
  // then the PDF's text between markers with a random id.
  const pdfNote = /^\[The PDF (.+) has (\d+) pages?\. .*\]$/;
  const pdfFailed = /^\[The hub could not read the PDF .+\. Its pages are not shown\.\]$/;
  const pdfStart = /^<untrusted-pdf-text id="([0-9a-f]{16})">$/;
  const imageExts = new Set(["png", "jpg", "jpeg", "gif", "webp"]);
  const baseName = (p) => p.replace(/\/+$/, "").split("/").pop() || p;
  const imagePath = (p) => {
    const b = baseName(p);
    const i = b.lastIndexOf(".");
    return i >= 0 && imageExts.has(b.slice(i + 1).toLowerCase());
  };

  // cutPage drops the block the hub adds to a page chat message (hub.PageContextHead).
  const pageHead = "[Talaria page chat.";
  function cutPage(text) {
    const s = String(text || "");
    const i = s.indexOf("\n" + pageHead);
    if (i >= 0) return s.slice(0, i).replace(/\n+$/, "");
    return s.startsWith(pageHead) ? "" : s;
  }

  // joinNotes folds each image's note into its hint, in order, so one
  // image is one attachment: the hint's path is the copy on Hermes, the
  // note's the copy on the chat's computer, which it falls back to. A
  // note without a hint, as in the live text of a turn, stands for the
  // image. PDFs stage page images with hints of their own, so a message
  // with one keeps its notes apart.
  function joinNotes(atts) {
    if (atts.some((a) => a.pdf)) return atts;
    const hints = atts.filter((a) => a.hint);
    const joined = new Set();
    const out = [];
    let n = 0;
    for (const a of atts) {
      if (!a.note) {
        if (!joined.has(a)) out.push(a);
        continue;
      }
      const h = hints[n++];
      if (!h) {
        out.push(a);
        continue;
      }
      h.name = a.name;
      h.title = a.title;
      h.fallback = a.path;
      joined.add(h);
      out.push(h);
    }
    return out;
  }

  function split(text) {
    text = cutPage(text);
    let atts = [];
    const keep = [];
    let skipTo = "";
    for (const line of String(text || "").split("\n")) {
      let m;
      if (skipTo) {
        if (line === skipTo) skipTo = "";
        continue;
      }
      const hubLines = atts.some((a) => a.file);
      if (hubLines && (m = line.match(pdfStart))) skipTo = "</untrusted-pdf-text id=\"" + m[1] + "\">";
      else if (hubLines && (m = line.match(pdfNote))) {
        const f = atts.find((a) => a.file && !a.pdf && a.name === m[1]);
        if (f) { f.pdf = true; f.pages = +m[2]; }
      } else if (hubLines && pdfFailed.test(line)) continue;
      else if ((m = line.match(imageHint))) atts.push({ name: baseName(m[1]), image: true, title: m[1], path: m[1], hint: true });
      else if ((m = line.match(imageRef))) {
        const p = m[1] || m[2] || m[3] || m[4];
        atts.push({ name: baseName(p), image: true, title: p, path: p, hint: true });
      } else if (imageData.test(line)) atts.push({ name: "image", image: true, url: line });
      else if ((m = line.match(fileNote))) {
        if (imagePath(m[2])) atts.push({ name: m[1], image: true, title: m[2], path: m[2], note: true });
        else atts.push({ name: m[1], image: false, title: m[2], file: true });
      } else if (!placeholder.test(line)) keep.push(line);
    }
    if (!atts.length) return { text, atts };
    atts = joinNotes(atts);
    let rest = keep.join("\n").trim();
    if (rest === defaultCaption && atts.some((a) => a.image)) rest = "";
    return { text: rest, atts };
  }

  return { split };
})();
