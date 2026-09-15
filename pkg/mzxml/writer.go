// Package mzxml writes mzXML 3.2 documents.
//
// # Normative sources
//
// The normative schema is the set published at
// http://sashimi.sourceforge.net/schema_revision/mzXML_3.2/ (root
// mzXML_idx_3.2.xsd). Those files are no longer served by the mzXML project, so
// scripts/fetch_mzxml_schema.sh recovers them from archived snapshots of the
// official URLs into testdata/schema, where they are committed together with a
// README that records the two repairs needed to load them offline. The emitted
// element and attribute contract was additionally cross-checked against
// ProteoWizard's Serializer_mzXML.cpp, the reference implementation every
// consumer in the ecosystem is built against. Both sources agree on the points
// that are commonly misunderstood:
//
//   - mzXML 3.2 still uses <scan> elements. <spectrum> belongs to mzML; the 3.x
//     revisions of mzXML did not rename it, and the official 3.2 schema has no
//     spectrum element at all.
//   - binary peak arrays are big-endian (byteOrder="network"), always, with no
//     negotiation - the opposite of mzML, whose arrays are little-endian.
//   - m/z and intensity are stored interleaved in one <peaks> array
//     (contentType="m/z-int"), not as two arrays.
//   - compressionType, compressedLen, byteOrder and contentType are all required
//     on <peaks>; compressedLen is the compressed byte count before base64.
//   - parentFile/@fileSha1 is required and restricted to exactly 40 characters,
//     so the source digest must really be computed.
//
// # Unknown values and provenance
//
// mzXML has no "unknown" marker, and <msInstrument> demands manufacturer, model,
// ionisation, analyzer, detector and acquisition software whenever it is
// present. The literal "Unknown" is what the reference implementation writes for
// missing instrument components (pwiz LegacyAdapter_Instrument), so this writer
// uses the same placeholder, records a MZXML_INSTRUMENT_PLACEHOLDER warning for
// every substituted component, and omits <msInstrument> entirely when nothing at
// all is known about the instrument.
//
// Optional facts are omitted rather than invented: centroided, polarity,
// retentionTime, lowMz/highMz, base-peak and TIC attributes are written only
// when the source actually stated them. <dataProcessing> is required by the
// schema, so it carries the conversion provenance (tool, source digests, unit
// resolution, scan plan) as processingOperation and comment entries.
//
// # Count integrity
//
// <msRun scanCount> is written from the run's declared spectrum count. If the
// number of spectra actually written differs, Close fails with
// msdata.ErrCountMismatch and leaves the document at <path>.partial instead of
// publishing a file whose header lies about its content. An empty run cannot be
// represented at all in mzXML (scan has minOccurs=1), so it is rejected rather
// than written as a schema-invalid stub.
package mzxml

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// Namespace is the mzXML 3.2 target namespace.
const Namespace = "http://sashimi.sourceforge.net/schema_revision/mzXML_3.2"

// SchemaLocation is the xsi:schemaLocation value written into the document root,
// pointing at the indexed schema exactly as the reference serializer does.
const SchemaLocation = Namespace + " http://sashimi.sourceforge.net/schema_revision/mzXML_3.2/mzXML_idx_3.2.xsd"

// Precision selects the binary array element type.
type Precision int

const (
	// PrecisionAuto writes 32-bit arrays when every value round-trips through
	// float32 exactly, and 64-bit otherwise.
	PrecisionAuto Precision = iota
	// PrecisionF32 always writes 32-bit arrays and warns about values that do not
	// fit instead of silently degrading them.
	PrecisionF32
	// PrecisionF64 always writes 64-bit arrays.
	PrecisionF64
)

// Options configures a Writer.
type Options struct {
	// Precision controls the binary array width.
	Precision Precision
	// Compress zlib-compresses the interleaved peak arrays.
	Compress bool
	// CompressionLevel is a compress/zlib level; 0 keeps the zlib default.
	CompressionLevel int
	// DisableIndex suppresses the byte-offset index. The document stays valid:
	// indexOffset is then written as xsi:nil="true".
	DisableIndex bool
	// SoftwareName and SoftwareVersion identify the converter in
	// <software type="conversion">.
	SoftwareName    string
	SoftwareVersion string
	// Timestamp is the conversion timestamp recorded in provenance.
	Timestamp time.Time
	// ExtraComments appends free-text provenance lines to <dataProcessing>.
	ExtraComments []string
	// ComputeSourceSHA1 controls whether a missing SourceFile.SHA1 is computed
	// by streaming the source file once (default true when nil).
	ComputeSourceSHA1 *bool
}

