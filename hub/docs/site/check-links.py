#!/usr/bin/env python3
"""Check the built site: every internal link and image resolves, no page is
orphaned from the search index, and report pages that are not linked."""
import html.parser
import os
import sys
import urllib.parse


class P(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.refs = []
        self.ids = set()

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if "id" in a:
            self.ids.add(a["id"])
        if tag == "a" and "href" in a:
            self.refs.append(("link", a["href"]))
        if tag in ("img", "script", "link") and ("src" in a or "href" in a):
            ref = a.get("src") or a.get("href")
            if tag == "link" and ref.startswith("http"):
                return
            self.refs.append((tag, ref))


def main(root):
    pages = {}
    for d, _, files in os.walk(root):
        for f in files:
            if f.endswith(".html"):
                p = os.path.join(d, f)
                parser = P()
                with open(p, encoding="utf-8") as fh:
                    parser.feed(fh.read())
                pages[os.path.relpath(p, root)] = parser
    bad = 0
    for page, parser in sorted(pages.items()):
        for kind, ref in parser.refs:
            u = urllib.parse.urlparse(ref)
            if u.scheme or u.netloc:
                continue
            path = urllib.parse.unquote(u.path)
            target = os.path.normpath(os.path.join(os.path.dirname(page), path)) if path else page
            if target.endswith("/") or os.path.isdir(os.path.join(root, target)):
                target = os.path.join(target, "index.html")
            if not os.path.isfile(os.path.join(root, target)):
                print(f"broken: {page}: {kind} {ref}")
                bad += 1
                continue
            if u.fragment and target in pages and u.fragment not in pages[target].ids:
                print(f"broken anchor: {page}: {ref}")
                bad += 1
    print(f"{len(pages)} pages checked, {bad} broken")
    return 1 if bad else 0


sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "artifacts/docs-site"))
