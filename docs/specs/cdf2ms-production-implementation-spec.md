# cdf2ms — Production Go Converter for ANDI/MS NetCDF → mzML / mzXML

**Status:** Implementation specification  
**Primary language:** Go  
**Primary goal:** Build a production-quality, high-throughput, scientifically defensible converter for large historical ANDI/MS NetCDF corpora, targeting mzML and mzXML.  
**Expected corpus scale:** 100,000+ NetCDF files  
**Default deployment model:** Single statically linked Go executable, suitable for workstations, servers, containers, HPC batch jobs, and Slurm environments.

---

## 1. Executive summary

Build `cdf2ms`, a native Go tool and reusable Go library that reads mass-spectrometry data stored in ANDI/MS-style NetCDF files and writes standards-compliant mzML and mzXML.

The implementation must be designed for a very large and heterogeneous existing corpus rather than a small set of hand-picked files. The project therefore includes not only conversion, but also:

- corpus inventory and structural fingerprinting;
- automated detection of NetCDF/ANDI variants;
- representative regression-corpus extraction;
- streaming conversion with bounded memory;
- parallel conversion across files;
- standards-compliant mzML and mzXML writers;
- preservation of metadata where scientifically valid;
- explicit handling of unknown or missing metadata without guessing;
- numerical round-trip validation;
- provenance and checksums;
- structured reports for millions/billions of spectra;
- fuzzing and regression testing;
- benchmark tooling;
- CI validation;
- deterministic output where practical.

The architecture must separate source parsing from the normalized MS representation and output writers so future formats can be added without rewriting the NetCDF reader.

The implementation should prefer pure Go and must produce a working `CGO_ENABLED=0` build for the default feature set.

---

# 2. Project goals

## 2.1 Primary goals

1. Convert ANDI/MS NetCDF files to mzML.
2. Convert ANDI/MS NetCDF files to mzXML.
3. Support recursive batch conversion of 100,000+ files.
4. Stream spectra and avoid loading whole source files into memory.
5. Run multiple file conversions concurrently.
6. Preserve numerical spectral data with controlled and documented precision.
7. Preserve available metadata when mappings are reliable.
8. Never invent scientific metadata that is absent or ambiguous.
9. Provide strong machine-readable validation and conversion reports.
10. Build a corpus-driven compatibility system using the existing large NetCDF archive.
11. Produce a portable Go binary without requiring Python, Java, ProteoWizard, or libnetcdf at runtime.
12. Expose core functionality as reusable Go packages, not only as a CLI.

## 2.2 Secondary goals

- Fast corpus inspection without full conversion.
- Structural fingerprinting of source files.
- Automated selection of representative regression fixtures.
- Optional indexed mzML output.
- Optional zlib compression.
- Configurable output precision.
- Resumable batch conversion.
- HPC/Slurm-friendly execution.
- JSON and JSONL reporting.
- Optional Parquet reporting if a lightweight, well-maintained Go library is appropriate.
- Prometheus-style runtime metrics may be added later but are not required for the first release.

---

# 3. Non-goals for v1

Do not expand scope prematurely.

The first production release does **not** need to:

- replace ProteoWizard for arbitrary vendor raw formats;
- implement vendor proprietary formats;
- infer precursor information not contained in the source;
- perform centroiding;
- perform peak picking;
- denoise spectra;
- recalibrate m/z values;
- alter retention times beyond unit normalization;
- convert profile spectra to centroid spectra;
- create synthetic MS/MS relationships;
- rewrite scientific content for convenience;
- implement a GUI;
- implement mzMLb or mzPeak in v1.

The architecture should permit future output formats.

---

# 4. Repository

Preferred repository name:

```text
cdf2ms
```

If working within an existing organization, create/use the repository under the organization's normal namespace.

Suggested module name:

```text
github.com/<org>/cdf2ms
```

Do not hard-code the organization name until the surrounding workspace/repository conventions are inspected.

---

# 5. Source format assumptions

The primary input is ANDI/MS-style NetCDF used historically for mass spectrometry, especially GC-MS.

Common variables may include, but are not limited to:

```text
scan_acquisition_time
scan_index
point_count
mass_values
intensity_values
total_intensity
mass_range_min
mass_range_max
actual_scan_number
```

Common global or variable attributes may include instrument information, acquisition metadata, units, scaling information, comments, dataset identifiers, operator information, experiment dates, and other vendor/exporter fields.

Files in the corpus may differ in:

