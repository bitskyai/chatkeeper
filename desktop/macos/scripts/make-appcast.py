#!/usr/bin/env python3
"""Writes a single-item Sparkle 2 appcast for a release zip.

The EdDSA signature comes from Sparkle's `sign_update` tool, whose output
looks like:  sparkle:edSignature="…" length="…"

    make-appcast.py --version 0.1.0 --build 42 --signature "$(sign_update …)" --out dist/appcast.xml
"""
import argparse
import email.utils
import pathlib
import re
from xml.sax.saxutils import quoteattr, escape

REPO = "bitskyai/chatkeeper"


def render(version: str, build: str, signature_line: str, pub_date: str) -> str:
    sig = re.search(r'sparkle:edSignature="([A-Za-z0-9+/=]+)"', signature_line)
    length = re.search(r'length="([0-9]+)"', signature_line)
    if not sig or not length:
        raise SystemExit("sign_update output missing edSignature/length")
    if not re.fullmatch(r"[0-9]+", build):
        raise SystemExit("build must be numeric")
    url = f"https://github.com/{REPO}/releases/download/v{version}/ChatKeeper-{version}.zip"
    notes = f"https://github.com/{REPO}/releases/tag/v{version}"
    return f"""<?xml version="1.0" encoding="utf-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>ChatKeeper</title>
    <link>https://github.com/{REPO}</link>
    <item>
      <title>ChatKeeper {escape(version)}</title>
      <pubDate>{escape(pub_date)}</pubDate>
      <sparkle:version>{escape(build)}</sparkle:version>
      <sparkle:shortVersionString>{escape(version)}</sparkle:shortVersionString>
      <sparkle:minimumSystemVersion>13.0</sparkle:minimumSystemVersion>
      <sparkle:releaseNotesLink>{escape(notes)}</sparkle:releaseNotesLink>
      <enclosure url={quoteattr(url)} length="{length.group(1)}" type="application/octet-stream" sparkle:edSignature={quoteattr(sig.group(1))}/>
    </item>
  </channel>
</rss>
"""


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--version", required=True)
    p.add_argument("--build", required=True)
    p.add_argument("--signature", required=True)
    p.add_argument("--out", required=True, type=pathlib.Path)
    a = p.parse_args()
    a.out.write_text(render(a.version.removeprefix("v"), a.build, a.signature, email.utils.formatdate(usegmt=True)))
    print(f"wrote {a.out}")


if __name__ == "__main__":
    main()
