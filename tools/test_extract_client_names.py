import json
import struct
import unittest

from extract_client_names import build, collect_names, locale_document


def textasset(name: str, doc: dict) -> bytes:
    """Serialize like a Unity TextAsset: name, then int32 length + BOM + JSON."""
    body = b"\xef\xbb\xbf" + json.dumps(doc, ensure_ascii=False).encode("utf-8")
    nb = name.encode()
    pad = b"\x00" * (-len(nb) % 4)
    return struct.pack("<i", len(nb)) + nb + pad + struct.pack("<i", len(body)) + body


EN = {"localeId": "en_US", "brands": {"b": {"itemFamilies": [{"systemId": "ef_corona", "name": "Corona"}]}},
      "items": {"crucian": {"systemId": "crucian", "name": "Gibel Carp", "description": "x"},
                "caddisfly": {"systemId": "caddisfly", "name": "Caddisfly"}}}
ZH = {"localeId": "zh_CN", "brands": {"b": {"itemFamilies": [{"systemId": "ef_corona", "name": "Corona"}]}},
      "items": {"crucian": {"systemId": "crucian", "name": "银鲫"},
                "caddisfly": {"systemId": "caddisfly", "name": "毛翅目幼虫"}}}


class ExtractClientNamesTests(unittest.TestCase):
    def setUp(self):
        self.blob = b"\x00" * 37 + textasset("zh_CN", ZH) + b"junk" + textasset("en_US", EN) + b"\x00" * 9

    def test_locale_document_reads_length_prefixed_json(self):
        self.assertEqual(locale_document(self.blob, "en_US")["items"]["crucian"]["name"], "Gibel Carp")
        with self.assertRaises(ValueError):
            locale_document(self.blob, "fr_FR")

    def test_collect_names_walks_nested_objects(self):
        self.assertEqual(collect_names(EN), {"ef_corona": "Corona", "crucian": "Gibel Carp", "caddisfly": "Caddisfly"})

    def test_build_maps_ids_and_chinese_names(self):
        out = build(self.blob)
        self.assertEqual(out["names"]["caddisfly"], "Caddisfly")
        self.assertEqual(out["zh2en"], {"毛翅目幼虫": "Caddisfly", "银鲫": "Gibel Carp"})  # identical names skipped


if __name__ == "__main__":
    unittest.main()