- NetCDF classic encoding;
- 64-bit-offset variants;
- NetCDF4/HDF5-backed encoding;
- variable names;
- variable data types;
- optional dimensions;
- missing `point_count`;
- missing `scan_index`;
- alternate scan-number conventions;
- retention-time units;
- mass-value types and scaling;
- intensity-value types;
- metadata naming;
- vendor-specific attributes;
- malformed or partially compliant data;
- unusual zero-point scans;
- empty scans;
- files truncated or corrupted during historical transfer.

The implementation must be capability-driven and tolerant of known variants rather than assuming one rigid layout.

---

# 6. Dependency requirements

## 6.1 NetCDF

Prefer a maintained pure-Go NetCDF implementation that supports the formats actually present in the corpus.

Candidate to evaluate:

```text
github.com/jed72/go-native-netcdf
```

Before committing to it:

1. inspect maintenance state;
2. inspect supported NetCDF encodings;
3. verify hyperslab/slice reads;
4. verify numeric type coverage;
5. verify behavior on real corpus samples;
6. benchmark memory and throughput;
7. confirm `CGO_ENABLED=0`.

If it fails materially on the corpus, implement an adapter so another reader can be substituted.

Do **not** couple all conversion logic directly to one NetCDF dependency.

Define an internal source abstraction.

## 6.2 XML

Use Go's standard XML facilities where practical, but do not rely on naïve `encoding/xml` marshaling if it prevents:

- exact streaming;
- reliable byte-offset calculation;
- deterministic output;
- high throughput;
- indexed output.

A small purpose-built streaming XML writer is acceptable and likely preferable.

## 6.3 Compression

Use the Go standard library:

```text
compress/zlib
encoding/base64
encoding/binary
```

unless benchmarks prove a substantial need for alternatives.

## 6.4 Validation dependencies

Production conversion must not require Java or Python.

Development/CI may optionally use independent validators/readers, including ProteoWizard or PSI validation tooling, to cross-check output compatibility.

---

# 7. High-level architecture

Use a layered design:

```text
            ┌──────────────────────┐
            │  NetCDF source file  │
            └──────────┬───────────┘
                       │
                ANDI source adapter
                       │
                       ▼
            ┌──────────────────────┐
            │ normalized MS model  │
            │ Run / Spectrum       │
            └──────────┬───────────┘
                       │
          ┌────────────┴────────────┐
          │                         │
          ▼                         ▼
 ┌─────────────────┐      ┌─────────────────┐
 │    mzML writer   │      │   mzXML writer  │
 └─────────────────┘      └─────────────────┘
```

The normalized model must not contain mzML-specific XML concepts.

---

# 8. Suggested package layout

```text
cmd/
  cdf2ms/

internal/
  andi/
    reader.go
    schema.go
    variables.go
    metadata.go
    units.go
    fingerprint.go
    errors.go

  netcdfio/
    reader.go
    native.go

  msdata/
    run.go
    spectrum.go
    instrument.go
    source.go

  mzml/
    writer.go
    binary.go
    cv.go
    index.go
    checksum.go

  mzxml/
    writer.go
    binary.go
    index.go

  convert/
    convert.go
    options.go
    worker.go

  audit/
    audit.go
    fingerprint.go
    summarize.go

  validate/
    validate.go
    compare.go
    numeric.go

  report/
    report.go
    json.go
    jsonl.go

  provenance/
    provenance.go

  testutil/
    fixtures.go
    synth.go

docs/
  specs/
  format-notes/
  compatibility/

testdata/
  synthetic/
  regression/
```

Public packages may be promoted out of `internal/` once the API stabilizes.

---

# 9. Core domain model

Define a normalized domain model similar to:

```go
type Run struct {
    ID          string
    Source      SourceFile
    Instrument  Instrument
    Metadata    map[string]MetadataValue
}

type Spectrum struct {
    Index         int
    ScanNumber    int64
    RetentionTime float64 // canonical seconds

    MSLevel       *int
    Polarity      Polarity

    MZ            []float64
    Intensity     []float64

    TIC           *float64
    BasePeakMZ    *float64
    BasePeakInt   *float64
    LowestMZ      *float64
    HighestMZ     *float64

    Metadata      map[string]MetadataValue
}
```

Important:

- use pointer/optional fields for unknown scientific metadata;
- do not encode "unknown" as a false scientific assertion;
- canonical retention-time unit internally is seconds;
- raw source metadata should remain inspectable for provenance/debugging;
- writers decide how a field maps to target-format concepts.

---

# 10. Reader interfaces

Define interfaces roughly equivalent to:

```go
type RunReader interface {
    Metadata(ctx context.Context) (*msdata.Run, error)
    Next(ctx context.Context) (*msdata.Spectrum, error)
    Close() error
}
```

