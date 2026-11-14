"use strict";
// A small Markdown renderer for assistant replies. It builds DOM nodes and
// sets text only through text nodes, so model output can never inject HTML.
// Links keep only http, https and mailto targets. Code blocks are colored by
// the vendored highlight.js, whose output escapes the code it is given.
window.hubMarkdown = (() => {
  const SAFE = new Set(["http:", "https:", "mailto:"]);
  const MAX_HIGHLIGHT = 100000;
  // MAX_DEPTH bounds nested quotes and lists; deeper ones show as text.
  const MAX_DEPTH = 12;

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = text;
    return e;
  }

  function safeURL(raw) {
    const s = raw.trim().replace(/^<|>$/g, "");
    if (!/^(https?:|mailto:)/i.test(s)) return null;
    try {
      const u = new URL(s);
      return SAFE.has(u.protocol) ? u.href : null;
    } catch (_) {
      return null;
    }
  }

  function link(href, label) {
    const a = el("a");
    a.href = href;
    a.rel = "noopener noreferrer";
    a.target = "_blank";
    if (typeof label === "string") a.textContent = label;
    else a.append(label);
    return a;
  }

  const PUNCT = /[!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~]/;
  const WORD = /[\p{L}\p{N}]/u;

  // closing finds the end delimiter of an emphasis run that starts at i.
  function closing(s, i, delim) {
    let j = i + delim.length;
    if (j >= s.length || /\s/.test(s[j])) return -1;
    for (;;) {
      j = s.indexOf(delim, j + 1);
      if (j < 0) return -1;
      if (s[j - 1] === "\\" || /\s/.test(s[j - 1])) continue;
      if (delim.length === 1 && s[j + 1] === delim) { j++; continue; }
      if (delim[0] === "_" && WORD.test(s[j + delim.length] || "")) continue;
      return j;
    }
  }

  // brackets maps each "[" in s to the "]" that closes it on the same line,
  // in one pass.
  function brackets(s) {
    const out = new Map();
    let stack = [];
    for (let j = 0; j < s.length; j++) {
      const c = s[j];
      if (c === "\\") { j++; continue; }
      if (c === "\n") stack = [];
      else if (c === "[") stack.push(j);
      else if (c === "]" && stack.length) out.set(stack.pop(), j);
    }
    return out;
  }

  // paren finds the ")" that closes the "(" at i.
  function paren(s, i) {
    let depth = 0;
    for (let j = i; j < s.length; j++) {
      if (s[j] === "\\") { j++; continue; }
      if (s[j] === "(") depth++;
      else if (s[j] === ")" && --depth === 0) return j;
      else if (s[j] === "\n") return -1;
    }
    return -1;
  }

  function bareURL(s, i) {
    const m = /^(https?:\/\/|mailto:)[^\s<>"]+/i.exec(s.slice(i));
    if (!m) return null;
    let u = m[0];
    for (;;) {
      const last = u[u.length - 1];
      if (".,;:!?'*_~".includes(last)) { u = u.slice(0, -1); continue; }
      if (last === ")" && (u.match(/\(/g) || []).length < (u.match(/\)/g) || []).length) { u = u.slice(0, -1); continue; }
      break;
    }
    return u;
  }

  const EMPHASIS = [["**", "strong"], ["__", "strong"], ["~~", "del"], ["*", "em"], ["_", "em"]];

  function inline(s, parent, inLink) {
    let buf = "";
    // dead[delim] is an opener position with no closer after it; no later
    // opener can have one either, which keeps a run of stray "*" linear.
    const dead = {};
    let closers = null;
    const flush = () => { if (buf) { parent.append(document.createTextNode(buf)); buf = ""; } };
    for (let i = 0; i < s.length; i++) {
      const c = s[i];
      if (c === "\\" && i + 1 < s.length && PUNCT.test(s[i + 1])) { buf += s[++i]; continue; }
      if (c === "\n") { flush(); parent.append(el("br")); continue; }
      if (c === "`") {
        let n = 1;
        while (s[i + n] === "`") n++;
        const fence = "`".repeat(n);
        let j = s.indexOf(fence, i + n);
        while (j >= 0 && s[j + n] === "`") j = s.indexOf(fence, j + n + 1);
        if (j < 0) { buf += fence; i += n - 1; continue; }
        let code = s.slice(i + n, j).replace(/\n/g, " ");
        if (code.length > 2 && code[0] === " " && code[code.length - 1] === " ") code = code.slice(1, -1);
        flush();
        parent.append(el("code", "", code));
        i = j + n - 1;
        continue;
      }
      const isImage = c === "!" && s[i + 1] === "[";
      if ((c === "[" || isImage) && !inLink) {
        const open = isImage ? i + 1 : i;
        if (!closers) closers = brackets(s);
        const close = closers.has(open) ? closers.get(open) : -1;
        if (close > 0 && s[close + 1] === "(") {
          const end = paren(s, close + 1);
          if (end > 0) {
            const inner = s.slice(close + 2, end).trim();
            const angle = /^<([^>\n]*)>/.exec(inner);
            const target = angle ? angle[1] : inner.split(/\s+/)[0] || "";
            const href = safeURL(target);
            const label = s.slice(open + 1, close);
            flush();
            const local = !href && localMedia && target[0] === "/" ? localMedia(target, label, isImage) : null;
            if (local) {
              parent.append(local);
            } else if (href) {
              const span = document.createDocumentFragment();
              inline(isImage ? (label || "image") : label, span, true);
              parent.append(link(href, span));
            } else {
              inline(label, parent, inLink);
            }
            i = end;
            continue;
          }
        }
      }
      if (c === "<" && !inLink) {
        const end = s.indexOf(">", i);
        const href = end > 0 ? safeURL(s.slice(i + 1, end)) : null;
        if (href) {
          flush();
          parent.append(link(href, s.slice(i + 1, end)));
          i = end;
          continue;
        }
      }
      if ((c === "h" || c === "H" || c === "m" || c === "M") && !inLink && !WORD.test(s[i - 1] || "")) {
        const u = bareURL(s, i);
        const href = u ? safeURL(u) : null;
        if (href) {
          flush();
          parent.append(link(href, u));
          i += u.length - 1;
          continue;
        }
      }
      let matched = false;
      for (const [delim, tag] of EMPHASIS) {
        if (!s.startsWith(delim, i)) continue;
        if (delim[0] === "_" && WORD.test(s[i - 1] || "")) break;
        if (dead[delim] !== undefined && i >= dead[delim]) continue;
        const j = closing(s, i, delim);
        if (j < 0) { dead[delim] = i; continue; }
        flush();
        const node = el(tag);
        inline(s.slice(i + delim.length, j), node, inLink);
        parent.append(node);
        i = j + delim.length - 1;
        matched = true;
        break;
      }
      if (!matched) buf += c;
    }
    flush();
  }

  const FENCE = /^ {0,3}(`{3,}|~{3,})\s*([^`\s]*)[^`]*$/;
  const HEADING = /^ {0,3}(#{1,6})(?:\s+(.*?))?(?:\s+#+)?\s*$/;
  const RULE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
  const QUOTE = /^ {0,3}> ?(.*)$/;
  const ITEM = /^( *)([-*+]|\d{1,9}[.)])(?:[ \t]+(.*))?$/;
  const DELIM_ROW = /^\s*\|?\s*:?-+:?\s*(\|\s*:?-+:?\s*)*\|?\s*$/;

  function cells(line) {
    let s = line.trim();
    if (s.startsWith("|")) s = s.slice(1);
    if (s.endsWith("|") && !s.endsWith("\\|")) s = s.slice(0, -1);
    const out = [];
    let cur = "";
    let tick = false;
    for (let i = 0; i < s.length; i++) {
      if (s[i] === "\\" && s[i + 1] === "|") { cur += "|"; i++; continue; }
      if (s[i] === "`") tick = !tick;
      if (s[i] === "|" && !tick) { out.push(cur.trim()); cur = ""; continue; }
      cur += s[i];
    }
    out.push(cur.trim());
    return out;
  }

  function isTable(lines, i) {
    return lines[i].includes("|") && i + 1 < lines.length && DELIM_ROW.test(lines[i + 1]) && lines[i + 1].includes("-");
  }

  function startsBlock(lines, i) {
    const l = lines[i];
    return FENCE.test(l) || HEADING.test(l) || RULE.test(l) || QUOTE.test(l) || isTable(lines, i) ||
      (ITEM.test(l) && ITEM.exec(l)[3] !== undefined);
  }

  const indentOf = (l) => l.length - l.trimStart().length;

  let seen = null;
  function lazy() {
    if (seen === null) {
      seen = typeof IntersectionObserver === "function"
        ? new IntersectionObserver((list) => {
          for (const e of list) {
            if (!e.isIntersecting) continue;
            seen.unobserve(e.target);
            if (e.target.hubPaint) { const f = e.target.hubPaint; e.target.hubPaint = null; f(); }
          }
        }, { rootMargin: "800px 0px" })
        : false;
    }
    return seen || null;
  }

  // One listener serves every code block's Copy button.
  if (typeof document !== "undefined" && document.addEventListener) {
    document.addEventListener("click", (e) => {
      const b = e.target && e.target.closest && e.target.closest("button.copy");
      if (b && b.hubCopy !== undefined) copy(b.hubCopy, b);
    });
  }

  function codeBlock(lang, text) {
    const box = el("div", "codeblock");
    const head = el("div", "codehead");
    head.append(el("span", "lang", lang || "code"));
    const btn = el("button", "copy", "Copy");
    btn.type = "button";
    btn.hubCopy = text;
    head.append(btn);
    const pre = el("pre");
    const code = el("code");
    code.textContent = text;
    const hl = window.hljs;
    const name = lang && hl && hl.getLanguage(lang) ? lang : "";
    if (name && text.length <= MAX_HIGHLIGHT) {
      const paint = () => {
        try {
          code.innerHTML = hl.highlight(text, { language: name, ignoreIllegals: true }).value;
          code.className = "hljs language-" + name;
        } catch (_) {
          code.textContent = text;
        }
      };
      // Highlight when the block comes near the screen, not when it is built:
      // a long chat has hundreds of blocks and shows a few.
      const io = lazy();
      if (io) {
        code.hubPaint = paint;
        io.observe(code);
      } else {
        paint();
      }
    }
    pre.append(code);
    box.append(head, pre);
    return box;
  }

  async function copy(text, btn) {
    let ok = false;
    try {
      await navigator.clipboard.writeText(text);
      ok = true;
    } catch (_) {
      const ta = el("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.append(ta);
      ta.select();
      try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
      ta.remove();
    }
    btn.textContent = ok ? "Copied" : "Copy failed";
    setTimeout(() => { btn.textContent = "Copy"; }, 1500);
  }

  function table(lines, i, parent) {
    const head = cells(lines[i]);
    const align = cells(lines[i + 1]).map((c) => {
      const l = c.startsWith(":");
      const r = c.endsWith(":");
      return l && r ? "center" : r ? "right" : l ? "left" : "";
    });
    const wrap = el("div", "table-wrap");
    const tbl = el("table");
    const thead = el("thead");
    const tr = el("tr");
    head.forEach((c, k) => {
      const th = el("th");
      if (align[k]) th.style.textAlign = align[k];
      inline(c, th);
      tr.append(th);
    });
    thead.append(tr);
    tbl.append(thead);
    const body = el("tbody");
    let j = i + 2;
    while (j < lines.length && lines[j].trim() && lines[j].includes("|")) {
      const row = el("tr");
      const cs = cells(lines[j]);
      for (let k = 0; k < head.length; k++) {
        const td = el("td");
        if (align[k]) td.style.textAlign = align[k];
        inline(cs[k] || "", td);
        if (td.textContent.length > 40) td.className = "wide";
        row.append(td);
      }
      body.append(row);
      j++;
    }
    tbl.append(body);
    wrap.append(tbl);
    parent.append(wrap);
    return j;
  }

  function list(lines, i, parent, depth) {
    const first = ITEM.exec(lines[i]);
    const base = first[1].length;
    const ordered = /\d/.test(first[2]);
    const marker = first[2].slice(-1);
    const node = el(ordered ? "ol" : "ul");
    if (ordered && parseInt(first[2], 10) !== 1) node.start = parseInt(first[2], 10);
    let loose = false;
    let j = i;
    const items = [];
    while (j < lines.length) {
      const m = ITEM.exec(lines[j]);
      if (!m || m[1].length !== base || /\d/.test(m[2]) !== ordered || m[2].slice(-1) !== marker || RULE.test(lines[j])) break;
      const after = lines[j].slice(base + m[2].length);
      const content = base + m[2].length + (m[3] ? after.length - after.trimStart().length : 1);
      const body = [m[3] || ""];
      j++;
      while (j < lines.length) {
        const l = lines[j];
        if (!l.trim()) {
          let k = j;
          while (k < lines.length && !lines[k].trim()) k++;
          if (k < lines.length && indentOf(lines[k]) > base) {
            body.push(...lines.slice(j, k));
            loose = true;
            j = k;
            continue;
          }
          break;
        }
        const ind = indentOf(l);
        if (ind > base) { body.push(l.slice(Math.min(ind, content))); j++; continue; }
        if (ITEM.test(l) || startsBlock(lines, j)) break;
        body.push(l);
        j++;
      }
      items.push(body);
      if (j < lines.length && !lines[j].trim()) {
        let k = j;
        while (k < lines.length && !lines[k].trim()) k++;
        const n = k < lines.length ? ITEM.exec(lines[k]) : null;
        if (n && n[1].length === base && /\d/.test(n[2]) === ordered) { loose = true; j = k; }
      }
    }
    for (const body of items) {
      const li = el("li");
      const task = /^\[([ xX])\]\s+/.exec(body[0]);
      if (task) {
        const box = el("input");
        box.type = "checkbox";
        box.disabled = true;
        box.checked = task[1] !== " ";
        li.append(box, " ");
        li.classList.add("task");
        body[0] = body[0].slice(task[0].length);
      }
      blocks(body, li, !loose, depth + 1);
      node.append(li);
    }
    parent.append(node);
    return j;
  }

  // blocks renders lines as block elements. tight lists put their
  // paragraphs' text straight into the list item.
  function blocks(lines, parent, tight, depth) {
    let i = 0;
    let wrote = false;
    while (i < lines.length) {
      const l = lines[i];
      if (!l.trim()) { i++; continue; }
      let m = FENCE.exec(l);
      if (m) {
        const fence = m[1];
        const indent = indentOf(l);
        const body = [];
        i++;
        while (i < lines.length) {
          const t = lines[i].trim();
          if (t.startsWith(fence) && t.split("").every((c) => c === fence[0])) { i++; break; }
          body.push(lines[i].slice(Math.min(indent, indentOf(lines[i]))));
          i++;
        }
        parent.append(codeBlock(m[2].toLowerCase(), body.join("\n")));
        continue;
      }
      m = HEADING.exec(l);
      if (m) {
        const h = el("h" + m[1].length);
        inline(m[2] || "", h);
        parent.append(h);
        i++;
        continue;
      }
      if (RULE.test(l)) { parent.append(el("hr")); i++; continue; }
      if (QUOTE.test(l)) {
        const body = [];
        while (i < lines.length && lines[i].trim()) {
          const q = QUOTE.exec(lines[i]);
          if (q) body.push(q[1]);
          else if (startsBlock(lines, i)) break;
          else body.push(lines[i]);
          i++;
        }
        const bq = el("blockquote");
        if (depth >= MAX_DEPTH) bq.append(document.createTextNode(body.join("\n")));
        else blocks(body, bq, false, depth + 1);
        parent.append(bq);
        continue;
      }
      if (isTable(lines, i)) { i = table(lines, i, parent); continue; }
      if (ITEM.test(l) && depth < MAX_DEPTH) { i = list(lines, i, parent, depth); continue; }
      const para = [];
      while (i < lines.length && lines[i].trim() && (para.length === 0 || !startsBlock(lines, i))) {
        para.push(lines[i].trim());
        i++;
      }
      if (tight) {
        if (wrote) parent.append(el("br"));
        inline(para.join("\n"), parent);
        wrote = true;
      } else {
        const p = el("p");
        inline(para.join("\n"), p);
        parent.append(p);
      }
    }
  }

  // plainPara says a paragraph of lines (no blank line in it) is a block that
  // nothing after the next blank line can join: no list item, quote, indented
  // line or fence in it, which a loose list or a continuation could extend.
  const plainPara = (ls) => ls.every((l) => !/^[ \t]/.test(l) && !ITEM.test(l) && !QUOTE.test(l) && !FENCE.test(l));

  // cutAt returns the line number where the open part of lines starts: the
  // start of the last paragraph after a plain one, outside any code fence,
  // from line from on. It is from when there is no such place.
  function cutAt(lines, from) {
    let cut = from;
    let fence = null;
    let start = from;
    let prevPlain = false;
    for (let i = from; i <= lines.length; i++) {
      const l = i < lines.length ? lines[i] : "";
      if (fence) {
        const m = FENCE.exec(l);
        if (m && m[1][0] === fence[0] && m[1].length >= fence.length && !m[2]) { fence = null; }
        if (!fence) start = i + 1, prevPlain = false;
        continue;
      }
      if (i < lines.length && FENCE.test(l)) { fence = FENCE.exec(l)[1]; continue; }
      if (l.trim() !== "") continue;
      // A blank line ends the paragraph that began at start.
      const para = lines.slice(start, i);
      if (para.length) {
        prevPlain = plainPara(para);
        // The paragraph after it opens at the next non-blank line.
        let j = i + 1;
        while (j < lines.length && lines[j].trim() === "") j++;
        if (prevPlain && j < lines.length && !/^[ \t]/.test(lines[j])) cut = j;
      }
      start = i + 1;
    }
    return cut;
  }

  // incremental draws a streaming reply: the blocks before the cut stay as
  // drawn, the rest is drawn again.
  function incremental(node, lines) {
    let st = node.mdInc;
    const head = st ? lines.slice(0, st.lines).join("\n") : "";
    if (!st || head !== st.text || node.childNodes.length < st.nodes) {
      node.replaceChildren();
      st = { lines: 0, text: "", nodes: 0 };
    }
    const cut = cutAt(lines, st.lines);
    const keep = st.nodes;
    while (node.childNodes.length > keep) node.removeChild(node.lastChild);
    if (cut > st.lines) {
      const f = document.createDocumentFragment();
      blocks(lines.slice(st.lines, cut), f, false, 0);
      node.appendChild(f);
      st = { lines: cut, text: lines.slice(0, cut).join("\n"), nodes: node.childNodes.length };
    }
    const f = document.createDocumentFragment();
    blocks(lines.slice(st.lines), f, false, 0);
    node.appendChild(f);
    node.mdInc = st;
  }

  // localMedia, set while render runs with a hook, turns a link or image
  // whose target is an absolute path into a node, or null to show its label.
  let localMedia = null;

  // render replaces node's children with text rendered as Markdown.
  // local(path, label, isImage), when given, makes a node for a link or
  // image that names an absolute local path; without it such a target
  // shows as its label. On any failure the text shows as it came,
  // unformatted.
  //
  // With inc true the text is a reply that is still streaming, and render
  // keeps what it drew of the blocks that cannot change any more (node.mdInc):
  // only the open last blocks are parsed and drawn again. The final render
  // (inc false) draws it all, so the end state is the same as ever.
  function render(node, text, local, inc) {
    const raw = String(text);
    localMedia = local || null;
    try {
      const lines = raw.replace(/\r\n?/g, "\n").split("\n");
      if (inc) {
        incremental(node, lines);
      } else {
        node.mdInc = null;
        const frag = document.createDocumentFragment();
        blocks(lines, frag, false, 0);
        node.replaceChildren(frag);
      }
      node.classList.remove("plain");
    } catch (_) {
      node.mdInc = null;
      node.textContent = raw;
      node.classList.add("plain");
    } finally {
      localMedia = null;
    }
  }

  return { render, safeURL, codeBlock };
})();
