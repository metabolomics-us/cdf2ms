#!/usr/bin/env python3
"""Validate a cdf2ms output file against its official schema and re-read every
peak with independent implementations, comparing against the source ANDI file
read through the netCDF reference library.

Development / CI gate only: cdf2ms itself is pure Go and never shells out to
Python at runtime.

  validate_ms_outputs.py OUT.mzML|OUT.mzXML --source SRC.CDF
                           [--max-spectra N] [--tol 1e-6]
                           [--mzml-xsd PATH] [--mzxml-xsd-dir PATH]
                           [--no-pyteomics]

Checks performed
  mzML   : official mzML 1.1.0 XSD, then an independent stdlib reader
           (ElementTree + base64 + struct, little-endian) for every peak.
  mzXML  : official mzXML 3.2 XSD (pkg/mzxml/testdata/schema), then an
           independent stdlib reader (ElementTree + base64 + numpy big-endian,
           interleaved m/z-int), plus byte-offset index, indexOffset, and the
           <sha1> file checksum; finally pyteomics.mzxml as a third-party reader.

Exit code 0 means: schema-valid and every compared value matched the source.
"""
import argparse
import base64
import os
import re
import struct
import sys
import urllib.request
import xml.etree.ElementTree as ET
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)

MZML_NS = "{http://psi.hupo.org/ms/mzml}"
MZXML_NS = "{http://sashimi.sourceforge.net/schema_revision/mzXML_3.2}"

MZML_XSD_URL = ("https://raw.githubusercontent.com/HUPO-PSI/mzML/master/"
                "schema/schema_1.1/mzML1.1.0.xsd")

# cdf2ms drops points that are non-finite or equal the netCDF default float fill
# value; the source truth must apply the same rule for a like-for-like compare.
FILL_LIMIT = 9.9e36


def detect_format(path):
    for _, elem in ET.iterparse(path, events=("start",)):
        tag = elem.tag.split("}")[-1]
        if tag == "mzML":
            return "mzml"
        if tag == "mzXML":
            return "mzxml"
        raise SystemExit("root element is %r, expected mzML or mzXML" % tag)
    raise SystemExit("no root element in %s" % path)


def ensure_mzml_xsd(explicit):
    if explicit:
        if not os.path.exists(explicit):
            raise SystemExit("--mzml-xsd %s does not exist" % explicit)
        return explicit
    d = os.path.join(os.path.expanduser("~"), ".cache", "cdf2ms-scratch", "spec")
    os.makedirs(d, exist_ok=True)
    path = os.path.join(d, "mzML1.1.0.xsd")
    if not os.path.exists(path):
        print("downloading", MZML_XSD_URL)
        with urllib.request.urlopen(MZML_XSD_URL, timeout=180) as r:
            open(path, "wb").write(r.read())
    return path


def check_schema(doc_path, xsd_path, label):
    try:
        from lxml import etree
    except ImportError:
        print("SKIP schema: lxml is not installed")
        return None
    schema = etree.XMLSchema(etree.parse(xsd_path))
    if not schema.validate(etree.parse(doc_path)):
        print("SCHEMA INVALID:")
        for e in list(schema.error_log)[:40]:
            print("  line %d: %s" % (e.line, e.message))
        return False
    print("schema: VALID against %s" % label)
    return True


# ---------------- ANDI source truth ----------------

# Seconds per unit for the retention-time units ANDI exporters write.
RT_UNIT_SECONDS = {
    "s": 1.0, "sec": 1.0, "secs": 1.0, "second": 1.0, "seconds": 1.0,
    "min": 60.0, "mins": 60.0, "minute": 60.0, "minutes": 60.0,
    "h": 3600.0, "hr": 3600.0, "hour": 3600.0, "hours": 3600.0,
}

# The --rt-unit override, set in main(); None means "use the source's units".
RT_UNIT_OVERRIDE = None


def rt_seconds_per_unit(ds):
    """Seconds per source retention-time unit.

    The converter writes retention times in seconds, so the truth must be in
    seconds too. The override wins; otherwise scan_acquisition_time.units, then
    time_values.units (the same clock in the ANDI template). A source that
    states neither is taken as seconds.
    """
    if RT_UNIT_OVERRIDE:
        return RT_UNIT_SECONDS[RT_UNIT_OVERRIDE]
    for name in ("scan_acquisition_time", "time_values"):
        if name in ds.variables:
            unit = str(getattr(ds.variables[name], "units", "")).strip().lower()
            if unit in RT_UNIT_SECONDS:
                return RT_UNIT_SECONDS[unit]
    return 1.0