For very large point arrays, the implementation should reuse buffers where practical.

The reader must distinguish:

- end-of-file;
- malformed scan;
- missing required variable;
- unsupported source encoding;
- recoverable metadata warning;
- corruption/truncation.

Use typed errors and structured warnings.

---

# 11. Spectrum slicing

The reader must support common ways scans are represented.

Preferred approach when both exist:

```text
start = scan_index[i]
count = point_count[i]
```

Fallback logic may derive counts from adjacent indexes:

```text
count = scan_index[i+1] - scan_index[i]
```

For the final scan:

```text
count = total_points - scan_index[last]
```

Only use derived behavior when structurally valid.

Validation requirements:

- start >= 0;
- count >= 0;
- start + count <= total point count;
- m/z and intensity arrays are compatible;
- scan indexes are monotonic where required.

Never silently clip invalid slices.

---

# 12. Retention-time normalization

The normalized model uses seconds.

Inspect variable attributes and source metadata for units.

Recognized examples should include:

```text
s
sec
second
seconds
min
minute
minutes
```

Normalize case and whitespace.

Behavior:

- seconds → unchanged;
- minutes → multiply by 60;
- known explicit alternate time units → convert correctly;
- missing/unknown units → apply a documented compatibility policy.

Default policy for unknown units should **not silently guess**.

Recommended CLI behavior:

```text
--unknown-rt-unit=error
--unknown-rt-unit=seconds
--unknown-rt-unit=minutes
```

Default production batch mode should be configurable, with warnings included in reports.

The audit command must report all observed unit spellings.

---

# 13. Numeric data and precision

Internally normalize m/z and intensity values to `float64` to avoid loss during reading and transformation.

Output defaults:

### mzML

```text
m/z arrays:        64-bit float
intensity arrays:  32-bit float, unless lossless mode is requested
compression:       zlib
```

### mzXML

Default precision should be configurable.

Provide:

```text
--mz-precision 32|64
--intensity-precision 32|64
--lossless
```

`--lossless` should choose output precision sufficient to represent source values without avoidable precision reduction, subject to target-format capabilities.

Numerical validation must use explicit tolerances based on requested output precision.

---

# 14. Metadata policy

## 14.1 General rule

Preserve what is known.

Do not infer scientific facts merely because a value is typical.

Examples that must not be guessed without evidence:

- MS level;
- polarity;
- centroid/profile state;
- precursor identity;
- precursor m/z;
- charge;
- collision energy;
- ionization mode;
- analyzer type;
- detector type.

## 14.2 Raw metadata preservation

Where target formats allow safe user parameters or comments, optionally preserve unmapped source metadata under an explicit namespace/prefix, for example:

```text
ANDI:original_attribute_name
```

Do not pollute controlled vocabulary fields with arbitrary text.

## 14.3 Provenance

Every output must record:

- converter name;
- converter version;
- Git commit if available;
- conversion timestamp, unless deterministic mode omits/normalizes it;
- source filename;
- source file size;
- source checksum;
- conversion options;
- target format/version.

Recommended checksum: SHA-256.

---

# 15. mzML writer

## 15.1 Version

Target PSI mzML 1.1.x, with the exact schema version selected after validating against the current official PSI schema used by mainstream readers.

Prefer indexed mzML where compatible.

The writer must:

- stream spectra;
- encode binary arrays correctly;
- describe array types using correct PSI-MS CV terms;
- encode numeric precision using correct CV terms;
- encode zlib/no-compression using correct CV terms;
- encode retention time with explicit units;
- emit spectrum count;
- emit source file information;
- emit software provenance;
- produce valid IDs;
- escape XML safely;
- optionally write an index with byte offsets;
- optionally write a file checksum if required by the selected indexed mzML schema.

## 15.2 Binary arrays

For each spectrum:

```text
binaryDataArray: m/z
binaryDataArray: intensity
```

Pipeline:

```text
numeric slice
  -> little-endian or specification-required binary representation
  -> optional zlib
  -> base64
  -> XML
```

Verify endianness against the mzML specification and independent readers.

Do not rely on assumptions inherited from mzXML.

## 15.3 Indexed mzML

When enabled:

- count bytes written at the actual output stream;
- record spectrum offsets;
- write the index list after the mzML payload;
- validate offsets with an independent random-access reader;
- ensure checksums match the specification exactly.

Provide:

```text
--indexed=true|false
```

Default should become `true` only after validation proves broad compatibility.

---

# 16. mzXML writer

Target the widely supported mzXML schema/version chosen after compatibility testing.

The writer must:

