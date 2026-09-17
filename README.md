# cdf2ms

Pure-Go, dependency-free converter from **ANDI/MS ASTM netCDF** (CDF-1/CDF-2/CDF-5)
to **mzML** and **mzXML**, built for large multi-file corpora.

`cdf2ms` is a data-integrity tool first and a converter second: it audits a corpus
before converting, streams spectra with bounded memory, round-trips every converted
spectrum back through an independent reader, and refuses to silently change numbers.

- **Pure Go, zero cgo.** The NetCDF reader is built in; the binary is `CGO_ENABLED=0`.
- **Bounded memory.** One streaming pass per source, block reads, no whole-file loads.
- **Scientific integrity.** Unknown facts are omitted, never invented; derived
  fields carry provenance codes; packed data is unpacked per convention; fill and
  non-finite values are counted and reported.
- **Verified.** Every output is checked against the official mzML 1.1.0 and mzXML
  3.2 schemas plus reference readers (see [docs/compatibility.md](docs/compatibility.md)).

## Quick start

```sh
go build ./cmd/cdf2ms

./cdf2ms audit ./corpus
./cdf2ms convert -overwrite -verify ./corpus
./cdf2ms report summarize run.jsonl
```

## Documentation

- [docs/usage.md](docs/usage.md) — commands, flags, exit codes, integrity rules.
- [docs/compatibility.md](docs/compatibility.md) — what is accepted and produced,
  and how it is validated against real vendor data and the official schemas.
- [docs/provenance.md](docs/provenance.md) — stable diagnostic codes and the
  data-integrity contract.
- [docs/format-notes/encoding.md](docs/format-notes/encoding.md) — exact
  mzML/mzXML encoding decisions.
- [docs/specs/cdf2ms-production-implementation-spec.md](docs/specs/cdf2ms-production-implementation-spec.md) — the implementation spec.

## Design

- `pkg/netcdfio` — pure-Go NetCDF classic engine (CDF-1/2/5), zero cgo.
- `pkg/andiio`  — ANDI/MS (ASTM E1205/E1947) semantic reader.
- `pkg/mzml`, `pkg/mzxml` — standard-compliant writers (validated against the
  official schemas).
- `pkg/convert` — batch engine: discovery, worker pool, atomic rename, resume,
  fail-fast, reports.
- `pkg/validate` — independent read-back verifier (parses XML/base64/zlib itself).
- `pkg/msdata` — format-neutral run/spectrum model and stable error/warning codes.

## Development

```sh
make lint        # gofmt + go vet + build
make test        # all package and CLI tests (pure Go)
make verify      # end-to-end: fixtures → convert --verify → summarize
make bench       # benchmarks
```

### Validation toolchain (dev/CI only)

`cdf2ms` never shells out to Python at runtime. The Python toolchain (`lxml`,
`netCDF4`, `numpy`, `pyteomics`) is used only as an external validation gate:

```sh
/tmp/msval/bin/python scripts/validate_ms_outputs.py OUT.mzML  --source SRC.CDF
/tmp/msval/bin/python scripts/validate_ms_outputs.py OUT.mzXML --source SRC.CDF
```

Real public ANDI/MS corpora used for validation live in the CI `real-corpus` job
(gated by `CDF2MS_CORPUS`) and locally under `~/.cache/cdf2ms-scratch/corpus`.

## Status

See [docs/specs/cdf2ms-production-implementation-spec.md](docs/specs/cdf2ms-production-implementation-spec.md).
