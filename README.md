# cdf2ms

Pure-Go, dependency-free converter from **ANDI/MS ASTM netCDF** (CDF-1/CDF-2/CDF-5)
to **mzML** and **mzXML**, built for large multi-file corpora.

`cdf2ms` is a data-integrity tool first and a converter second: it audits a corpus
before converting, streams spectra with bounded memory, round-trips every converted
spectrum back through an independent reader, and refuses to silently change numbers.

## Status

See `docs/specs/cdf2ms-production-implementation-spec.md`.

## Quick start

```sh
go build ./cmd/cdf2ms
./cdf2ms audit ./corpus
./cdf2ms convert ./corpus/single.CDF --format mzml --out ./mzml
```

## Design

- `pkg/netcdfio` — pure-Go NetCDF classic engine (CDF-1/2/5), zero cgo.
- `pkg/andiio`  — ANDI/MS (ASTM E1205/E1947) semantic reader.
- `pkg/mzml`, `pkg/mzxml` — standard-compliant writers.
- `pkg/msdata` — format-neutral run/spectrum model shared by all of the above.
