# Compatibility

`cdf2ms` converts ANDI/MS (ASTM E1205/E1947) NetCDF files to mzML and mzXML. This
document records what it accepts, what it produces, and how that was verified
against real vendor data and the official schemas.

## Source: ANDI/MS NetCDF

### Containers

| Container | Read | Convert | Notes |
|-----------|------|---------|-------|
| NetCDF classic (CDF-1, 32-bit offset) | yes | yes | pure-Go reader, no cgo |
| NetCDF 64-bit offset (CDF-2) | yes | yes | |
| NetCDF 64-bit data (CDF-5) | yes | yes | |
| NetCDF-4 / HDF5 | detected | no | reported as `NC_UNSUPPORTED_ENCODING`; TIC-only files that happen to be NetCDF4 are described but not converted |

The NetCDF reader was cross-checked against the `netCDF4` reference library on
real files and on synthetic CDF-1/2/5 fixtures.

### ANDI layout features handled

- required role variables (`mass_values`, `intensity_values`,
  `scan_acquisition_time`, `scan_index`, `point_count`, ...) via exact names and
  documented aliases;
- scan plans derived from `scan_index` + `point_count`, `scan_index` only,
  `point_count` only, or an unambiguous one-peak-per-scan layout — each with a
  recorded provenance code;
- retention-time units declared in the variable's `units` attribute, the
  point-level `time_values` unit, or the file-global `units` attribute that
  ASTM E1482 general data defines for the chromatographic axis (Agilent writes
  `units = "Seconds"` there while leaving `scan_acquisition_time` bare), plus a
  decisive magnitude heuristic when none of them is present or usable; an
  ambiguous unit refuses to convert (`--rt-unit` resolves it). A stated unit
  always outranks the heuristic, and each is recorded with its origin
  (`var:`, `global:` or `inference:`). Where two attributes disagree the more
  specific one wins and `ANDI_RT_UNIT_CONFLICT` is reported;
- packed integer data unpacked per the netCDF convention, with the
  `ANDI_PACKING_APPLIED` warning;
- 0-based and 1-based `scan_index`; non-monotonic or overlapping scans are
  detected and reported rather than silently renumbered;
- zero-point scans (emitted as empty spectra), non-UTF8 metadata (sanitised and
  reported), and mixed MS levels; and
- vendor missing-data markers in optional scan metadata (Agilent writes `-9999`
  for an undefined scan number or inter-scan delay): read as absence, never
  published as a value, and reported once per file rather than per scan.

### Validated against

Real public corpora used during development (see `scripts/validate_ms_outputs.py`
and the CI `real-corpus` job):

- `gc01_0812_066.cdf` (Agilent GC-MS)
- `tnodeco-gcms1.CDF`, `tnodeco-gcms2.CDF`
- `lcms_sim_gdgts_1.CDF`
- `mzkit-200ppm-scan.CDF`
- `mzkit-08GB.cdf` (HDF5/NetCDF4, TIC-only — detected, not converted)
- `Warden0001_MX05001119_posPM_40-6660-458-0040_1.cdf` (Agilent GC/MS, centroid;
  14 989 scans, 4 441 571 points, SHA-256 `6551244f…7094ba`) — a file a user
  reported as unloadable in mzMine: `scan_acquisition_time` carries no
  attributes at all and the 0.0588 s scan spacing fits both clocks, so the unit
  comes only from the file-global `units = "Seconds"`. Converted and verified
  peak-for-peak; the equivalent shape is reproduced by the `global-units`
  fixture variant.

## Output: mzML 1.1.0

- Written to the official **mzML 1.1.0 XSD** (fetched at dev/CI time from the
  canonical location; see `scripts/validate_ms_outputs.py`).
- **Binary arrays are little-endian**, as mzML requires.
- Controlled-vocabulary terms are emitted only when the term's accession/name
  pair exists in the current PSI-MS ontology (`pkg/mzml/cv.go`, pinned to
  `psi-ms.obo` version `4.1.261`). Terms absent from the current ontology (e.g.
  `base64 array`) are never emitted.
- The declared `spectrumList/@count` must equal the number of written spectra; a
  mismatch fails the file and preserves it at `*.partial`.
- Spectrum IDs always include `index=` so they are unique and ordered even when
  the source uses 0-based or duplicate scan numbers.

## Output: mzXML 3.2

- Written to the **official mzXML 3.2 schema** (recovered from Internet Archive
  snapshots of the canonical sashimi URLs, committed at
  `pkg/mzxml/testdata/schema/`; provenance and two schema repairs are documented
  in `pkg/mzxml/testdata/schema/README.md`).
- **Binary data is big-endian** (network order), interleaved `m/z-int`, matching
  the mzXML convention.
- The spectrum element is `<scan>` (mzXML's form; `<spectrum>` is mzML's). The
  indexed envelope is `<index>`/`<indexOffset>`/`<sha1>` beside `<msRun>` inside
  the root `<mzXML>`.
- `peaks` base64 payload is the element's own text (mzXML has no nested
  `<binary>` child, unlike mzML); `compressedLen` is the pre-base64 compressed
  byte length.
- `indexOffset` is schema-required, so a document without an index writes
  `xsi:nil="true"` rather than omitting it.
- `parentFile/@fileSha1` is the SHA-1 of the source (required by the schema; a
  missing source digest is computed in the same pass that computes SHA-256).
- Scan numbering is decided once on the first spectrum: source numbers when they
  are positive and strictly increasing, otherwise 1-based ordinals with a
  recorded warning. A 0-based source (e.g. `actual_scan_number` starting at 0)
  shifts the whole document to 1-based ordinals.

## Verification

Every produced document is checked three ways, so a buggy round-trip cannot
cancel itself out:

1. **Independent Go reader** (`pkg/validate`) re-parses the XML/base64/zlib and
   compares every spectrum's numbers against the re-read source.
2. **Official schema** (lxml, dev/CI only) validates the document against the
   official mzML 1.1.0 / mzXML 3.2 XSD.
3. **Reference readers** (dev/CI only): `pyteomics` and a stdlib/numpy reader
   (mzML little-endian `struct`, mzXML big-endian `>f4`/`>f8`) cross-check
   numbers, byte offsets, the `<sha1>` prefix, and the `<index>` landing on
   `<scan`.

`cdf2ms` itself is pure Go and never shells out to Python; the Python toolchain
is a development and CI gate only.