// Stats counts what a Writer produced.
type Stats struct {
	Spectra    int
	Points     int64
	F32Arrays  int
	F64Arrays  int
	Compressed int
	Empty      int
	// Bytes is the size of the finished document.
	Bytes int64
	// Indexed reports whether a scan index was written.
	Indexed bool
	// SourceSHA1 is the SHA-1 of the source file written to parentFile.
	SourceSHA1 string
	// Warnings holds writer-level diagnostics.
	Warnings []msdata.Diagnostic
}

// scanNumbering is the document-wide choice of values for scan/@num.
type scanNumbering int

const (
	numberingUnset scanNumbering = iota
	numberingSource
	numberingOrdinal
)

type indexEntry struct {
	scanNumber int64
	offset     int64
}

// Writer streams a mzXML 3.2 document.
//
// Spectra must be written in ascending Index order. The document prologue is
// buffered until the first spectrum because <msRun startTime> is taken from it;
// peak data is never buffered beyond a single spectrum.
// Writer implements msdata.SpectrumWriter.
var _ msdata.SpectrumWriter = (*Writer)(nil)

type Writer struct {
	path     string
	tmp      string
	fh       *os.File
	bw       *bufio.Writer
	opts     Options
	run      *msdata.Run
	src      msdata.SourceFile
	declared int
	stats    Stats

	// pos counts every byte handed to the buffered writer, which equals the
	// on-disk offset once flushed. The index depends on this being exact.
	pos      int64
	sha      hash.Hash
	hashing  bool
	head     bytes.Buffer
	headDone bool

	index         []indexEntry
	lastNum       int64
	indexDisabled bool
	numbering     scanNumbering
	startTime     string
	endTime       string
	closed        bool
}

// Create opens path for writing and prepares the document prologue.
//
// The source SHA-1 required by <parentFile> is taken from src.SHA1 when present
// and otherwise computed by streaming the source file, because the schema
// restricts fileSha1 to exactly 40 characters and forbids leaving it out.
func Create(path string, run *msdata.Run, src msdata.SourceFile, opts Options) (*Writer, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: mzxml: run metadata is required", msdata.ErrValidationFailed)
	}
	if opts.SoftwareName == "" {
		opts.SoftwareName = "cdf2ms"
	}
	if opts.SoftwareVersion == "" {
		opts.SoftwareVersion = "dev"
	}
	if opts.Timestamp.IsZero() {
		opts.Timestamp = time.Now().UTC()
	}
	computeSHA1 := opts.ComputeSourceSHA1 == nil || *opts.ComputeSourceSHA1
	if len(src.SHA1) != 40 && computeSHA1 {
		if src.Path == "" {
			return nil, msdata.WrapError(msdata.CodeMZXMLValidationFailed,
				errors.New("source path is unknown"),
				"mzxml: cannot compute the required parentFile/@fileSha1")
		}
		sum, err := fileSHA1(src.Path)
		if err != nil {
			return nil, msdata.WrapError(msdata.CodeMZXMLValidationFailed, err,
				"mzxml: computing source SHA-1 for %s", src.Path)
		}
		src.SHA1 = sum
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("%w: mzxml: creating %s: %v", msdata.ErrValidationFailed, filepath.Dir(path), err)
	}
	tmp := path + ".writing"
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, msdata.WrapError(msdata.CodeOutputWriteFailed, err, "creating %s", path)
	}
	w := &Writer{
		path: path, tmp: tmp, fh: fh, bw: bufio.NewWriterSize(fh, 1<<20),
		opts: opts, run: run, src: src, declared: run.ScanCount,
		sha: sha1.New(), hashing: true,
	}
	return w, nil
}

// Stats returns the writer counters.
func (w *Writer) Stats() Stats { return w.stats }

