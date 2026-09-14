#!/usr/bin/env python3
"""Cross-validate cdf2ms fixtures against the reference netCDF C library.

Development/CI helper only. cdf2ms itself has no Python dependency.

Usage:
    python3 scripts/crosscheck_netcdf4.py fixtures <dir>      # check Go-written files
    python3 scripts/crosscheck_netcdf4.py truth <file.cdf>... --out ground_truth.json
"""
from __future__ import annotations

import json
import math
import os
import sys

try:
    from netCDF4 import Dataset
    import numpy as np
except ImportError as exc:  # pragma: no cover
    sys.stderr.write(
        "netCDF4/numpy required (python3 -m venv .venv && .venv/bin/pip install netCDF4 numpy)\n"
    )
    raise SystemExit(2)


def mass_at(scan: int, point: int) -> float:
    return 50.0 + point * 0.7 + (scan % 17) * 0.01


def intensity_at(scan: int, point: int) -> float:
    return ((scan * 7919 + point * 104729) % 100000) * 1.5


def check_fixture(path: str) -> list[str]:
    errs: list[str] = []
    with Dataset(path) as ds:
        if "mass_values" not in ds.variables:
            return [f"{os.path.basename(path)}: mass_values missing"]
        rt = np.asarray(ds.variables["scan_acquisition_time"][:])
        pc = np.asarray(ds.variables["point_count"][:]) if "point_count" in ds.variables else None
        si = np.asarray(ds.variables["scan_index"][:]) if "scan_index" in ds.variables else None
        mv = ds.variables["mass_values"]
        iv = ds.variables["intensity_values"]
        mv.set_auto_maskandscale(False)
        iv.set_auto_maskandscale(False)
        packed = "scale_factor" in mv.ncattrs()
        scale = float(mv.getncattr("scale_factor")) if packed else 1.0
        offset = float(mv.getncattr("add_offset")) if packed else 0.0
        total = int(mv.shape[0])
        if si is None and pc is not None:
            si = np.concatenate([[0], np.cumsum(pc[:-1])])
        if si is None or pc is None:
            return [f"{os.path.basename(path)}: index/point_count missing"]
        base = int(si[0])
        for scan in range(len(pc)):
            start = int(si[scan]) - base
            count = int(pc[scan])
            if start + count > total:
                errs.append(f"{os.path.basename(path)} scan {scan}: slice out of range")
                continue
            got_mz = np.asarray(mv[start : start + count], dtype=np.float64)
            got_int = np.asarray(iv[start : start + count], dtype=np.float64)
            if packed:
                got_mz = got_mz * scale + offset
            want_mz = np.array([mass_at(scan, p) for p in range(count)])
            want_int = np.array([intensity_at(scan, p) for p in range(count)])
            # Packed storage quantises to the scale_factor, so the oracle may
            # differ by at most half a quantum (plus float32 round-off).
            mz_atol = max(1e-3, abs(scale) / 2.0 + 1e-6)
            int_atol = max(1.0, abs(float(iv.getncattr("scale_factor")) if "scale_factor" in iv.ncattrs() else 1.0) / 2.0 + 1e-6)
            if not np.allclose(got_mz, want_mz, rtol=1e-5, atol=mz_atol):
                errs.append(f"{os.path.basename(path)} scan {scan}: mz mismatch {got_mz[:4]} != {want_mz[:4]}")
            if not np.allclose(got_int, want_int, rtol=1e-4, atol=int_atol):
                errs.append(f"{os.path.basename(path)} scan {scan}: intensity mismatch {got_int[:4]} != {want_int[:4]}")
    return errs