- stream scans;
- encode interleaved `(m/z, intensity)` pairs correctly;
- use the byte order required by mzXML;
- support 32-bit and 64-bit precision where allowed;
- support zlib compression where allowed;
- base64 encode payloads;
- encode retention time correctly;
- emit scan counts;
- emit indexes if supported by the selected schema;
- preserve compatible metadata;
- validate generated files against independent software.

Example logical binary layout:

```text
mz1
intensity1
mz2
intensity2
...
```

Do not copy mzML binary encoding rules into mzXML.

---

# 17. Commands

Use one executable:

```text
cdf2ms
```

## 17.1 Convert one file

```bash
cdf2ms convert sample.cdf \
  --format mzml \
  --output sample.mzML
```

## 17.2 Convert recursively

```bash
cdf2ms convert /data/netcdf \
  --recursive \
  --format mzml \
  --output-dir /data/mzml \
  --jobs 24
```

## 17.3 Multiple target formats

Support either:

```bash
--format mzml
--format mzxml
```

and, if cleanly implementable:

```bash
--format mzml,mzxml
```

If producing both formats from one read materially improves performance, support fan-out through one normalized spectrum stream with bounded buffering.

## 17.4 Inspect

```bash
cdf2ms inspect sample.cdf
```

Human-readable output should include:

- NetCDF encoding/version;
- dimensions;
- variables;
- types;
- relevant units;
- scan count;
- total point count;
- retention-time range;
- m/z range if cheaply available;
- detected instrument/vendor metadata;
- recognized ANDI capabilities;
- warnings;
- structural fingerprint.

Also support:

```bash
--json
```

## 17.5 Audit a corpus

```bash
cdf2ms audit /archive/netcdf \
  --recursive \
  --jobs 32 \
  --report audit.jsonl
```

Audit should read metadata/headers and only enough data to classify structure where possible.

It must identify:

- total files;
- readable/unreadable files;
- NetCDF encodings;
- variable layouts;
- variable data types;
- dimensions;
- attributes;
- retention-time units;
- vendors/models;
- missing required fields;
- suspicious index/count layouts;
- zero-length arrays;
- corruption indicators;
- unique structural fingerprints.

## 17.6 Validate

```bash
cdf2ms validate source.cdf output.mzML
```

Validation should read the output using an independent code path from the writer when practical.

Compare:

- scan count;
- scan numbering;
- retention time;
- point count;
- m/z arrays;
- intensity arrays;
- TIC when meaningful;
- base peak when meaningful;
- min/max m/z.

Report exact/tolerance matches.

## 17.7 Corpus validation

```bash
cdf2ms verify-corpus /archive/netcdf \
  --format mzml \
  --temp-dir /scratch/cdf2ms \
  --jobs 24 \
  --report verify.jsonl
```

This command may convert into scratch storage, validate, summarize, and optionally delete successful outputs.

---

# 18. Structural fingerprinting

A major requirement is automatic corpus classification.

Each input file should receive a deterministic fingerprint derived from relevant structural characteristics, for example:

- NetCDF encoding;
- dimensions and sizes where appropriate;
- set of relevant variable names;
- variable data types;
- variable dimensions;
- key unit attributes;
- presence/absence of scan index;
- presence/absence of point count;
- relevant scale-factor/add-offset usage;
- relevant metadata fields.

Do not include values that make every file unique, such as run names or exact scan counts, unless intentionally producing a secondary fingerprint.

Produce at least two concepts:

### Schema fingerprint

Groups files with equivalent layouts.

### Metadata/vendor fingerprint

Groups files by exporter/instrument characteristics.

Fingerprint output must be deterministic and stable across machines.

---

# 19. Regression-corpus builder

Provide tooling to select representative source files from the huge archive without committing the whole archive into Git.

Example:

```bash
cdf2ms fixtures select audit.jsonl \
  --per-schema 5 \
  --include-failures \
  --output manifest.json
```

The fixture manifest should store:

- source path or corpus identifier;
- checksum;
- schema fingerprint;
- vendor fingerprint;
- reason selected;
- expected behavior;
- known warnings.

Do not copy source files unless explicitly requested.

Support generating tiny synthetic fixtures for CI when real files cannot be redistributed.

Local/private full-corpus regression tests should be separately runnable.

---

# 20. Batch engine

For a large corpus, parallelize primarily across files.

Architecture:

```text
file discovery
    ↓
bounded job queue
    ↓
worker pool
    ↓
one streaming converter per worker
    ↓
atomic output rename
```

Requirements:

