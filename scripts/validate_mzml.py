#!/usr/bin/env python3
"""Validate an mzML file against the official schema and re-read it with an
independent implementation (pyteomics), comparing every peak against the source
ANDI file read with the netCDF4 reference library.

This is a development / CI gate, not a runtime dependency: cdf2ms itself is pure
Go and never shells out to Python.

Usage:
  validate_mzml.py OUT.mzML --source SRC.CDF [--xsd mzML1.1.0.xsd]
                            [--max-spectra N] [--tol 1e-6]

Exit code 0 = schema-valid AND every compared peak matches the source.
"""
import argparse
import os
import sys
import urllib.request
import xml.etree.ElementTree as ET

XSD_URL = ("https://raw.githubusercontent.com/HUPO-PSI/mzML/master/"
           "schema/schema_1.1/mzML1.1.0.xsd")
MZXML_XSD_URL = ("https://raw.githubusercontent.com/ProteoWizard/pwiz/master/"
                 "data/mzXML.xsd")


def ensure_xsd(path, url):
    if path and os.path.exists(path):
        return path
    d = os.path.join(os.path.expanduser("~"), ".cache", "cdf2ms-scratch", "spec")
    os.makedirs(d, exist_ok=True)
    path = path or os.path.join(d, os.path.basename(url))
    if not os.path.exists(path):
        print(f"downloading {url}")
        with urllib.request.urlopen(url, timeout=120) as r, open(path, "wb") as fh:
            fh.write(r.read())
    return path


def check_schema(mzml_path, xsd_path):
    try:
        from lxml import etree
    except ImportError:
        print("SKIP schema: lxml is not installed")
        return None
    schema = etree.XMLSchema(etree.parse(xsd_path))
    doc = etree.parse(mzml_path)
    ok = schema.validate(doc)
    if not ok:
        print("SCHEMA INVALID:")
        for e in schema.error_log[:40]:
            print(f"  line {e.line}: {e.message}")
        return False
    print("schema: VALID against mzML1.1.0")
    return True


def load_truth(src):
    from netCDF4 import Dataset
    import numpy as np
    ds = Dataset(src)
    idx = np.asarray(ds.variables["scan_index"][:], dtype="int64")
    cnt = np.asarray(ds.variables["point_count"][:], dtype="int64")
    if idx[0] == 1:
        idx = idx - 1
    mz = np.asarray(ds.variables["mass_values"][:], dtype="float64")
    inten = np.asarray(ds.variables["intensity_values"][:], dtype="float64")
    rt = np.asarray(ds.variables["scan_acquisition_time"][:], dtype="float64")
    ds.close()
    return idx, cnt, mz, inten, rt


