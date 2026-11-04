#!/usr/bin/env python3
"""Writes the emoji a reaction may be (docs/adr/0037-reactions-pins.md).

Reads emojibase-data (MIT, https://github.com/milesj/emojibase,
packages/data) and prints every fully-qualified emoji, skin tones included,
one per line in emojibase order. Components (skin tone and hair swatches
alone) and lone regional indicators are left out: they are parts, not emoji.

    scripts/gen-emoji.py path/to/emojibase/packages/data > internal/message/emoji.txt
"""
import json
import sys

data = json.load(open(sys.argv[1] + "/en/compact.raw.json", encoding="utf-8"))
COMPONENT = 2
out = []
for e in sorted(data, key=lambda e: e.get("order", 0)):
    if e.get("group") in (None, COMPONENT):
        continue
    out.append(e["unicode"])
    out.extend(s["unicode"] for s in e.get("skins", []))
version = json.load(open(sys.argv[1] + "/package.json", encoding="utf-8"))["version"]
print(f"# emojibase-data {version}: {len(out)} emoji, one per line", file=sys.stdout)
print("\n".join(out))