- configurable `--jobs`;
- bounded queues;
- context cancellation;
- clean SIGINT/SIGTERM handling;
- no partial file presented as successful;
- temporary output followed by atomic rename;
- skip/overwrite policies;
- structured per-file result;
- aggregate statistics;
- failure isolation;
- no one bad file should terminate an entire batch unless `--fail-fast`.

Options:

```text
--jobs N
--overwrite
--skip-existing
--fail-fast
--temp-dir
--resume
```

Resume behavior should rely on output + provenance/report state, not only filename existence.

---

# 21. Reporting

Batch mode must produce machine-readable results.

Recommended default:

```text
JSONL
```

One record per source file.

Fields should include:

```json
{
  "source": "...",
  "source_sha256": "...",
  "source_size": 0,
  "schema_fingerprint": "...",
  "vendor_fingerprint": "...",
  "status": "success|warning|failed|skipped",
  "target_format": "mzml",
  "output": "...",
  "output_sha256": "...",
  "scan_count": 0,
  "point_count": 0,
  "duration_ms": 0,
  "read_bytes": 0,
  "write_bytes": 0,
  "warnings": [],
  "error_code": null,
  "error": null,
  "converter_version": "..."
}
```

Also print a concise terminal summary.

Optional aggregate report:

```text
cdf2ms report summarize conversion.jsonl
```

---

# 22. Error taxonomy

Use stable error codes.

Examples:

```text
NC_OPEN_FAILED
NC_UNSUPPORTED_ENCODING
ANDI_REQUIRED_VARIABLE_MISSING
ANDI_INVALID_SCAN_INDEX
ANDI_POINT_COUNT_MISMATCH
ANDI_UNKNOWN_RT_UNIT
ANDI_MASS_INTENSITY_LENGTH_MISMATCH
SOURCE_TRUNCATED
OUTPUT_WRITE_FAILED
MZML_VALIDATION_FAILED
MZXML_VALIDATION_FAILED
NUMERIC_MISMATCH
```

Machine-readable reports should use codes rather than requiring log parsing.

---

# 23. Logging

Use structured logging.

Support:

```text
--log-format text
--log-format json
--log-level debug|info|warn|error
```

Do not log every spectrum by default.

Include:

- file identity;
- worker;
- stage;
- error code;
- elapsed time.

---

# 24. Memory requirements

The converter must be streaming.

Do not load:

- full m/z array;
- full intensity array;
- all spectra;
- entire generated XML;

unless a specific source encoding makes partial reads impossible.

Target memory model:

```text
O(points in current spectrum)
```

plus compression/XML buffers.

Use buffer reuse and `sync.Pool` only if benchmarks show a benefit and complexity remains manageable.

Benchmark memory with large files.

---

# 25. Performance requirements

Do not set unrealistic fixed throughput until real files are benchmarked.

Instead establish baselines.

Required benchmarks:

- NetCDF metadata inspection throughput;
- scan slicing throughput;
- mzML binary encoding;
- mzXML interleaving;
- zlib compression;
- Base64 encoding;
- complete single-file conversion;
- concurrent multi-file conversion.

Capture:

```text
MB/s source
MB/s output
spectra/s
points/s
allocations
peak RSS
CPU utilization
```

Performance acceptance criterion:

The implementation must show near-linear improvement when increasing worker count until storage or CPU becomes the bottleneck, without unbounded memory growth.

---

# 26. Scientific validation strategy

This is a critical part of the project.

## 26.1 Level 1: structural

For every converted file:

- output opens;
- schema/structure valid;
- expected scan count;
- valid array lengths;
- valid indexes.

## 26.2 Level 2: numerical

Read converted spectra and compare against source.

For every scan:

```text
source point count == output point count
RT source ≈ output
m/z source ≈ output
intensity source ≈ output
```

Use exact comparison where the selected output representation permits exact equality.

Otherwise use documented tolerances derived from the output type.

Do not use broad arbitrary tolerances.

## 26.3 Level 3: derived statistics

Compare when appropriate:

- TIC;
- base peak intensity;
- base peak m/z;
- lowest observed m/z;
- highest observed m/z.

Derived statistics should be computed consistently and should not fail files merely because optional source metadata was absent.

## 26.4 Level 4: independent compatibility

Automated development/CI tests should, where practical, verify output using independent software.

Possible independent checks:

- official PSI schema validation;
- ProteoWizard reader;
- OpenMS;
- another mature mzML reader.

Do not make these external tools runtime dependencies of `cdf2ms`.

---

# 27. Corpus-driven development workflow

Because hundreds of thousands of source files are available, use the corpus as a compatibility oracle.

Recommended sequence:

## Phase A — audit

Run `cdf2ms audit` over the archive.

Produce:

- corpus summary;
- schema fingerprints;
- metadata/vendor fingerprints;
- unit spellings;
- variable layouts;
- unreadable files;
- corruption candidates.

## Phase B — representative samples

Select representative files from every discovered structural class.

Prioritize:

- common classes;
- unique classes;
- malformed files;
- files with unusual types;
- files with unknown RT units;
- files from every exporter/vendor family.

## Phase C — implementation

Implement reader variants from most common to least common.

## Phase D — full corpus verification

Continuously run against the full archive.

Every unexplained failure becomes:

1. classified;
2. reproduced;
3. fixed if valid input;
4. converted into a regression fixture/test;
5. documented.

## Phase E — stability gate

Do not declare production-ready based on a small fixture set.

Require a full-corpus report.

---

# 28. Test strategy

## 28.1 Unit tests

Test:

- unit parsing;
- scan slicing;
- missing point-count fallback;
- index validation;
- numeric conversion;
- zlib compression;
- Base64;
- byte order;
- XML escaping;
- offset tracking;
- checksum generation;
- provenance;
- output precision;
- error taxonomy.

## 28.2 Synthetic NetCDF fixtures

Generate tiny files covering:

- one scan;
- many scans;
- empty scan;
- zero scans;
- missing optional metadata;
- seconds;
- minutes;
- float32;
- float64;
- integer mass/intensity types if encountered;
- invalid indexes;
- truncated point arrays;
- unusual metadata.

## 28.3 Golden tests

Maintain small mzML/mzXML golden files for stable, deterministic cases.

Avoid golden tests that fail solely because timestamps or nondeterministic metadata changed.

## 28.4 Regression tests

Every real-world bug gets a permanent regression test.

## 28.5 Fuzz tests

Use native Go fuzzing on:

- unit parsing;
- variable mapping;
- metadata parsing;
- scan-index arithmetic;
- binary encoders;
- XML escaping;
- corrupted input handling.

Never allow integer overflow or out-of-bounds slicing from hostile/corrupt inputs.

---

# 29. Security and robustness

Treat corpus files as untrusted binary inputs.

Requirements:

- check integer conversions;
- check overflow in `start + count`;
- bounded allocations;
- no panic on malformed source;
- no path traversal when deriving output names;
- no overwriting unrelated files;
- safe temporary file permissions;
- context cancellation;
- handle disk-full errors;
- avoid decompression bombs if compressed NetCDF4 input is encountered;
- explicit maximums where needed, configurable for legitimate large files.

A malformed file must fail cleanly and produce a report entry.

---

# 30. Atomic output behavior

Write to a temporary file in the target filesystem:

```text
sample.mzML.cdf2ms.tmp-<id>
```

Then:

1. flush;
2. close;
3. optionally fsync depending on durability mode;
4. validate minimum structural integrity;
5. atomically rename to final output.

On failure:

- remove temporary file by default;
- optionally retain with `--keep-failed-output`.

---

# 31. Naming behavior

By default:

```text
/path/run.cdf
→ output-dir/run.mzML
```

and:

```text
/path/run.CDF
→ output-dir/run.mzML
```

Recursive mode should preserve directory hierarchy unless `--flat` is explicitly requested.

Detect filename collisions in flat mode before starting conversion.

---

# 32. Determinism

Where possible, the same source + same converter version + same options should generate identical output bytes.

Provide:

```text
--deterministic
```

This mode should:

- omit or normalize conversion timestamps;
- use deterministic XML ordering;
- use deterministic compression settings;
- use stable IDs;
- use stable metadata ordering.

This is useful for archive migration and reproducibility.

---

# 33. CLI UX

Example:

```text
$ cdf2ms inspect sample.cdf

Source:          sample.cdf
NetCDF:          classic / CDF-2
Scans:           9,865
Points:          554,826
RT:              0.000–1873.840 s
RT source unit:  seconds
Mass range:      35–650
Schema:          sha256:...
Vendor family:   ...
Capabilities:
  ✓ scan_index
  ✓ point_count
  ✓ mass_values
  ✓ intensity_values
  ✓ total_intensity

Warnings: 0
```

Batch summary:

```text
Files discovered:      384,271
Converted:             384,190
Converted w/warnings:       73
Failed:                      8
Skipped:                     0

Scans processed:     4,827,194,221
Points processed:   ...
Elapsed:            ...
Throughput:         ... files/s
```

Progress output must be usable in a normal terminal and suppressible:

```text
--quiet
--progress auto|always|never
```

Do not require a TTY.

---

# 34. Configuration

CLI flags are authoritative.

