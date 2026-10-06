# cdf2ms - ANDI/MS NetCDF to mzML/mzXML converter (pure Go, no cgo).
#
# Every target honours CGO_ENABLED=0: the converter must remain a pure-Go binary
# with no cgo, so the default build is exercised exactly as it ships.

GO        ?= go
CGO_FLAGS := CGO_ENABLED=0

.PHONY: all lint fmt test build vet verify clean bench release test-release

all: fmt vet build test

# lint is the gate target: format conformance plus static analysis. It must be
# cheap enough to run on every commit and strict enough to catch the usual
# mistakes (unused code, suspicious conversions, missing error handling).
lint:
	@echo "== gofmt =="
	@out=$$(gofmt -l pkg cmd); if [ -n "$$out" ]; then \
		echo "gofmt: files need formatting:"; echo "$$out"; exit 1; fi
	@echo "== go vet =="
	$(CGO_FLAGS) $(GO) vet ./...
	@echo "== go build =="
	$(CGO_FLAGS) $(GO) build ./...

fmt:
	gofmt -w pkg cmd

vet:
	$(CGO_FLAGS) $(GO) vet ./...

build:
	$(CGO_FLAGS) $(GO) build ./...

test:
	$(CGO_FLAGS) $(GO) test -count=1 ./...

# verify is the end-to-end gate: build the CLI and run it against synthetic
# fixtures, including re-open verification of every output.
verify: build
	@tmp=$$(mktemp -d); trap 'rm -rf $$tmp' EXIT; $(CGO_FLAGS) $(GO) build -o $$tmp/cdf2ms ./cmd/cdf2ms && $$tmp/cdf2ms fixtures -variant plain,minutes,global-units,cdf2,cdf5,packed -manifest $$tmp/fx/manifest.jsonl $$tmp/fx >/dev/null && $$tmp/cdf2ms convert -overwrite -verify -report-jsonl $$tmp/fx/run.jsonl $$tmp/fx >/dev/null && $$tmp/cdf2ms report summarize $$tmp/fx/run.jsonl >/dev/null && echo "verify: OK"

bench:
	$(CGO_FLAGS) $(GO) test -bench . -benchmem ./pkg/...

# VERSION must match the release tag; output must be empty to prevent mixing builds.
release:
	python3 scripts/build_release.py --version "$(VERSION)" --output dist

test-release:
	python3 -m unittest discover -s scripts -p 'test_*release.py'

clean:
	rm -rf bin dist