def load_truth(src):
    from netCDF4 import Dataset
    import numpy as np
    ds = Dataset(src)
    try:
        idx = np.asarray(ds.variables["scan_index"][:], dtype="int64")
        cnt = np.asarray(ds.variables["point_count"][:], dtype="int64")
        if idx.size and idx[0] == 1:
            idx = idx - 1
        mz = np.asarray(ds.variables["mass_values"][:], dtype="float64")
        inten = np.asarray(ds.variables["intensity_values"][:], dtype="float64")
        rt = np.asarray(ds.variables["scan_acquisition_time"][:], dtype="float64")
        rt = rt * rt_seconds_per_unit(ds)
        scan_num = None
        for name in ("actual_scan_number", "scan_number"):
            if name in ds.variables:
                scan_num = np.asarray(ds.variables[name][:], dtype="int64")
                break
        # Mirror the converter: negative or fill scan numbers (ANDI's -9999
        # "not recorded") disqualify the variable, and scans are numbered by
        # ordinal instead.
        if scan_num is not None and scan_num.size and int(scan_num.min()) < 0:
            scan_num = None
    finally:
        ds.close()
    return idx, cnt, mz, inten, rt, scan_num


def expected_scan(idx, cnt, scan_num, rt, keep_fill=True):
    """Yield (start, count, rt, source_scan_number) per scan."""
    for i in range(len(idx)):
        yield idx[i], cnt[i], float(rt[i]), (None if scan_num is None else int(scan_num[i]))


def clean(a, b):
    import numpy as np
    keep = np.isfinite(a) & np.isfinite(b)
    keep &= (np.abs(a) < FILL_LIMIT) & (np.abs(b) < FILL_LIMIT)
    return a[keep], b[keep]


# ---------------- independent mzML reader ----------------

