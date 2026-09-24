# Encoding notes

The two output formats have *opposite* binary byte orders, different base64
containers, and different spectrum elements. These notes capture the exact
decisions the writers make so the implementation and the schemas stay in lockstep.

## Binary arrays

| | mzML | mzXML |
|---|---|---|
| Byte order | **little-endian** | **big-endian** (network order) |
| Precision | `32-bit float` / `64-bit double` | `32-bit` / `64-bit` |
| Auto | f32 iff every value round-trips exactly | same |
| Base64 | payload in a nested `<binary>` child | payload is the `<peaks>` element's own text |
| Layout | one array per `binaryDataArray` (m/z and intensity separate) | interleaved `m/z-int` pairs in one array |
| Compression | `zlib` on the binary array, `encodedLength` reflects the compressed+base64 length | `compressionType="zlib"`, `compressedLen` is the *pre-base64 compressed byte length* |

## Spectrum element and index

- mzML: `<spectrum>`, id with `index=`, count in `<spectrumList count>`.
- mzXML: `<scan>` (mzXML's form). The indexed envelope is `<index>/<indexOffset>/<sha1>`
  beside `<msRun>` inside root `<mzXML>` — not `<indexedListmzXML>/<indexList>`.
- mzXML `indexOffset` is schema-required; a document without an index writes
  `xsi:nil="true"` rather than omitting it.
- mzXML `parentFile/@fileSha1` is the SHA-1 of the source (schema-required,
  exactly 40 hex chars). Missing source digests are computed in one pass that
  yields both SHA-1 and SHA-256.

## Controlled vocabulary

- mzML CV terms are emitted only when the accession/name pair exists in the
  current PSI-MS ontology (`pkg/mzml/cv.go`, pinned to version `4.1.261`). Terms
  absent from the current ontology are never emitted.
- mzXML instrument identity is derived only from explicit wording; a missing
  identity omits `<msInstrument>` (warning `MZXML_INSTRUMENT_UNREPORTED`), and a
  placeholder identity is recorded (`MZXML_INSTRUMENT_PLACEHOLDER`).

## Scan numbering

- mzML `spectrumID` always includes `index=` so IDs are unique and ordered.
- mzXML `scan/@num` is decided once on the first spectrum: source numbers when
  positive and strictly increasing, otherwise 1-based ordinals with a recorded
  warning. A 0-based source shifts the whole document to 1-based ordinals.

## Provenance

- mzXML `<dataProcessing>` collapses to ONE `<comment>` element, because the
  schema model requires a `processingOperation` (with an optional `comment`) per
  repetition — two comments in one repetition is invalid.
- Both writers stamp the converter name, version, timestamp, and RT-unit origin.
- The conversion time lives only in provenance (`cdf2ms:converted_at_utc`).
  mzML `run@startTimeStamp` is the acquisition time, taken from the source's
  `experiment_date_time_stamp` (`YYYYMMDDhhmmss±hhmm`) and written in UTC. It is
  omitted when the source states none or states one without a UTC offset
  (`ANDI_ACQUISITION_TIME_UNUSABLE`).
