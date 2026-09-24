# Provenance and data-integrity record

`cdf2ms` treats conversion as a *recording* task: every non-trivial decision is
written into the output or the report with a stable machine-readable code, so a
consumer can always tell what the tool knew, what it inferred, and what it
refused to guess.

## Where provenance is recorded

- **In the document**: mzML `softwareParam`/`userParam` and mzXML
  `<dataProcessing>` carry the converter name, version, timestamp, and RT-unit
  decision. mzXML additionally embeds the source SHA-1 in `parentFile/@fileSha1`.
- **In the report**: `-report-json` (aggregate) and `-report-jsonl` (one record
  per source) carry source SHA-1/SHA-256, schema and vendor fingerprints, worker
  id, elapsed time, read/write bytes, warnings, and per-output digests.
- **On the command line**: `audit` prints the RT unit origin/confidence and scan
  plan before anything is written.

## Stable codes

Diagnostics use stable codes, not message text, so scripts and dashboards can key
on them. Notable ones:

| Code | Meaning |
|------|---------|
| `ANDI_MS_LEVEL_UNKNOWN` | no `ms_level`/precursor info; every scan labelled MS1 (recorded in output provenance) |
| `ANDI_POLARITY_UNKNOWN` | polarity not stated; omitted, never assumed |
| `ANDI_CENTROID_STATE_UNKNOWN` | centroid/profile state not stated; omitted, never inferred from peak spacing |
| `ANDI_PACKING_APPLIED` | packed integers unpacked per the netCDF convention |
| `ANDI_UNKNOWN_RT_UNIT` | retention-time unit attribute unparseable |
| `ANDI_AMBIGUOUS_UNITS` | RT unit undetermined and magnitudes not decisive; re-run with `--rt-unit` |
| `ANDI_UNIT_INFERRED_FROM_MAG` | RT unit inferred from a decisive magnitude test |
| `ANDI_USED_POINT_TIME_UNIT` | RT taken from per-point time values' unit |
| `ANDI_POINT_COUNT_MISMATCH` | declared peak counts disagree with the peak arrays |
| `ANDI_POINT_COUNT_DERIVED` | `point_count` absent; counts derived from `scan_index` |
| `ANDI_SCAN_LAYOUT_ASSUMED` | neither index nor count; one-peak-per-scan assumed from array extent |
| `ANDI_SCAN_INDEX_BASE_ASSUMED` | `scan_index` starts at a non-0/1 origin; used as offset |
| `ANDI_ZERO_POINT_SCAN` | a scan has zero peaks; emitted as an empty spectrum |
| `ANDI_SCAN_NUMBERS_UNUSABLE` | the scan-number variable holds negative or fill values (e.g. `-9999`); every spectrum is numbered by ordinal |
| `ANDI_METADATA_NOT_UTF8` | a global attribute was not valid UTF-8; sanitised and reported |
| `MZXML_INSTRUMENT_UNREPORTED` | source reports no instrument identity |
| `MZXML_INSTRUMENT_PLACEHOLDER` | instrument identity was a placeholder, not a real name |
| `MZXML_SCAN_NUMBER_FALLBACK` | source scan numbers unusable; 1-based ordinals used |
| `OUTPUT_VERIFIED` | an output was re-opened and matched the source |
| `NC_UNSUPPORTED_ENCODING` | container is HDF5/NetCDF4; described but not converted |
| `SKIPPED_RESUMED` | skipped because an earlier run's output is still present and matching |
| `FAIL_FAST_ABORTED` | not started because an earlier file failed under `--fail-fast` |
| `OUTPUT_WRITE_FAILED` / `OUTPUT_RENAME_FAILED` | output could not be written / moved into place |

## Rules the tool never breaks

1. **Optional metadata stays unset.** Polarity, centroid state, isolation width,
   end times, and any value the source does not state are omitted, not invented.
   Derived fields (e.g. MS1 labelling) carry a provenance code and a warning.
2. **Numbers are never silently changed.** Packed data is unpacked per the
   convention; fill values and non-finite numbers are counted and reported;
   precision `auto` is only chosen when every value round-trips exactly; `f32`
   never silently truncates. mzML arrays are little-endian, mzXML arrays
   big-endian — never swapped silently.
3. **A wrong guess is refused, not papered over.** An undetermined RT unit is a
   hard error with a `--rt-unit` remedy. A count mismatch fails the file and
   keeps the partial document for inspection.
4. **Provenance is honest.** The RT unit origin (`declared` vs `inferred` vs
   `operator`) and its confidence are recorded, never hidden. The converter
   version, source digests, and output digests are part of the report.