// Summary reports the write in format-neutral terms for batch reports and
// provenance records.
func (w *Writer) Summary() msdata.WriteSummary {
	return msdata.WriteSummary{
		Format:       "mzXML",
		Version:      "3.2",
		Path:         w.path,
		Bytes:        w.stats.Bytes,
		Spectra:      int64(w.stats.Spectra),
		Points:       w.stats.Points,
		Indexed:      w.stats.Indexed,
		Compressed:   w.stats.Compressed > 0,
		F32Arrays:    w.stats.F32Arrays,
		F64Arrays:    w.stats.F64Arrays,
		EmptySpectra: w.stats.Empty,
		SourceSHA1:   w.stats.SourceSHA1,
		SourceSHA256: w.src.SHA256,
		Warnings:     w.stats.Warnings,
	}
}

// Write appends one spectrum as a <scan> element.
func (w *Writer) Write(sp *msdata.Spectrum) error {
	if w.closed {
		return errors.New("mzxml: writer already closed")
	}
	if sp == nil {
		return errors.New("mzxml: nil spectrum")
	}
	if sp.Index != w.stats.Spectra {
		return msdata.WrapError(msdata.CodeMZXMLValidationFailed,
			fmt.Errorf("spectrum index %d arrived after %d spectra", sp.Index, w.stats.Spectra),
			"mzxml: spectra must be written in order")
	}
	if err := w.ensurePrologue(sp); err != nil {
		return err
	}

	n := len(sp.MZ)
	if len(sp.Intensity) < n {
		n = len(sp.Intensity)
	}
	scanNum, err := w.scanNumber(sp)
	if err != nil {
		return err
	}
	// Write the indentation first so the index offset lands exactly on "<scan".
	if _, err := w.write("      "); err != nil {
		return err
	}
	w.index = append(w.index, indexEntry{scanNumber: scanNum, offset: w.pos})

	var sb strings.Builder
	sb.WriteString("<scan")
	sb.WriteString(` num="` + strconv.FormatInt(scanNum, 10) + `"`)
	if st := scanType(sp); st != "" {
		sb.WriteString(` scanType="` + st + `"`)
	}
	if sp.Centroid.Known() {
		sb.WriteString(` centroided="` + boolAttr(sp.Centroid == msdata.CentroidCentroided) + `"`)
	}
	msLevel := 1
	if sp.MSLevel != nil && *sp.MSLevel > 0 {
		msLevel = *sp.MSLevel
	}
	sb.WriteString(` msLevel="` + strconv.Itoa(msLevel) + `"`)
	sb.WriteString(` peaksCount="` + strconv.Itoa(n) + `"`)
	if p := polarityAttr(sp.Polarity); p != "" {
		sb.WriteString(` polarity="` + p + `"`)
	}
	if sp.RetentionTimeSet {
		if d, ok := formatDuration(sp.RetentionTime); ok {
			sb.WriteString(` retentionTime="` + d + `"`)
		} else {
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeMZXMLValidationFailed,
				Message: "retention time is not representable as an xs:duration",
				Detail:  fmt.Sprintf("scan index %d value %v; the retentionTime attribute was omitted", sp.Index, sp.RetentionTime),
			})
		}
	}
	addNum(&sb, "collisionEnergy", sp.CollisionEn)
	addNum(&sb, "lowMz", sp.LowestMZ)
	addNum(&sb, "highMz", sp.HighestMZ)
	addNum(&sb, "basePeakMz", sp.BasePeakMZ)
	addNum(&sb, "basePeakIntensity", sp.BasePeakInt)
	addNum(&sb, "totIonCurrent", sp.TIC)
	sb.WriteString(` msInstrumentID="1"` + ">\n")
	if _, err := w.write(sb.String()); err != nil {
		return err
	}

	if err := w.writePrecursors(sp); err != nil {
		return err
	}
	if err := w.writePeaks(sp.MZ[:n], sp.Intensity[:n]); err != nil {
		return err
	}
	if _, err := w.write("      </scan>\n"); err != nil {
		return err
	}
	w.stats.Spectra++
	w.stats.Points += int64(n)
	if n == 0 {
		w.stats.Empty++
	}
	return nil
}

func (w *Writer) write(s string) (int, error) {
	n, err := w.bw.WriteString(s)
	w.pos += int64(n)
	if w.hashing {
		w.sha.Write([]byte(s))
	}
	return n, err
}

