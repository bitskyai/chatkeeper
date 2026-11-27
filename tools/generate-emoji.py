#!/usr/bin/env python3
"""Build the Slack plugin's compact standard-emoji table.

Input: emoji.json from https://github.com/iamcal/emoji-data (MIT License, the
short-name set Slack uses). Usage:

    python3 -I tools/generate-emoji.py path/to/emoji.json > internal/plugins/slack/emoji/emoji.json

Output rows are [name, glyph, category index, "space separated aliases"], in the
dataset's sort order. Skin-tone variations and the Component category are omitted.
"""
import json
import sys

src = json.load(open(sys.argv[1], encoding="utf-8"))
src = [e for e in src if e.get("category") != "Component"]
src.sort(key=lambda e: e["sort_order"])
categories = []
rows = []
seen = set()
for e in src:
    cat = e["category"]
    if cat not in categories:
        categories.append(cat)
    names = [n for n in e["short_names"] if n not in seen]
    if not names:
        continue
    seen.update(names)
    glyph = "".join(chr(int(p, 16)) for p in e["unified"].split("-"))
    rows.append([names[0], glyph, categories.index(cat), " ".join(names[1:])])
json.dump({"source": "iamcal/emoji-data (MIT)", "categories": categories, "emoji": rows},
          sys.stdout, ensure_ascii=False, separators=(",", ":"))
sys.stdout.write("\n")