def truth(path: str) -> dict:
    with Dataset(path) as ds:
        out: dict = {
            "file": os.path.basename(path),
            "format": getattr(ds, "file_format", None),
            "globals": {k: str(ds.getncattr(k)) for k in ds.ncattrs()},
            "dims": {d: len(v) for d, v in ds.dimensions.items()},
            "variables": {},
        }
        for name, var in ds.variables.items():
            out["variables"][name] = {
                "dtype": str(var.dtype),
                "shape": list(var.shape),
                "dims": list(var.dimensions),
                "attrs": {k: str(var.getncattr(k)) for k in var.ncattrs()},
            }
        if "point_count" in ds.variables:
            pc = np.asarray(ds.variables["point_count"][:])
            out["point_count"] = {"n": int(pc.size), "min": int(pc.min()), "max": int(pc.max()), "sum": int(pc.sum())}
        if "scan_index" in ds.variables:
            si = np.asarray(ds.variables["scan_index"][:])
            out["scan_index"] = {"first3": [int(x) for x in si[:3]], "last3": [int(x) for x in si[-3:]]}
        rt = None
        if "scan_acquisition_time" in ds.variables:
            rt = np.asarray(ds.variables["scan_acquisition_time"][:], dtype=np.float64).ravel()
            out["rt"] = {"n": int(rt.size), "min": float(rt.min()), "max": float(rt.max()),
                         "first3": [float(x) for x in rt[:3]]}
        if "mass_values" in ds.variables and "scan_index" in ds.variables and "point_count" in ds.variables:
            mv = ds.variables["mass_values"]
            iv = ds.variables["intensity_values"]
            mv.set_auto_maskandscale(True)
            iv.set_auto_maskandscale(True)
            si = np.asarray(ds.variables["scan_index"][:]).ravel()
            pc = np.asarray(ds.variables["point_count"][:]).ravel()
            base = int(si[0])
            scans = []
            for scan in (0, 1, min(7, len(pc) - 1), len(pc) - 1):
                start = int(si[scan]) - base
                count = int(pc[scan])
                mz = np.asarray(mv[start : start + min(count, 8)], dtype=np.float64)
                it = np.asarray(iv[start : start + min(count, 8)], dtype=np.float64)
                full_mz = np.asarray(mv[start : start + count], dtype=np.float64)
                full_it = np.asarray(iv[start : start + count], dtype=np.float64)
                scans.append({
                    "scan": int(scan),
                    "start": start,
                    "count": count,
                    "mz_head": [float(x) for x in mz],
                    "int_head": [float(x) for x in it],
                    "tic": float(full_it.sum()),
                    "min_mz": float(full_mz.min()) if count else None,
                    "max_mz": float(full_mz.max()) if count else None,
                    "base_peak_mz": float(full_mz[int(np.argmax(full_it))]) if count else None,
                    "base_peak_int": float(full_it.max()) if count else None,
                })
            out["scans"] = scans
            if rt is not None:
                out["rt"]["scan_values"] = [float(rt[s["scan"]]) for s in scans]
    return out


def main() -> int:
    if len(sys.argv) < 3:
        __doc__ and print(__doc__)
        return 2
    mode = sys.argv[1]
    if mode == "fixtures":
        bad = 0
        for name in sorted(os.listdir(sys.argv[2])):
            if not name.endswith((".cdf", ".CDF")):
                continue
            errs = check_fixture(os.path.join(sys.argv[2], name))
            if errs:
                bad += 1
                for e in errs:
                    print("FAIL", e)
            else:
                print("OK  ", name)
        return 1 if bad else 0
    if mode == "truth":
        args = [a for a in sys.argv[2:] if not a.startswith("--")]
        out_path = None
        if "--out" in sys.argv:
            out_path = sys.argv[sys.argv.index("--out") + 1]
            args = [a for a in args if a != out_path]
        data = [truth(p) for p in args]
        text = json.dumps(data, indent=1, sort_keys=True, allow_nan=False)
        if out_path:
            with open(out_path, "w") as fh:
                fh.write(text)
            print("wrote", out_path, len(data), "files")
        else:
            print(text)
        return 0
    print("unknown mode", mode)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
