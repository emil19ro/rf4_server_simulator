"""Export official item / fish names from the RF4 client localization.

The client ships one JSON localization document per locale inside
resources.assets (a Unity TextAsset: int32 byte length, then UTF-8 JSON that
starts with {"localeId":"en_US",...}). Every item, fish and brand entry is an
object with "systemId" (the same id the server uses: item_id / fish_key) and a
localized "name".

This writes ref/client_names.json (ignored by git: official client content is
not committed, same rule as ref/icons.zip and ref/paper_maps/):

    {"version": 1, "locale": "en_US",
     "names": {"<systemId>": "<English name>", ...},
     "zh2en": {"<zh_CN name>": "<English name>", ...}}

The server uses it to show English names for database rows that only have a
Chinese name (see internal/admin/names.go). Re-run after a client update:

    python tools/extract_client_names.py --client-data "<game>/rf4_x64_Data"
"""
import argparse
import json
import re
import struct
import sys
from pathlib import Path

LOCALE_RE = rb'\{\s*"localeId"\s*:\s*"%s"'


def locale_document(data: bytes, locale: str) -> dict:
    """Return the parsed localization JSON for locale, or raise ValueError."""
    m = re.search(LOCALE_RE % re.escape(locale.encode()), data)
    if not m:
        raise ValueError(f"locale {locale} not found")
    start = m.start()
    if data[start - 3:start] == b"\xef\xbb\xbf":
        start -= 3
    if start < 4:
        raise ValueError(f"locale {locale}: no length prefix")
    (length,) = struct.unpack_from("<i", data, start - 4)
    if not 0 < length <= len(data) - start:
        raise ValueError(f"locale {locale}: bad length {length}")
    return json.loads(data[start:start + length].decode("utf-8-sig"))


def collect_names(doc) -> dict:
    """Map every systemId to its name (first occurrence wins)."""
    out = {}
    stack = [doc]
    while stack:
        o = stack.pop()
        if isinstance(o, dict):
            sid, name = o.get("systemId"), o.get("name")
            if isinstance(sid, str) and isinstance(name, str) and name.strip():
                out.setdefault(sid, name.strip())
            stack.extend(o.values())
        elif isinstance(o, list):
            stack.extend(o)
    return out


def build(data: bytes, locale: str = "en_US", source: str = "zh_CN") -> dict:
    names = collect_names(locale_document(data, locale))
    zh = collect_names(locale_document(data, source))
    zh2en = {}
    for sid, zname in zh.items():
        en = names.get(sid)
        if en and zname != en:
            zh2en.setdefault(zname, en)
    return {"version": 1, "locale": locale, "names": dict(sorted(names.items())), "zh2en": dict(sorted(zh2en.items()))}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    ap.add_argument("--client-data", required=True, help="path to the client's rf4_x64_Data folder")
    ap.add_argument("--out", default=str(Path(__file__).resolve().parents[1] / "ref" / "client_names.json"))
    args = ap.parse_args(argv)
    assets = Path(args.client_data) / "resources.assets"
    if not assets.is_file():
        print(f"not found: {assets}", file=sys.stderr)
        return 2
    result = build(assets.read_bytes())
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(result, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
    print(f"wrote {out}: {len(result['names'])} names, {len(result['zh2en'])} zh->en pairs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