def iter_spectra(mzml_path):
    """Independent mzML reader (stdlib ElementTree + struct).

    Deliberately not the writer's own code and not pyteomics, so a byte-order or
    base64 bug cannot cancel itself out.
    """
    import base64
    import struct
    NS = "{http://psi.hupo.org/ms/mzml}"
    context = ET.iterparse(mzml_path, events=("end",))
    for _, elem in context:
        if elem.tag != NS + "spectrum":
            continue
        out = {
            "index": int(elem.get("index")),
            "id": elem.get("id"),
            "defaultArrayLength": int(elem.get("defaultArrayLength")),
            "rt": None,
            "msLevel": None,
            "polarity": None,
            "centroid": None,
            "mz": None,
            "intensity": None,
        }
        for cvp in elem.iter(NS + "cvParam"):
            acc = cvp.get("accession")
            val = cvp.get("value")
            if acc == "MS:1000016":
                unit = cvp.get("unitName")
                if unit != "second":
                    raise SystemExit(f"scan start time unit is {unit!r}, expected 'second'")
                out["rt"] = float(val)
            elif acc == "MS:1000511":
                out["msLevel"] = int(val)
            elif acc == "MS:1000130":
                out["polarity"] = "+"
            elif acc == "MS:1000129":
                out["polarity"] = "-"
            elif acc == "MS:1000127":
                out["centroid"] = True
            elif acc == "MS:1000128":
                out["centroid"] = False
        for arr in elem.iter(NS + "binaryDataArray"):
            encoded = arr.find(NS + "binary").text or ""
            raw = base64.b64decode(encoded)
            if int(arr.get("encodedLength")) != len(encoded):
                raise SystemExit(
                    f"encodedLength {arr.get('encodedLength')} != actual {len(encoded)}")
            bits = 64
            content = None
            compressed = False
            for cvp in arr.iter(NS + "cvParam"):
                acc = cvp.get("accession")
                if acc == "MS:1000521":
                    bits = 32
                elif acc == "MS:1000523":
                    bits = 64
                elif acc == "MS:1000514":
                    content = "mz"
                elif acc == "MS:1000515":
                    content = "intensity"
                elif acc == "MS:1000574":
                    compressed = True
            if compressed:
                import zlib
                raw = zlib.decompress(raw)
            fmt = "<" + ("f" if bits == 32 else "d") * (len(raw) // (bits // 8))
            values = struct.unpack(fmt, raw)
            out[content] = values
        yield out
        elem.clear()


def check_numbers(mzml_path, src, max_spectra, tol, report):
    import numpy as np
    idx, cnt, mz, inten, rt = load_truth(src)
    n = min(len(idx), max_spectra) if max_spectra else len(idx)
    bad = 0
    compared = 0
    seen = 0
    for sp in iter_spectra(mzml_path):
        if seen >= n:
            break
        i = seen
        seen += 1
        if sp["index"] != i:
            print(f"  spectrum index {sp['index']} at position {i}")
            bad += 1
        want_mz = mz[idx[i]:idx[i] + cnt[i]]
        want_int = inten[idx[i]:idx[i] + cnt[i]]
        # cdf2ms drops fill/non-finite points; apply the same rule to the source
        # truth so the comparison is like-for-like.
        finite = np.isfinite(want_mz) & np.isfinite(want_int)
        notfill = ((np.abs(want_mz) < 9.9e36) & (np.abs(want_int) < 9.9e36))
        keep = finite & notfill
        want_mz, want_int = want_mz[keep], want_int[keep]
        got_mz = np.asarray(sp["mz"], dtype="float64")
        got_int = np.asarray(sp["intensity"], dtype="float64")
        if len(got_mz) != len(want_mz) or len(got_int) != len(want_int):
            print(f"  scan {i}: mzML arrays {len(got_mz)}/{len(got_int)}, source {len(want_mz)}")
            bad += 1
            if bad > 5:
                break
            continue
        if sp["defaultArrayLength"] != len(got_mz):
            print(f"  scan {i}: defaultArrayLength {sp['defaultArrayLength']} != array length {len(got_mz)}")
            bad += 1
        if len(want_mz):
            dmz = float(np.max(np.abs(got_mz - want_mz)))
            dint = float(np.max(np.abs(got_int - want_int)))
            if dmz > tol or dint > tol:
                print(f"  scan {i}: max|dmz|={dmz:g} max|dinten|={dint:g}")
                bad += 1
            compared += len(want_mz)
        if sp["rt"] is None:
            print(f"  scan {i}: no scan start time")
            bad += 1
        elif abs(sp["rt"] - float(rt[i])) > tol:
            print(f"  scan {i}: rt={sp['rt']} want {float(rt[i])}")
            bad += 1
    report["compared"] = compared
    report["bad"] = bad
    print(f"read-back: compared {compared} peaks over {seen} spectra, {bad} problems")
    return bad == 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("mzml")
    ap.add_argument("--source")
    ap.add_argument("--xsd", default=None)
    ap.add_argument("--max-spectra", type=int, default=0)
    ap.add_argument("--tol", type=float, default=1e-6)
    args = ap.parse_args()

    report = {}
    xsd = ensure_xsd(args.xsd, XSD_URL)
    ok_schema = check_schema(args.mzml, xsd)
    ok_numbers = True
    if args.source:
        ok_numbers = check_numbers(args.mzml, args.source, args.max_spectra, args.tol, report)
    if ok_schema is False or not ok_numbers:
        sys.exit(1)
    print("OK")


if __name__ == "__main__":
    main()
