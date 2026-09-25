# cdf2ms usage

`cdf2ms` converts ANDI/MS (ASTM E1205/E1947) NetCDF corpora to `mzML` and `mzXML`
in one streaming pass per file, with bounded memory and independent read-back
verification. It is pure Go (`CGO_ENABLED=0`), has no runtime dependency on Python
or any external tool, and never loads a whole source file into memory.

## Build

```sh
go build ./cmd/cdf2ms        # CGO_ENABLED=0 is the default and is enforced by make
make lint                    # gofmt + go vet + build
make test                    # all package + CLI tests
make verify                  # end-to-end: fixtures → convert --verify → summarize
```

## Commands

```text
cdf2ms inspect  [flags] FILE...   describe the NetCDF container (inspection)
cdf2ms audit    [flags] FILE...   ANDI conformance and convertibility audit
cdf2ms convert  [flags] PATH...   convert files or directories (batch)
cdf2ms validate SOURCE.CDF OUT    independently verify one output against the source
cdf2ms verify-corpus PATH... [flags] convert into scratch, verify, summarize, delete
cdf2ms fixtures DIR [flags]       write synthetic ANDI files for testing
cdf2ms report summarize FILE.jsonl aggregate a conversion journal
cdf2ms version                    print build information
```

## Inspect

`inspect` describes the NetCDF container without interpreting ANDI semantics:

```sh
cdf2ms inspect -json corpus/*.cdf
cdf2ms inspect -globals -vars gc01_0812_066.cdf
```

It reports container format (CDF-1/2/5, or HDF5/NetCDF4 which is described but not
convertible), magic, size, record/dimension layout, global attributes, variables,
and — when the source is convertible — the ANDI-level summary: scan and point
counts, retention-time range in seconds, m/z range, instrument, and the
schema/vendor fingerprints. `-json` emits one object per file.

## Audit

`audit` checks whether a source is honestly convertible *before* anything is
written, and reports the decisions the converter will make:

```sh
cdf2ms audit ./corpus
cdf2ms audit -deep -json ./corpus
```

The audit reports:

- verdict (convertible / convertible-with-warnings / not-convertible);
- retention-time unit, its origin (`var:` declared on the variable, `global:`
  declared by the file, `inference:` from magnitude, or ambiguous) and
  confidence;
- the scan plan (how per-scan peak counts are derived);
- role mapping (which variables play which ANDI role);
- missing required roles, aliases, unmapped variables, and warnings;
- with `-deep`, optional per-scan statistics.

Retention-time units are resolved in this order: `scan_acquisition_time.units`,
then `time_values.units`, then the file-global `units` attribute (ASTM E1482
general data defines it for the chromatographic axis, and Agilent writes it on
exports whose time variable has no attributes of its own), and only then a
magnitude test on the scan spacing. The magnitude test must be decisive — one
hypothesis plausible, the other not — or the file is a **not-convertible**
verdict with a stable `ANDI_AMBIGUOUS_UNITS` code and a pointer to `--rt-unit`.
A unit stated anywhere in the file is never overridden by the heuristic, and is
honoured by `--rt-unit strict` too; a global `units` that is not a time unit (an
m/z or intensity unit) is ignored for this purpose.

## Convert

```sh
# One file, both formats, beside the source.
cdf2ms convert gc01_0812_066.cdf

# A whole corpus, one read per file, outputs in ./mzml, re-open verified.
cdf2ms convert -out-dir ./mzml -overwrite -verify ./corpus

# mzML only, compressed, 8 workers, journal + aggregate report.
cdf2ms convert -format mzml -compress -jobs 8 \
  -report-jsonl run.jsonl -report-json run.json \
  -verify ./corpus

# Resume a large run from a previous journal: files already converted and still
# byte-identical are skipped.
cdf2ms convert -resume run.jsonl -hash-outputs ./corpus
```

### Options