// ensurePrologue flushes the buffered document header once the first spectrum
// (and therefore the run start time) is known.
func (w *Writer) ensurePrologue(first *msdata.Spectrum) error {
	if w.headDone {
		return nil
	}
	w.headDone = true
	if first.RetentionTimeSet {
		if d, ok := formatDuration(first.RetentionTime); ok {
			w.startTime = d
		}
	}
	if et, ok := w.runEndTime(); ok {
		w.endTime = et
	}
	h := &w.head
	h.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	h.WriteString(`<mzXML` + "\n")
	h.WriteString(`    xmlns="` + Namespace + `"` + "\n")
	h.WriteString(`    xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` + "\n")
	h.WriteString(`    xsi:schemaLocation="` + SchemaLocation + `">` + "\n")
	h.WriteString("    <msRun")
	h.WriteString(` scanCount="` + strconv.Itoa(w.declared) + `"`)
	if w.startTime != "" {
		h.WriteString(` startTime="` + w.startTime + `"`)
	}
	if w.endTime != "" {
		h.WriteString(` endTime="` + w.endTime + `"`)
	}
	h.WriteString(">\n")
	w.writeParentFile(h)
	w.writeInstrument(h)
	w.writeDataProcessing(h)
	_, err := w.write(h.String())
	return err
}

func (w *Writer) writeParentFile(h *bytes.Buffer) {
	h.WriteString("      <parentFile")
	h.WriteString(` fileName="` + xmlAttr(fileLocation(w.src)) + `"`)
	h.WriteString(` fileType="RAWData"`)
	h.WriteString(` fileSha1="` + xmlAttr(w.src.SHA1) + `"/>` + "\n")
	w.stats.SourceSHA1 = w.src.SHA1
}

// writeInstrument emits <msInstrument>, whose schema sequence requires all six
// components. Missing ones become the reference implementation's "Unknown"
// placeholder, each recorded as a warning; when nothing is known the element is
// omitted entirely rather than filled with placeholders.
func (w *Writer) writeInstrument(h *bytes.Buffer) {
	in := w.run.Instrument
	known := in.Manufacturer != "" || in.Model != "" || in.Name != "" ||
		in.IonizationMode != "" || in.DetectorType != ""
	if !known {
		w.recordWarning(msdata.Diagnostic{
			Code:    msdata.CodeMZXMLInstrumentUnreported,
			Message: "the source reports no instrument identity",
			Detail:  "<msInstrument> was omitted; mzXML requires every component when the element is present",
		})
		return
	}
	manufacturer := orUnknown(in.Manufacturer)
	model := orUnknown(firstNonEmpty(in.Model, in.Name))
	ionisation := orUnknown(in.IonizationMode)
	// ANDI has no mass-analyzer variable. Deriving one from
	// test_resolution_type ("Constant Resolution") or test_scan_function
	// ("Mass Scan") would assert a hardware fact the source never stated, so the
	// analyzer is only filled in when the instrument wording itself names one.
	analyzer := orUnknown(analyzerHint(in))
	detector := orUnknown(in.DetectorType)
	for _, v := range []struct{ name, val string }{
		{"msManufacturer", manufacturer}, {"msModel", model},
		{"msIonisation", ionisation}, {"msMassAnalyzer", analyzer},
		{"msDetector", detector},
	} {
		if v.val == placeholder {
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeMZXMLInstrumentPlaceholder,
				Message: v.name + " is not reported by the source",
				Detail:  `written as the literal "Unknown", matching the reference implementation's legacy adapter`,
			})
		}
	}
	h.WriteString(`      <msInstrument msInstrumentID="1">` + "\n")
	ontologyEntry(h, "msManufacturer", manufacturer)
	ontologyEntry(h, "msModel", model)
	ontologyEntry(h, "msIonisation", ionisation)
	ontologyEntry(h, "msMassAnalyzer", analyzer)
	ontologyEntry(h, "msDetector", detector)
	software(h, "acquisition", "unknown", orUnknownVersion(in.SoftwareVer))
	h.WriteString("      </msInstrument>\n")
}

const placeholder = "Unknown"