Optional config file support may be added using a simple format only if it materially helps batch operation.

Avoid a heavy configuration framework.

Environment variables may exist for operational defaults but must be documented.

---

# 35. Slurm/HPC compatibility

The tool must work naturally in HPC environments.

Requirements:

- no daemon required;
- no local database required;
- no network access required for conversion;
- static or near-static binary;
- noninteractive execution;
- structured output;
- sensible exit codes;
- deterministic sharding.

Add file-list input:

```bash
cdf2ms convert --file-list shard-003.txt ...
```

Add deterministic sharding if useful:

```bash
cdf2ms files shard /archive/netcdf \
  --shards 100 \
  --output shards/
```

or:

```bash
cdf2ms convert /archive/netcdf \
  --shard-index 3 \
  --shard-count 100
```

Hash-based sharding is preferable to order-dependent sharding.

---

# 36. Exit codes

Define stable process exit behavior.

Suggested:

```text
0  all requested work successful
1  one or more conversion failures
2  CLI/configuration error
3  source discovery error
4  output/reporting infrastructure failure
```

Document exact semantics.

---

# 37. Documentation

Create:

```text
README.md
docs/architecture.md
docs/andi-compatibility.md
docs/mzml-mapping.md
docs/mzxml-mapping.md
docs/validation.md
docs/corpus-audit.md
docs/hpc.md
docs/troubleshooting.md
```

`docs/andi-compatibility.md` should become a living compatibility matrix containing observed source layouts and known quirks.

---

# 38. CI

At minimum:

```text
go fmt
go vet
go test ./...
go test -race ./...
go test fuzz smoke cases
go build
CGO_ENABLED=0 go build
```

Add static analysis with a well-supported Go linter if already standard in the surrounding repositories.

CI should generate and validate tiny synthetic mzML/mzXML files.

If independent external validators can be installed reliably in CI, add a separate compatibility job.

Do not make every normal unit-test run download large external tools.

---

# 39. Release artifacts

Produce binaries for at least:

```text
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
```

if dependencies permit.

Release should include:

- binaries;
- SHA-256 checksums;
- version;
- changelog;
- reproducible build metadata where practical.

---

# 40. Versioning

Use semantic versioning.

Embed version metadata at build time.

Example:

```bash
cdf2ms version
```

Output:

```text
cdf2ms 0.1.0
commit: abc1234
built: ...
go: ...
```

---

# 41. Implementation phases

## Phase 0 — repository and technical spike

Deliver:

- Go module;
- CLI skeleton;
- NetCDF abstraction;
- dependency evaluation;
- one known source file opened;
- `inspect` skeleton;
- `CGO_ENABLED=0` build.

Exit criteria:

A real corpus file can be opened and key arrays identified.

---

## Phase 1 — corpus auditor

Implement:

- recursive discovery;
- header inspection;
- structural fingerprints;
- vendor/metadata fingerprints;
- JSONL report;
- aggregate summary;
- error codes;
- parallel file auditing.

Exit criteria:

Can audit a large directory tree safely and produce a useful fingerprint distribution.

This phase should happen before overfitting the reader.

---

## Phase 2 — normalized ANDI reader

Implement:

- metadata;
- scan count;
- indexes;
- point counts;
- RT normalization;
- m/z;
- intensity;
- optional TIC;
- typed warnings/errors;
- streaming scan iterator.

Exit criteria:

Representative source files can be read spectrum-by-spectrum and compared to direct NetCDF values.

---

## Phase 3 — mzML writer

Implement:

- streaming XML;
- binary arrays;
- compression;
- precision;
- CV mapping;
- metadata/provenance;
- indexed output if validated;
- schema compatibility tests.

Exit criteria:

Independent readers open generated mzML and numerical comparison passes.

---

## Phase 4 — mzXML writer

Implement:

- scan XML;
- interleaved peaks;
- byte order;
- compression;
- precision;
- metadata/provenance;
- indexing where appropriate;
- compatibility tests.

Exit criteria:

Independent readers open generated mzXML and numerical comparison passes.

---

## Phase 5 — batch conversion

Implement:

- recursive mode;
- worker pool;
- temp + atomic rename;
- skip/overwrite/resume;
- reports;
- progress;
- failure isolation;
- signal handling.

Exit criteria:

Large directory conversion can run unattended.

---

## Phase 6 — numerical validator

Implement:

- read-back path;
- scan-by-scan compare;
- precision-aware tolerances;
- aggregate summaries;
- mismatch diagnostics.

Exit criteria:

Conversion can prove data preservation rather than only XML validity.

---

## Phase 7 — corpus regression pipeline

Implement:

