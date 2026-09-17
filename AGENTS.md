# Agent and CI guide for cdf2ms

`cdf2ms` is a pure-Go (`CGO_ENABLED=0`), dependency-free converter from ANDI/MS
NetCDF to mzML/mzXML. This file tells agents and the engineering gate how to
lint, test, and verify the repository.

## Build & test commands

All commands are declared as Make targets and are the single source of truth for
CI and the engineering gate:

```sh
make lint    # gofmt -l + go vet + go build (CGO_ENABLED=0)
make test    # go test -count=1 ./... (CGO_ENABLED=0)
make build   # CGO_ENABLED=0 go build ./...
make verify  # end-to-end: fixtures → convert --verify → report summarize
make bench   # benchmarks
```

These run exactly as the shipped binary is built (`CGO_ENABLED=0`); there is no
cgo and no runtime dependency on Python.

## Lint (the gate's lint step)

The canonical lint command is:

```sh
make lint
```

It runs `gofmt -l pkg cmd` (must print nothing), `go vet ./...`, and
`go build ./...` under `CGO_ENABLED=0`, and fails on any non-conformant file.

## Tests

The canonical test command is:

```sh
make test
```

It runs `go test -count=1 ./...` under `CGO_ENABLED=0`, covering the NetCDF
engine, the ANDI reader, the mzML/mzXML writers, the batch engine, the
independent verifier, and the CLI.

## Verification

The canonical end-to-end verification command is:

```sh
make verify
```

It builds the CLI, writes synthetic ANDI fixtures, converts them with
`--verify`, and summarizes the journal, failing on any discrepancy.

## Repository layout

- `pkg/netcdfio` — pure-Go NetCDF classic engine (CDF-1/2/5).
- `pkg/andiio` — ANDI/MS semantic reader.
- `pkg/mzml`, `pkg/mzxml` — schema-validated writers.
- `pkg/convert` — batch engine (discovery, workers, resume, fail-fast, reports).
- `pkg/validate` — independent read-back verifier.
- `cmd/cdf2ms` — the CLI.
- `docs/` — usage, compatibility, provenance, format notes, and the spec.

## Python validation (dev/CI only, never at runtime)

`cdf2ms` never shells out to Python. `scripts/validate_ms_outputs.py` is an
external gate that validates outputs against the official schemas and reference
readers; it requires a venv with `lxml netCDF4 numpy pyteomics`.