func (w *Writer) writeDataProcessing(h *bytes.Buffer) {
	h.WriteString("      <dataProcessing>\n")
	software(h, "conversion", w.opts.SoftwareName, w.opts.SoftwareVersion)
	ops := []struct{ name, value string }{
		{"ANDI/MS to mzXML 3.2 conversion", w.opts.SoftwareName + " " + w.opts.SoftwareVersion},
		{"retention time unit", w.run.RetentionTimeUnit},
		{"scan partitioning", metaValue(w.run, "derived:scan_plan")},
		{"MS level origin", metaValue(w.run, "derived:ms_level")},
		{"centroid state origin", metaValue(w.run, "derived:centroid")},
		{"polarity origin", metaValue(w.run, "derived:polarity")},
	}
	for _, op := range ops {
		if op.value == "" {
			op.value = "unreported"
		}
		h.WriteString(`      <processingOperation name="` + xmlAttr(op.name) +
			`" value="` + xmlAttr(op.value) + `"/>` + "\n")
	}
	comments := []string{
		"converted " + w.opts.Timestamp.UTC().Format(time.RFC3339Nano) + " by " +
			w.opts.SoftwareName + "/" + w.opts.SoftwareVersion,
		"source file " + w.src.Name + " (" + w.src.Encoding + ", " +
			strconv.FormatInt(w.src.Size, 10) + " bytes)",
		"source SHA-256 " + orUnreported(w.src.SHA256),
		"source SHA-1 " + orUnreported(w.src.SHA1),
		"retention time unit origin: " + orUnreported(w.run.RetentionTimeUnitOrigin),
	}
	if w.endTime == "" {
		comments = append(comments,
			"msRun endTime omitted: the source does not declare an acquisition end time and the last retention time is unknown while the header is written")
	}
	comments = append(comments, w.opts.ExtraComments...)
	// mzXML's dataProcessing model repeats (processingOperation, comment?) with
	// the operation required in every repetition, so one comment element per
	// operation is the most that can follow it. Provenance is therefore emitted as
	// a single comment whose lines carry each fact, rather than as a trailing block
	// of comments, which the schema rejects.
	if len(comments) > 0 {
		h.WriteString("        <comment>" + xmlText(strings.Join(comments, "\n")) + "</comment>\n")
	}
	h.WriteString("      </dataProcessing>\n")
}

func (w *Writer) writePrecursors(sp *msdata.Spectrum) error {
	if sp.PrecursorMZ == nil {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("        <precursorMz")
	if sp.PrecursorInt != nil {
		sb.WriteString(` precursorIntensity="` + formatNum(*sp.PrecursorInt) + `"`)
	} else {
		// precursorIntensity is a required attribute in mzXML; 0 is what the
		// reference serializer writes when the source does not report it.
		sb.WriteString(` precursorIntensity="0"`)
	}
	if sp.PrecursorZ != nil && *sp.PrecursorZ > 0 {
		sb.WriteString(` precursorCharge="` + strconv.Itoa(*sp.PrecursorZ) + `"`)
	}
	sb.WriteString(">" + formatNum(*sp.PrecursorMZ) + "</precursorMz>\n")
	_, err := w.write(sb.String())
	return err
}

// writePeaks encodes the interleaved big-endian array mzXML mandates.
func (w *Writer) writePeaks(mz, inten []float64) error {
	n := len(mz)
	if n == 0 {
		_, err := w.write(`        <peaks compressionType="none" compressedLen="0" byteOrder="network" contentType="m/z-int" xsi:nil="true"></peaks>` + "\n")
		return err
	}
	use32 := false
	switch w.opts.Precision {
	case PrecisionF32:
		use32 = true
		if bad := countNonRepresentable(mz) + countNonRepresentable(inten); bad > 0 {
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeNumericPrecisionLoss,
				Message: fmt.Sprintf("%d values are not exactly representable in 32-bit float", bad),
				Detail:  "--precision=f32 was requested; use auto or f64 for a lossless encoding",
			})
		}
	case PrecisionF64:
	default:
		use32 = allFloat32Exact(mz) && allFloat32Exact(inten)
	}
	elem := 8
	if use32 {
		elem = 4
	}
	raw := make([]byte, elem*2*n)
	off := 0
	for i := 0; i < n; i++ {
		if use32 {
			binary.BigEndian.PutUint32(raw[off:], math.Float32bits(float32(mz[i])))
			off += 4
			binary.BigEndian.PutUint32(raw[off:], math.Float32bits(float32(inten[i])))
			off += 4
		} else {
			binary.BigEndian.PutUint64(raw[off:], math.Float64bits(mz[i]))
			off += 8
			binary.BigEndian.PutUint64(raw[off:], math.Float64bits(inten[i]))
			off += 8
		}
	}
	if use32 {
		w.stats.F32Arrays++
	} else {
		w.stats.F64Arrays++
	}
	binaryBytes := raw
	if w.opts.Compress {
		binaryBytes = zlibBytes(raw, w.opts.CompressionLevel)
		w.stats.Compressed++
	}
	encoded := base64.StdEncoding.EncodeToString(binaryBytes)
	var sb strings.Builder
	sb.WriteString("        <peaks")
	if w.opts.Compress {
		sb.WriteString(` compressionType="zlib" compressedLen="` + strconv.Itoa(len(binaryBytes)) + `"`)
	} else {
		sb.WriteString(` compressionType="none" compressedLen="0"`)
	}
	if use32 {
		sb.WriteString(` precision="32"`)
	} else {
		sb.WriteString(` precision="64"`)
	}
	sb.WriteString(` byteOrder="network" contentType="m/z-int">`)
	sb.WriteString(encoded)
	sb.WriteString("</peaks>\n")
	_, err := w.write(sb.String())
	return err
}