| Flag | Meaning |
|------|---------|
| `-format mzml[,mzxml]` | output formats; repeatable or comma-separated |
| `-out-dir DIR` | write outputs here instead of beside each source |
| `-overwrite` | replace existing outputs (default: report a collision, skip) |
| `-skip-existing` | skip sources whose output already exists |
| `-jobs N` / `-workers N` | concurrent file conversions (0 = one per CPU) |
| `-fail-fast` | stop scheduling new files after the first failure |
| `-temp-dir DIR` | hold scratch files here; atomic rename into place after |
| `-resume FILE.jsonl` | skip sources already recorded and still matching |
| `-rt-unit auto\|strict\|seconds\|minutes` | retention-time unit policy |
| `-precision auto\|f32\|f64` | binary array precision (auto = lossless) |
| `-compress` / `-compression-level N` | zlib-compress binary arrays |
| `-recursive` | descend into directories |
| `-max-source-bytes SIZE` | skip sources larger than SIZE (e.g. `4GB`) |
| `-verify` / `-verify-tolerance X` | re-open each output and compare every number |
| `-report-json PATH` | write the aggregate machine-readable report |
| `-report-jsonl PATH` | stream one JSON record per source file (resume contract) |
| `-hash-source` | hash each source (SHA-1 + SHA-256), one pass |
| `-hash-outputs` | digest each output as it is written |
| `-log-format text\|json`, `-log-level debug\|info\|warn\|error`, `-log-file PATH` | structured logging |
| `-json`, `-quiet` | report rendering / suppress progress |
| `-fail-on-warning` | exit 4 when files convert but report warnings |
| `-ext .cdf,.CDF` | source extensions for directory scans |

### Exit codes

| Code | Meaning |
|------|---------|
| `0` | success (warnings are reported but do not fail the run) |
| `1` | usage or flag error |
| `2` | at least one file failed or is not convertible |
| `3` | interrupted by SIGINT/SIGTERM before finishing |
| `4` | `--fail-on-warning` tripped |

### Scientific-integrity guarantees

- **Never invent a fact.** Unknown polarity, centroid state, isolation width, and
  end times are omitted, not guessed. Optional metadata stays unset.
- **Never silently change a number.** Packed data is unpacked per the netCDF
  convention; fill values and non-finite numbers are counted and reported
  (`ANDI_PACKING_APPLIED`, non-finite/`NaN` counts). Precision is `auto` only
  when every value round-trips exactly; `f32` never silently truncates.
- **RT normalization** converts to seconds and records the origin and confidence
  of the unit. An undetermined unit refuses to convert rather than guess.
- **Count integrity.** The declared spectrum count must equal the written count;
  a mismatch fails the file and preserves the document at `*.partial`.
- **Independent verification.** `--verify` re-parses each output with its own
  XML/base64/zlib reader (not the writer's code) and compares every value.
- **mzML binary data is little-endian; mzXML binary data is big-endian** (network
  order), matching each standard.

## Report

`convert` writes a machine-readable report in two shapes:

- `-report-json` — the aggregate report (`cdf2ms report summarize` input is the
  JSONL form).
- `-report-jsonl` — one JSON record per source file, streamed live so a killed
  run still leaves a resumable journal. Fields follow the spec §21 shape
  (`source`, `source_sha256`, `schema_fingerprint`, `vendor_fingerprint`,
  `status`, `scan_count`, `point_count`, `duration_ms`, `read_bytes`,
  `write_bytes`, `warnings`, `error_code`, `error`, `converter_version`,
  `outputs[].sha256`).

```sh
cdf2ms report summarize run.jsonl
cdf2ms report summarize -json run.jsonl
```

## Fixtures

`fixtures` writes synthetic ANDI files reproducing awkward real-world layouts, so
the toolchain can be tested without vendor data:

```sh
cdf2ms fixtures ./fx
cdf2ms fixtures -variant plain,minutes,cdf2,cdf5,packed -manifest fx.jsonl ./fx
```

Variants include `plain`, `minutes`, `unitless-ambiguous`, `cdf2`, `cdf5`,
`packed`, `no-scan-index`, `no-point-count`, `one-based-index`,
`zero-point-scans`, `mixed-msn`, `nonutf8-metadata`, and `irregular-points`.
