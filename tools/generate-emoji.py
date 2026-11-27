#!/usr/bin/env python3
"""Build the Slack plugin's compact standard-emoji table.

Input: emoji.json from https://github.com/iamcal/emoji-data (MIT License, the
short-name set Slack uses), pinned to SOURCE_COMMIT and verified against
SOURCE_SHA256 so the embedded table is reproducible. Usage:

    curl -fsSLo /tmp/emoji-src/emoji.json \
      https://raw.githubusercontent.com/iamcal/emoji-data/<SOURCE_COMMIT>/emoji.json
    python3 -I tools/generate-emoji.py /tmp/emoji-src/emoji.json > internal/plugins/slack/emoji/emoji.json

To update the dataset, change both pins in one commit after reviewing the diff.

Output rows are [name, glyph, category index, "space separated aliases"], in the
dataset's sort order. Skin-tone variations and the Component category are omitted.
"""
import hashlib
import json
import sys

SOURCE_COMMIT = "e77759c296ed37beb0dbd63908b43d8619509c6d"
SOURCE_SHA256 = "8736d721991e48c41c57acc46b65e5d32258a45e4bcdebcc6012f420f81e775d"

raw = open(sys.argv[1], "rb").read()
digest = hashlib.sha256(raw).hexdigest()
if digest != SOURCE_SHA256:
    sys.exit(f"emoji.json SHA-256 {digest} does not match pinned {SOURCE_SHA256} (commit {SOURCE_COMMIT})")
src = json.loads(raw.decode("utf-8"))
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
json.dump({"source": f"iamcal/emoji-data@{SOURCE_COMMIT} (MIT)", "categories": categories, "emoji": rows},
          sys.stdout, ensure_ascii=False, separators=(",", ":"))
sys.stdout.write("\n")