// Close finishes the document, appending the index, index offset and file
// checksum.
//
// The document is published only when the number of spectra written equals the
// count declared in <msRun scanCount>. On mismatch the file is kept as
// <path>.partial and msdata.ErrCountMismatch is returned. An empty run is
// rejected: mzXML requires at least one <scan>.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if !w.headDone {
		w.fh.Close()
		os.Remove(w.tmp)
		return msdata.WrapError(msdata.CodeMZXMLValidationFailed,
			fmt.Errorf("0 spectra written, %d declared", w.declared),
			"mzxml: an empty run cannot be represented (mzXML requires at least one <scan>)")
	}
	if _, err := w.write("    </msRun>\n"); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "closing msRun in %s", w.path)
	}
	indexOffset := w.pos
	indexed := !w.opts.DisableIndex && !w.indexDisabled && len(w.index) > 0
	if indexed {
		var sb strings.Builder
		sb.WriteString("<index name=\"scan\">")
		for _, e := range w.index {
			sb.WriteString("<offset id=\"" + strconv.FormatInt(e.scanNumber, 10) + "\">" +
				strconv.FormatInt(e.offset, 10) + "</offset>")
		}
		sb.WriteString("</index>\n")
		if _, err := w.write(sb.String()); err != nil {
			w.fh.Close()
			return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "writing index to %s", w.path)
		}
	}
	var tail string
	if indexed {
		tail = "<indexOffset>" + strconv.FormatInt(indexOffset, 10) + "</indexOffset>\n"
	} else {
		tail = "<indexOffset xsi:nil=\"true\"></indexOffset>\n"
	}
	if _, err := w.write(tail); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "writing index offset to %s", w.path)
	}
	// The checksum covers the file up to and including the opening <sha1> tag.
	if _, err := w.write("<sha1>"); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "writing checksum element to %s", w.path)
	}
	w.hashing = false
	sum := w.sha.Sum(nil)
	if _, err := w.write(hexString(sum) + "</sha1>\n</mzXML>\n"); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "writing checksum to %s", w.path)
	}
	if err := w.bw.Flush(); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "flushing %s", w.path)
	}
	if err := w.fh.Sync(); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "syncing %s", w.path)
	}
	if err := w.fh.Close(); err != nil {
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "closing %s", w.path)
	}
	if w.stats.Spectra != w.declared {
		rename := w.path + ".partial"
		_ = os.Rename(w.tmp, rename)
		return msdata.WrapError(msdata.CodeCountMismatch,
			fmt.Errorf("declared %d spectra, wrote %d", w.declared, w.stats.Spectra),
			"%s: spectrum count mismatch; document kept at %s for inspection",
			filepath.Base(w.path), filepath.Base(rename))
	}
	if err := os.Rename(w.tmp, w.path); err != nil {
		return msdata.WrapError(msdata.CodeOutputRenameFailed, err, "renaming %s to %s", w.tmp, w.path)
	}
	if st, err := os.Stat(w.path); err == nil {
		w.stats.Bytes = st.Size()
	}
	w.stats.Indexed = indexed
	return nil
}

// Abort discards a document under construction.
func (w *Writer) Abort() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.bw != nil {
		w.bw.Reset(io.Discard)
	}
	if w.fh != nil {
		w.fh.Close()
	}
	return os.Remove(w.tmp)
}