- fixture selection;
- full-corpus verification;
- failure classification;
- compatibility matrix generation.

Exit criteria:

Every structural fingerprint is represented by tests or explicitly documented as unsupported/broken.

---

## Phase 8 — optimization and production hardening

Profile and optimize:

- I/O;
- allocations;
- compression;
- concurrency;
- NetCDF slicing.

Add:

- fuzz tests;
- race tests;
- corruption handling;
- HPC sharding;
- release automation.

Exit criteria:

Production release candidate.

---

# 42. Acceptance criteria

The first production-ready release is complete when all of the following are true:

1. `CGO_ENABLED=0 go build ./...` succeeds for the normal converter.
2. `cdf2ms inspect` works on representative real files.
3. `cdf2ms audit` can process the large archive without crashing on individual bad files.
4. Corpus fingerprinting identifies structural classes deterministically.
5. The converter streams source files with bounded memory.
6. mzML files open in at least one independent mature reader.
7. mzXML files open in at least one independent mature reader.
8. mzML output validates against the selected official schema where applicable.
9. mzXML output validates against the selected schema where applicable.
10. Numerical read-back preserves scan count and point count.
11. m/z and intensity arrays match exactly or within documented precision-derived tolerances.
12. Retention times are unit-normalized correctly.
13. Missing scientific metadata is not fabricated.
14. Source checksum and converter provenance are recorded.
15. Parallel conversion works with configurable worker count.
16. Partial outputs are never mistaken for completed outputs.
17. Batch conversion produces JSONL reports with stable error codes.
18. Full-corpus testing can be resumed and summarized.
19. Every discovered valid structural family is either supported or explicitly reported as unsupported.
20. Real-world failures added during development become regression tests.
21. `go test -race ./...` passes.
22. CLI help and operational documentation are complete.

---

# 43. Recommended implementation principles

## Keep it simple

This is a focused scientific file converter, not a distributed platform.

Prefer:

- standard library;
- small interfaces;
- explicit code;
- direct streaming;
- typed structs;
- simple worker pools.

Avoid:

- unnecessary microservices;
- databases;
- message queues;
- plugin frameworks;
- generic workflow engines;
- reflection-heavy abstractions.

## Optimize based on measurements

The corpus is large enough to benchmark reality.

Do not prematurely optimize theoretical bottlenecks.

## Treat weird data as evidence

When a file fails:

- inspect it;
- classify it;
- preserve the failure case;
- improve support if the file is valid;
- report corruption clearly if it is not.

## Protect scientific integrity

Correctness is more important than silently producing a file.

When information is ambiguous, warn or fail according to policy instead of guessing.

---

# 44. Initial implementation tasks for PI

PI should begin by:

1. inspect the current workspace and repository conventions;
2. locate this spec in the user's Downloads directory;
3. copy it into `docs/specs/` in the implementation repository;
4. create the repository/module if no suitable repository already exists;
5. research the current official mzML/mzXML schemas and exact binary encoding requirements;
6. evaluate the current native-Go NetCDF libraries against `CGO_ENABLED=0`;
7. create a technical spike that opens a real NetCDF file;
8. implement `inspect`;
9. implement `audit` before building assumptions around one schema;
10. add unit/synthetic tests;
11. implement the normalized reader;
12. implement mzML;
13. implement mzXML;
14. implement numerical validation;
15. implement batch conversion;
16. run tests continuously;
17. run corpus audit/verification on whatever local corpus paths are available;
18. fix discovered compatibility cases iteratively;
19. document all unsupported layouts;
20. continue until the acceptance criteria are satisfied or a concrete external blocker is proven.

PI must not stop after writing a plan.

PI must show command output, test output, and relevant benchmark/validation results while working.

---

# 45. Definition of done

The project is done for the first production release when it can be handed a directory tree containing a large historical ANDI/MS NetCDF collection and can:

```bash
cdf2ms audit /archive/netcdf --recursive --report audit.jsonl
```

then:

```bash
cdf2ms convert /archive/netcdf \
  --recursive \
  --format mzml \
  --output-dir /archive/mzml \
  --jobs 24 \
  --report conversion.jsonl
```

and:

```bash
cdf2ms verify-corpus /archive/netcdf \
  --format mzml \
  --jobs 24 \
  --report verification.jsonl
```

with bounded memory, explicit failures, reproducible provenance, and numerical evidence that the spectral data in the generated files matches the source data.

The same pipeline must work for mzXML.

The final product should be boring to operate, easy to deploy, scientifically defensible, and fast enough to process hundreds of thousands of historical files in parallel on ordinary servers or HPC infrastructure.
