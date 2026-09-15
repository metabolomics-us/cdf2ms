#!/usr/bin/env python3
"""Recover the official mzXML 3.2 schema set into pkg/mzxml/testdata/schema.

The mzXML project's schema host (sashimi.sourceforge.net/schema_revision/...)
now returns 404 for every file, so the normative schemas are no longer
downloadable from their canonical location. This script fetches them from
archived snapshots of the official URLs, applies the two repairs that are needed
to load them offline, and verifies the result parses as an XML Schema.

Repairs (both are recorded in testdata/schema/README.md):
  1. xs:include points at the dead absolute URLs; they are rewritten to relative
     file names so the set is self-contained.
  2. general_types_1.0.xsd ships WITHOUT a targetNamespace, yet mzXML_3.2.xsd
     references its types through the cff: prefix (the mzXML 3.2 namespace), so
     cff:namevalueType / cff:ontologyEntryType cannot resolve as published. The
     targetNamespace is added, which is what the referencing schema already
     assumes.

Neither repair changes what a mzXML 3.2 document must look like.

Only the four files the include chain actually needs are recovered:
mzXML_idx_3.2.xsd -> mzXML_3.2.xsd -> {separation_technique_1.0.xsd,
general_types_1.0.xsd}. The release directory also carries
mzXML_idx_separation_1.0.xsd, an illustrative combination that pulls in
column_separation_1.0.xsd; nothing in the validation chain includes it, and the
archive does not serve that companion file, so it is deliberately left out to
keep the recovered set closed and loadable.

Usage: scripts/fetch_mzxml_schema.py [--outdir DIR] [--offline]
"""
import argparse
import os
import re
import sys
import urllib.request
import xml.etree.ElementTree as ET

NS = "http://sashimi.sourceforge.net/schema_revision/mzXML_3.2/"

# Official URL -> archived snapshot timestamp (verified to return the real file).
SNAPSHOTS = {
    "mzXML_3.2.xsd": "20120210053846",
    "mzXML_idx_3.2.xsd": "20120211084304",
    "general_types_1.0.xsd": "20120211084259",
    "separation_technique_1.0.xsd": "20120211072657",
}


def fetch(name, ts):
    url = ("https://web.archive.org/web/%sid_/%s%s" % (ts, NS, name))
    last = None
    for _ in range(5):  # the archive is intermittently unavailable
        try:
            with urllib.request.urlopen(url, timeout=120) as r:
                body = r.read()
            if body.lstrip().startswith(b"<?xml"):
                return body.decode("utf-8")
            last = "snapshot returned a non-XML payload"
        except Exception as exc:  # noqa: BLE001
            last = str(exc)
        import time
        time.sleep(3)
    raise SystemExit("failed to fetch %s: %s" % (name, last))


def repair(name, text):
    # 1. self-contained includes
    text = re.sub(r'schemaLocation="[^"]*?/([^"/]+\.xsd)"', r'schemaLocation="\1"', text)
    # 2. general_types_1.0.xsd is published without a targetNamespace
    if name == "general_types_1.0.xsd" and "targetNamespace" not in text:
        text = text.replace(
            '<xs:schema xmlns:xs="http://www.w3.org/2001/XMLSchema">',
            '<xs:schema targetNamespace="%s" xmlns:xs="http://www.w3.org/2001/XMLSchema">' % NS.rstrip("/"),
            1,
        )
        if "targetNamespace" not in text:
            raise SystemExit("could not add targetNamespace to general_types_1.0.xsd")
    return text


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    default_out = os.path.join(os.path.dirname(here), "pkg", "mzxml", "testdata", "schema")
    ap = argparse.ArgumentParser()
    ap.add_argument("--outdir", default=default_out)
    ap.add_argument("--offline", action="store_true",
                    help="only verify the files already in --outdir")
    args = ap.parse_args()
    os.makedirs(args.outdir, exist_ok=True)

    if not args.offline:
        for name, ts in SNAPSHOTS.items():
            text = repair(name, fetch(name, ts))
            with open(os.path.join(args.outdir, name), "w", encoding="utf-8") as fh:
                fh.write(text)
            print("recovered", name)

    for name in SNAPSHOTS:
        path = os.path.join(args.outdir, name)
        try:
            ET.parse(path)
        except Exception as exc:  # noqa: BLE001
            raise SystemExit("%s does not parse: %s" % (name, exc))
    try:
        from lxml import etree  # noqa: F401
    except ImportError:
        print("NOTE: lxml not installed; XMLSchema load check skipped")
        return 0
    from lxml import etree
    try:
        etree.XMLSchema(etree.parse(os.path.join(args.outdir, "mzXML_idx_3.2.xsd")))
    except Exception as exc:  # noqa: BLE001
        raise SystemExit("schema set does not load as a schema: %s" % exc)
    print("OK: mzXML_idx_3.2.xsd loads as a schema from", args.outdir)
    return 0


if __name__ == "__main__":
    sys.exit(main())