// scanNumber returns the 1-based scan number mzXML requires. Source scan numbers
// are used when they are positive and strictly increasing; otherwise the writer
// falls back to ordinals and says so, because the index keys must be unique and
// ordered.
func (w *Writer) scanNumber(sp *msdata.Spectrum) (int64, error) {
	ordinal := int64(sp.Index + 1)

	// The numbering scheme is chosen once, on the first spectrum, and never
	// changes mid-document. Switching schemes part-way through would emit numbers
	// the source never used and could collide with numbers already written, and
	// scan/@num is the key consumers build lookup tables on.
	if w.numbering == numberingUnset {
		switch {
		case sp.ScanNumber == nil:
			w.numbering = numberingOrdinal
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeMZXMLScanNumberFallback,
				Message: "the source does not number scans",
				Detail:  "every scan/@num uses the 1-based ordinal",
			})
		case *sp.ScanNumber <= 0:
			w.numbering = numberingOrdinal
			detail := "every scan/@num uses the 1-based ordinal"
			if *sp.ScanNumber == 0 {
				detail = "the source numbers scans from 0, which mzXML cannot express " +
					"(scan/@num is xs:positiveInteger); every scan/@num uses the 1-based " +
					"ordinal, which equals the source number plus one while that sequence is dense"
			}
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeMZXMLScanNumberFallback,
				Message: fmt.Sprintf("the first source scan number is %d, not a positive integer", *sp.ScanNumber),
				Detail:  detail,
			})
		default:
			w.numbering = numberingSource
		}
	}

	if w.numbering == numberingOrdinal {
		w.lastNum = ordinal
		return ordinal, nil
	}

	src := ordinal
	if sp.ScanNumber != nil && *sp.ScanNumber > 0 {
		src = *sp.ScanNumber
	} else {
		// A gap in the middle of source numbering cannot be represented honestly:
		// keep the ordinal (the least inventive choice) and drop the index.
		w.indexDisabled = true
		w.recordWarning(msdata.Diagnostic{
			Code:    msdata.CodeMZXMLScanNumberFallback,
			Message: "a spectrum in the middle of the run carries no usable source scan number",
			Detail:  "the 1-based ordinal was used for that scan and the scan index is omitted",
		})
	}
	if src <= w.lastNum {
		// mzXML uses scan/@num as the key consumers index on. Renumbering part of
		// a document mid-stream would invent numbers the source never used and can
		// collide with earlier ones, so the source numbers are kept verbatim and
		// the byte-offset index is dropped instead, with a warning.
		if !w.indexDisabled {
			w.indexDisabled = true
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeMZXMLScanNumberFallback,
				Message: fmt.Sprintf("source scan numbers are not strictly increasing (saw %d after %d)", src, w.lastNum),
				Detail:  "source scan numbers are preserved verbatim and the scan index is omitted; ordered lookup would be wrong",
			})
		}
	}
	w.lastNum = src
	return src, nil
}

func (w *Writer) recordWarning(d msdata.Diagnostic) {
	if len(w.stats.Warnings) < 64 {
		w.stats.Warnings = append(w.stats.Warnings, d)
	}
}

// runEndTime uses a source-declared acquisition end time when one exists; it is
// never inferred from the last spectrum, which is not yet known here.
func (w *Writer) runEndTime() (string, bool) {
	for _, k := range []string{"global:time_end", "global:total_run_time", "var:time_end"} {
		if v, ok := w.run.Metadata[k]; ok {
			if f, ok := v.Float(); ok {
				return formatDuration(f)
			}
		}
	}
	return "", false
}

// ---- helpers ----

func ontologyEntry(h *bytes.Buffer, category, value string) {
	h.WriteString(`        <` + category + ` category="` + category +
		`" value="` + xmlAttr(value) + `"/>` + "\n")
}

func software(h *bytes.Buffer, typ, name, version string) {
	h.WriteString(`        <software type="` + typ + `" name="` + xmlAttr(name) +
		`" version="` + xmlAttr(version) + `"/>` + "\n")
}

// analyzerHint derives a free-text mass-analyzer description only from instrument
// wording that explicitly names one. ANDI carries no analyzer variable.
func analyzerHint(in msdata.Instrument) string {
	for _, raw := range []string{in.Model, in.Name, in.Comments} {
		k := strings.ToLower(strings.TrimSpace(raw))
		if k == "" {
			continue
		}
		for _, e := range []struct{ needle, name string }{
			{"quadrupole ion trap", "quadrupole ion trap"},
			{"ion trap", "ion trap"},
			{"orbitrap", "orbitrap"},
			{"ion cyclotron", "fourier transform ion cyclotron resonance"},
			{"fourier transform", "fourier transform ion cyclotron resonance"},
			{"time-of-flight", "time-of-flight"},
			{"time of flight", "time-of-flight"},
			{"magnetic sector", "magnetic sector"},
			{"quadrupole", "quadrupole"},
		} {
			if strings.Contains(k, e.needle) {
				return e.name
			}
		}
	}
	return ""
}