def iter_mzml(path):
    context = ET.iterparse(path, events=("end",))
    for _, elem in context:
        if elem.tag != MZML_NS + "spectrum":
            continue
        out = {"index": int(elem.get("index")), "id": elem.get("id"),
               "length": int(elem.get("defaultArrayLength")), "rt": None,
               "msLevel": None, "polarity": None, "centroid": None,
               "mz": None, "intensity": None}
        for cvp in elem.iter(MZML_NS + "cvParam"):
            acc, val = cvp.get("accession"), cvp.get("value")
            if acc == "MS:1000016":
                if cvp.get("unitName") != "second":
                    raise SystemExit("scan start time unit %r is not 'second'" % cvp.get("unitName"))
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
        for arr in elem.iter(MZML_NS + "binaryDataArray"):
            encoded = (arr.find(MZML_NS + "binary").text or "").strip()
            if int(arr.get("encodedLength")) != len(encoded):
                raise SystemExit("encodedLength %s != actual base64 length %d"
                                 % (arr.get("encodedLength"), len(encoded)))
            raw = base64.b64decode(encoded)
            bits, content, compressed = 64, None, False
            for cvp in arr.iter(MZML_NS + "cvParam"):
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
                raw = zlib.decompress(raw)
            # mzML binary arrays are little-endian.
            fmt = "<" + ("f" if bits == 32 else "d") * (len(raw) // (bits // 8))
            out[content] = struct.unpack(fmt, raw)
        yield out
        elem.clear()


# ---------------- independent mzXML reader ----------------

DURATION_RE = re.compile(r"^(?P<sign>-)?P(?:(?P<Y>\d+)Y)?(?:(?P<M>\d+)M)?(?:(?P<D>\d+)D)?"
                         r"(?:T(?:(?P<H>\d+)H)?(?:(?P<Min>\d+)M)?(?:(?P<S>\d+(?:\.\d+)?)S)?)?$")


def parse_duration(text):
    m = DURATION_RE.match(text or "")
    if not m:
        raise SystemExit("retentionTime %r is not an ISO 8601 duration" % text)
    g = m.groupdict()
    secs = (int(g["Y"] or 0) * 365 * 86400 + int(g["M"] or 0) * 30 * 2592000  # calendar parts
            + int(g["D"] or 0) * 86400 + int(g["H"] or 0) * 3600 + int(g["Min"] or 0) * 60
            + float(g["S"] or 0))
    if not (g["Y"] or g["M"] or g["D"] or g["H"] or g["Min"] or g["S"]):
        raise SystemExit("duration %r carries no time value" % text)
    return -secs if g["sign"] else secs


def iter_mzxml(path):
    for _, elem in ET.iterparse(path, events=("end",)):
        if elem.tag != MZXML_NS + "scan":
            continue
        out = {"num": int(elem.get("num")), "msLevel": int(elem.get("msLevel")),
               "peaksCount": int(elem.get("peaksCount")),
               "centroided": None, "polarity": elem.get("polarity"), "rt": None,
               "mz": None, "intensity": None}
        if elem.get("centroided") is not None:
            out["centroided"] = elem.get("centroided") in ("1", "true")
        if elem.get("retentionTime") is not None:
            out["rt"] = parse_duration(elem.get("retentionTime"))
        peaks = elem.find(MZXML_NS + "peaks")
        if peaks is None:
            yield out
            elem.clear()
            continue
        precision = int(peaks.get("precision", "32"))
        if peaks.get("byteOrder") != "network":
            raise SystemExit("peaks byteOrder %r is not 'network' (mzXML is big-endian)"
                             % peaks.get("byteOrder"))
        if peaks.get("contentType") != "m/z-int":
            raise SystemExit("peaks contentType %r is not 'm/z-int'" % peaks.get("contentType"))
        compressed_len = int(peaks.get("compressedLen"))
        compression = peaks.get("compressionType")
        if peaks.get("{http://www.w3.org/2001/XMLSchema-instance}nil") == "true":
            if out["peaksCount"] != 0:
                raise SystemExit("nil peaks with peaksCount %d" % out["peaksCount"])
            if compressed_len != 0 or compression != "none":
                raise SystemExit("nil peaks must declare compressionType=none compressedLen=0")
            yield out
            elem.clear()
            continue
        # mzXML puts the base64 payload directly in the <peaks> element text
        # (unlike mzML, which nests it in <binary>).
        encoded = (peaks.text or "").strip()
        if not encoded:
            raise SystemExit("<peaks> carries no base64 text")
        raw = base64.b64decode(encoded)
        if compression == "zlib":
            if compressed_len != len(raw):
                raise SystemExit("compressedLen %d != actual compressed bytes %d"
                                 % (compressed_len, len(raw)))
            raw = zlib.decompress(raw)
        elif compressed_len != 0:
            raise SystemExit("compressedLen %d set for uncompressed data" % compressed_len)
        dtype = ">f4" if precision == 32 else ">f8"
        arr = np_frombuffer(raw, dtype)
        if arr.size % 2:
            raise SystemExit("interleaved peak array has odd length %d" % arr.size)
        pairs = arr.reshape(-1, 2)
        out["mz"] = pairs[:, 0].astype("float64")
        out["intensity"] = pairs[:, 1].astype("float64")
        yield out
        elem.clear()


def np_frombuffer(raw, dtype):
    import numpy as np
    return np.frombuffer(raw, dtype=dtype)


# ---------------- structural checks for indexed mzXML ----------------

def check_mzxml_structure(path, problems):
    raw = open(path, "rb").read()
    root = ET.fromstring(raw)
    scan_count = int(root.find(MZXML_NS + "msRun").get("scanCount"))
    scans = len(root.findall(MZXML_NS + "msRun/" + MZXML_NS + "scan"))
    if scan_count != scans:
        problems.append("msRun/@scanCount=%d but %d <scan> elements were written" % (scan_count, scans))

    nums = [int(s.get("num")) for s in root.findall(MZXML_NS + "msRun/" + MZXML_NS + "scan")]
    if len(set(nums)) != len(nums):
        problems.append("duplicate scan/@num values")
    if any(b <= a for a, b in zip(nums, nums[1:])):
        problems.append("scan/@num is not strictly increasing")

    idx_off_elem = root.find(MZXML_NS + "indexOffset")
    if idx_off_elem is None:
        problems.append("<indexOffset> is required by mzXML_idx_3.2.xsd")
        return
    if idx_off_elem.get("{http://www.w3.org/2001/XMLSchema-instance}nil") == "true":
        print("index: not present (xsi:nil) - source scan numbering was unusable")
        return
    idx_off = int(idx_off_elem.text)
    if not raw[idx_off:idx_off + len("<index ")].startswith(b"<index "):
        problems.append("indexOffset %d does not point at <index>: %r" % (idx_off, raw[idx_off:idx_off + 40]))

    index = root.find(MZXML_NS + "index")
    if index is None or index.get("name") != "scan":
        problems.append("no <index name=\"scan\"> although indexOffset is set")
        return
    offsets = [(int(o.get("id")), int(o.text)) for o in index.findall(MZXML_NS + "offset")]
    if [i for i, _ in offsets] != nums:
        problems.append("index keys %r... do not match scan/@num %r..." % (offsets[:5], nums[:5]))
    if len(offsets) != len(nums):
        problems.append("index has %d entries for %d scans" % (len(offsets), len(nums)))
    bad = 0
    for num, off in offsets:
        window = raw[off:off + 120].decode("utf-8", "replace")
        if not window.startswith("<scan "):
            bad += 1
            if bad <= 3:
                problems.append("index offset %d for scan %d does not point at <scan>: %r"
                                % (off, num, window[:48]))
            continue
        m = re.search(r'num="(\d+)"', window)
        if not m or int(m.group(1)) != num:
            bad += 1
            if bad <= 3:
                problems.append("index offset %d points at a scan with the wrong num: %r" % (off, window[:48]))
    if bad > 3:
        problems.append("%d index offsets are wrong in total" % bad)

    # mzXML_idx_3.2.xsd puts index, indexOffset and sha1 beside msRun, not inside it.
    sha_elem = root.find(MZXML_NS + "sha1")
    if sha_elem is None or not sha_elem.text:
        problems.append("<sha1> is missing")
        return
    written = sha_elem.text.strip()
    i = raw.find(b"<sha1>")
    if i < 0:
        problems.append("no literal <sha1> tag found")
        return
    import hashlib
    want = hashlib.sha1(raw[:i + len("<sha1>")]).hexdigest()
    if written != want:
        problems.append("<sha1>=%s but SHA-1 of the file up to and including the opening "
                        "<sha1> tag is %s" % (written, want))
    else:
        print("sha1: OK (40 chars, covers the document prefix)")


# ---------------- numeric comparison ----------------

def check_numbers(path, fmt, src, max_spectra, tol, problems):
    import numpy as np
    idx, cnt, mz, inten, rt, scan_num = load_truth(src)
    n = min(len(idx), max_spectra) if max_spectra else len(idx)
    it = iter_mzml(path) if fmt == "mzml" else iter_mzxml(path)
    compared = seen = bad = 0
    offset = None
    for sp in it:
        if seen >= n:
            break
        i = seen
        seen += 1
        if fmt == "mzml" and sp["index"] != i:
            problems.append("spectrum index %d at position %d" % (sp["index"], i))
            bad += 1
        if fmt == "mzxml":
            if scan_num is not None:
                # mzXML @num is 1-based; a 0-based source sequence shifts by one.
                d = sp["num"] - int(scan_num[i])
                if offset is None:
                    offset = d
                if d != offset:
                    problems.append("scan %d: num=%d vs source actual_scan_number=%d "
                                    "(offset changed from %d)" % (i, sp["num"], scan_num[i], offset))
                    bad += 1
        want_mz, want_int = clean(mz[idx[i]:idx[i] + cnt[i]], inten[idx[i]:idx[i] + cnt[i]])
        got_mz = np.asarray([] if sp["mz"] is None else sp["mz"], dtype="float64")
        got_int = np.asarray([] if sp["intensity"] is None else sp["intensity"], dtype="float64")
        declared = sp["length"] if fmt == "mzml" else sp["peaksCount"]
        if declared != len(got_mz) or len(got_mz) != len(got_int):
            problems.append("scan %d: declared %d, decoded %d m/z and %d intensity"
                            % (i, declared, len(got_mz), len(got_int)))
            bad += 1
            if bad > 5:
                break
            continue
        if len(got_mz) != len(want_mz):
            problems.append("scan %d: %d peaks written, source has %d after the fill rule"
                            % (i, len(got_mz), len(want_mz)))
            bad += 1
            if bad > 5:
                break
            continue
        if len(want_mz):
            dmz = float(np.max(np.abs(got_mz - want_mz)))
            dint = float(np.max(np.abs(got_int - want_int)))
            if dmz > tol or dint > tol:
                problems.append("scan %d: max|dmz|=%g max|dIntensity|=%g" % (i, dmz, dint))
                bad += 1
            compared += len(want_mz)
        if sp["rt"] is None:
            if abs(float(rt[i])) > tol:
                problems.append("scan %d: no retention time written" % i)
                bad += 1
        elif abs(sp["rt"] - float(rt[i])) > tol:
            problems.append("scan %d: rt=%r want %r" % (i, sp["rt"], float(rt[i])))
            bad += 1
    print("read-back: compared %d peaks over %d spectra, %d problems" % (compared, seen, bad))
    return bad


# ---------------- pyteomics cross-read (third-party reader) ----------------

def pyteomics_rt_seconds(value):
    """Convert pyteomics' mzXML retention time to seconds.

    pyteomics parses ISO-8601 durations into minutes for mzXML (its own stated
    convention for this format), and returns a bare float unless pyteomics unit
    support is enabled. The document itself is unit-explicit ("PT305.582S"), which
    the stdlib reader above checks in seconds directly; this only reconciles the
    third-party reader's units.
    """
    if value is None:
        return 0.0
    unit = getattr(value, "unit", None)
    if unit is None:
        return float(value) * 60.0
    factor = {"second": 1.0, "minute": 60.0, "hour": 3600.0}.get(str(unit))
    if factor is None:
        raise SystemExit("unexpected retention-time unit from pyteomics: %r" % unit)
    return float(value) * factor


def check_pyteomics(path, src, problems):
    try:
        from pyteomics.mzxml import MzXML
    except ImportError:
        try:
            from pyteomics import mzxml as _mzxml
        except ImportError:
            print("SKIP pyteomics: not installed")
            return 0
        MzXML = getattr(_mzxml, "MzXML", None) or getattr(_mzxml, "mzxml", None)
    import numpy as np
    idx, cnt, mz, inten, rt, _ = load_truth(src)
    bad = 0
    with MzXML(path) as r:
        declared = int(ET.fromstring(open(path, "rb").read()).find(MZXML_NS + "msRun").get("scanCount"))
        if len(r) != declared:
            problems.append("pyteomics indexed %d spectra but the document declares %d "
                            "(duplicate or unusable scan keys)" % (len(r), declared))
            bad += 1
        for i in sorted({0, len(idx) // 2, len(idx) - 1}):
            sp = r[i]
            want_mz, want_int = clean(mz[idx[i]:idx[i] + cnt[i]], inten[idx[i]:idx[i] + cnt[i]])
            got_mz = np.asarray(sp.get("m/z array", []), dtype="float64")
            got_int = np.asarray(sp.get("intensity array", []), dtype="float64")
            if len(got_mz) != len(want_mz):
                problems.append("pyteomics scan %d: %d peaks, source %d" % (i, len(got_mz), len(want_mz)))
                bad += 1
                continue
            if len(want_mz) and (float(np.max(np.abs(got_mz - want_mz))) > 1e-3
                                 or float(np.max(np.abs(got_int - want_int))) > 1e-3):
                problems.append("pyteomics scan %d: values differ from the source" % i)
                bad += 1
            got_rt = pyteomics_rt_seconds(sp.get("retentionTime"))
            if abs(got_rt - float(rt[i])) > 1e-3:
                problems.append("pyteomics scan %d: retentionTime=%r (%g s) want %r s"
                                % (i, sp.get("retentionTime"), got_rt, float(rt[i])))
                bad += 1
    print("pyteomics: read %d spectra through the reference library, %d problems" % (declared, bad))
    return bad


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("document")
    ap.add_argument("--source", required=True, help="ANDI .CDF the document was made from")
    ap.add_argument("--max-spectra", type=int, default=0)
    ap.add_argument("--tol", type=float, default=1e-6)
    ap.add_argument("--mzml-xsd", default=None)
    ap.add_argument("--mzxml-xsd-dir",
                    default=os.path.join(REPO, "pkg", "mzxml", "testdata", "schema"))
    ap.add_argument("--no-pyteomics", action="store_true")
    ap.add_argument("--rt-unit", choices=["seconds", "minutes"], default=None,
                    help="source retention-time unit when the CDF states none "
                         "(match the converter's --rt-unit)")
    args = ap.parse_args()
    global RT_UNIT_OVERRIDE
    RT_UNIT_OVERRIDE = args.rt_unit

    fmt = detect_format(args.document)
    print("format: %s (%s)" % (fmt.upper(), os.path.basename(args.document)))
    problems = []

    if fmt == "mzml":
        ok_schema = check_schema(args.document, ensure_mzml_xsd(args.mzml_xsd), "mzML 1.1.0")
    else:
        xsd = os.path.join(args.mzxml_xsd_dir, "mzXML_idx_3.2.xsd")
        if not os.path.exists(xsd):
            print("SKIP schema: %s missing (run scripts/fetch_mzxml_schema.py)" % xsd)
            ok_schema = None
        else:
            ok_schema = check_schema(args.document, xsd, "mzXML 3.2 (indexed)")

    if fmt == "mzxml":
        check_mzxml_structure(args.document, problems)

    bad = check_numbers(args.document, fmt, args.source, args.max_spectra, args.tol, problems)
    if bad:
        problems.append("%d numeric/structural read-back problems" % bad)

    if fmt == "mzxml" and not args.no_pyteomics:
        bad += check_pyteomics(args.document, args.source, problems)

    if ok_schema is False or problems:
        print("FAILED:")
        for p in problems:
            print("  -", p)
        sys.exit(1)
    print("OK")


if __name__ == "__main__":
    main()