// addNum writes an optional numeric attribute, skipping absent and non-finite
// values rather than writing a fabricated zero.
func addNum(sb *strings.Builder, name string, v *float64) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return
	}
	sb.WriteString(" " + name + "=\"" + strconv.FormatFloat(*v, 'g', -1, 64) + "\"")
}

// scanType maps the source-declared scan function onto the mzXML scanType
// enumeration. Unknown functions leave the attribute out rather than claiming
// "Full".
func scanType(sp *msdata.Spectrum) string {
	if sp.Metadata == nil {
		return ""
	}
	v, ok := sp.Metadata["global:test_scan_function"]
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(v.String())) {
	case "mass scan":
		return "Full"
	case "selected ion storage", "single ion monitoring":
		return "SIM"
	case "multiple ion detection", "selected reaction monitoring":
		return "SRM"
	default:
		return ""
	}
}

func polarityAttr(p msdata.Polarity) string {
	switch p {
	case msdata.PolarityPositive:
		return "+"
	case msdata.PolarityNegative:
		return "-"
	}
	return ""
}

func boolAttr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// formatDuration renders seconds in the xs:duration lexical form mzXML uses
// ("PT353.43S"). Exponent notation is avoided because xs:duration requires a
// decimal, and non-finite values are rejected instead of written.
func formatDuration(seconds float64) (string, bool) {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "", false
	}
	s := strconv.FormatFloat(seconds, 'f', -1, 64)
	if strings.HasPrefix(s, "-") {
		return "-PT" + s[1:] + "S", true
	}
	return "PT" + s + "S", true
}

func zlibBytes(buf []byte, level int) []byte {
	var out bytes.Buffer
	zw, err := zlib.NewWriterLevel(&out, level)
	if err != nil {
		zw = zlib.NewWriter(&out)
	}
	if _, err := zw.Write(buf); err != nil {
		return buf
	}
	if err := zw.Close(); err != nil {
		return buf
	}
	return out.Bytes()
}

func allFloat32Exact(vals []float64) bool {
	for _, v := range vals {
		if float64(float32(v)) != v {
			return false
		}
	}
	return true
}

func countNonRepresentable(vals []float64) int {
	n := 0
	for _, v := range vals {
		if float64(float32(v)) != v {
			n++
		}
	}
	return n
}

func fileSHA1(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	buf := make([]byte, 1<<20)
	h := sha1.New()
	for {
		n, rerr := fh.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", rerr
		}
	}
	return hexString(h.Sum(nil)), nil
}

func hexString(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0xf])
	}
	return string(out)
}

func fileLocation(src msdata.SourceFile) string {
	p := src.Path
	if p == "" {
		return "file:///" + url.PathEscape(src.Name)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	return "file://" + (&url.URL{Path: abs}).EscapedPath()
}

func metaValue(run *msdata.Run, key string) string {
	if run.Metadata == nil {
		return ""
	}
	v, ok := run.Metadata[key]
	if !ok {
		return ""
	}
	return v.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return placeholder
	}
	return s
}

func orUnreported(s string) string {
	if s == "" {
		return "unreported"
	}
	return s
}

func orUnknownVersion(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func xmlAttr(s string) string {
	s = sanitizeUTF8(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '\n':
			b.WriteString("&#10;")
		case '\r':
			b.WriteString("&#13;")
		case '\t':
			b.WriteString("&#9;")
		default:
			if r < 0x20 {
				b.WriteString("&#")
				b.WriteString(strconv.Itoa(int(r)))
				b.WriteString(";")
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func xmlText(s string) string {
	s = sanitizeUTF8(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			if r < 0x20 {
				b.WriteString("&#")
				b.WriteString(strconv.Itoa(int(r)))
				b.WriteString(";")
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeUTF8 replaces invalid UTF-8 byte sequences with "?". mzXML documents
// declare UTF-8, so a source carrying a Latin-1 title must not break the file.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "?")
}
