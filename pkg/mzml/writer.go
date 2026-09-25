package mzml

import (
	"bufio"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// SchemaVersion is the mzML document version this writer emits.
const SchemaVersion = "1.1.0"

// DefaultMSVersion is the PSI-MS ontology release this writer's term table was
// verified against (psi-ms.obo "data-version"). scripts/fetch_psi_ms_cv.sh
// updates it together with the test snapshot, so a document never claims a CV
// version its terms were not checked against.
const DefaultMSVersion = "4.1.261"

// MSOntologyURI is where the ontology above is published.
const MSOntologyURI = "https://raw.githubusercontent.com/HUPO-PSI/psi-ms-CV/master/psi-ms.obo"

// UnitOntologyURI is the URI published for the unit ontology used for units.
const UnitOntologyURI = "http://purl.obolibrary.org/obo/unit.obo"

// Precision selects the binary array element type.
type Precision int

const (
	// PrecisionAuto writes 32-bit arrays when every value round-trips through
	// float32 exactly, and 64-bit otherwise. ANDI peak arrays are usually
	// float32, so this is lossless and half the size.
	PrecisionAuto Precision = iota
	// PrecisionF32 always writes 32-bit float arrays. Values that do not fit are
	// reported as a warning, never silently degraded.
	PrecisionF32
	// PrecisionF64 always writes 64-bit float arrays.
	PrecisionF64
)

// Options configures a Writer.
type Options struct {
	// TempDir, when set, holds the scratch file that is renamed onto the final
	// path. Empty keeps the scratch file beside the output, where the rename is
	// atomic.
	TempDir string
	// HashOutput records the SHA-256 of the written document in the summary. The
	// digest is computed as bytes leave the writer, so it costs no extra pass.
	HashOutput bool
	// DocumentID is the mzML @id; defaults to the run ID.
	DocumentID string
	// Precision controls binary array width.
	Precision Precision
	// Compress zlib-compresses binary arrays.
	Compress bool
	// CompressionLevel is a compress/zlib level (-1 keeps the default).
	CompressionLevel int
	// MSVersion overrides the CV version written into cvList.
	MSVersion string
	// Contact information is optional; the schema does not require it, so it is
	// only written when supplied.
	ContactName        string
	ContactAffiliation string
	ContactAddress     string
	ContactEmail       string
	// SoftwareVersion is the cdf2ms build written into softwareList.
	SoftwareVersion string
	// Timestamp overrides the generated file timestamp (used by reproducible
	// builds and tests).
	Timestamp time.Time
}

// Stats counts what a writer emitted.
type Stats struct {
	Spectra       int
	Points        int64
	F32Arrays     int
	F64Arrays     int
	CompressedArr int
	Bytes         int64
	// OutputSHA256 is the digest of the document this writer produced, when the
	// caller asked for one.
	OutputSHA256 string
	// DroppedWarnings counts warnings beyond the retained cap.
	DroppedWarnings int
	// Warnings carries writer-level diagnostic codes raised during Write.
	Warnings []msdata.Diagnostic
}

// Writer emits an mzML 1.1 document.
//
// Create writes the document header (including the declared spectrum count);
// each Write appends one spectrum; Close finishes the document and verifies that
// the number of spectra actually written matches the count declared in the
// opening tag. A mismatch is an error and the file is renamed to *.partial,
// because a document whose @count disagrees with its content silently misleads
// every downstream tool.
// Writer implements msdata.SpectrumWriter.
var _ msdata.SpectrumWriter = (*Writer)(nil)

type Writer struct {
	path     string
	tmp      string
	fh       *os.File
	bw       *bufio.Writer
	hasher   *msdata.HashWriter
	opts     Options
	run      *msdata.Run
	src      msdata.SourceFile
	declared int
	stats    Stats
	written  int
	closed   bool
	start    time.Time

	swID   string
	icID   string
	dpID   string
	sfID   string
	sample string

	// noticed keys the per-file diagnostics already recorded, so a sentinel that
	// repeats on every spectrum does not burn the diagnostic budget per spectrum.
	noticed map[string]bool
}

// Create writes the mzML header to path.
func Create(path string, run *msdata.Run, src msdata.SourceFile, opts Options) (*Writer, error) {
	if run == nil {
		return nil, errors.New("mzml: run metadata is required")
	}
	if opts.MSVersion == "" {
		opts.MSVersion = DefaultMSVersion
	}
	if opts.DocumentID == "" {
		opts.DocumentID = run.ID
	}
	if opts.SoftwareVersion == "" {
		opts.SoftwareVersion = "dev"
	}
	if opts.Timestamp.IsZero() {
		opts.Timestamp = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("%w: creating %s: %v", msdata.ErrValidationFailed, filepath.Dir(path), err)
	}
	tmp := msdata.TempPathFor(path, opts.TempDir)
	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, msdata.WrapError(msdata.CodeOutputWriteFailed, err, "creating %s", path)
	}
	// The hash has to sit between the buffered writer and the file so it sees
	// every byte exactly once, including the bytes flushed during Close.
	var sink io.Writer = fh
	var hasher *msdata.HashWriter
	if opts.HashOutput {
		hasher = msdata.NewHashWriter(fh)
		sink = hasher
	}
	w := &Writer{
		path: path, tmp: tmp, fh: fh, bw: bufio.NewWriterSize(sink, 1<<20), hasher: hasher,
		opts: opts, run: run, src: src, declared: run.ScanCount, start: time.Now(),
		swID: xmlID("cdf2ms"), icID: xmlID("IC_1"), dpID: xmlID("cdf2ms_processing"),
		sfID:    xmlID("source_file_1"),
		noticed: map[string]bool{},
	}
	if err := w.writeHeader(); err != nil {
		fh.Close()
		os.Remove(tmp)
		return nil, err
	}
	return w, nil
}

// Stats returns the writer counters.
func (w *Writer) Stats() Stats { return w.stats }

// Summary reports the write in format-neutral terms for batch reports and
// provenance records.
func (w *Writer) Summary() msdata.WriteSummary {
	return msdata.WriteSummary{
		Format:       "mzML",
		Version:      SchemaVersion,
		Path:         w.path,
		Bytes:        int64(w.stats.Bytes),
		Spectra:      int64(w.stats.Spectra),
		Points:       w.stats.Points,
		Compressed:   w.stats.CompressedArr > 0,
		F32Arrays:    w.stats.F32Arrays,
		F64Arrays:    w.stats.F64Arrays,
		SourceSHA1:   w.src.SHA1,
		SourceSHA256: w.src.SHA256,
		SHA256:       w.stats.OutputSHA256,
		Warnings:     w.stats.Warnings,
	}
}

func (w *Writer) writeHeader() error {
	b := w.bw
	if _, err := b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n"); err != nil {
		return err
	}
	fmt.Fprintf(b, `<mzML xmlns=%q xmlns:xsi=%q xsi:schemaLocation=%q id=%q version=%q>`+"\n",
		NamespaceURI, XSIURI, SchemaLocation, xmlAttr(w.opts.DocumentID), SchemaVersion)

	// cvList
	contentTerms := []CVTerm{}
	if hasMSn(w.run) {
		contentTerms = append(contentTerms, TermMSnSpectrum)
	} else {
		contentTerms = append(contentTerms, TermMS1Spectrum)
	}
	fmt.Fprintf(b, "  <cvList count=\"2\">\n")
	fmt.Fprintf(b, "    <cv id=%q fullName=%q version=%q URI=%q/>\n", MSRef,
		"Proteomics Standards Initiative Mass Spectrometry Ontology", w.opts.MSVersion, MSOntologyURI)
	fmt.Fprintf(b, "    <cv id=%q fullName=%q URI=%q/>\n", UORef, "Unit Ontology", UnitOntologyURI)
	fmt.Fprintf(b, "  </cvList>\n")

	// fileDescription (required by the schema)
	fmt.Fprintf(b, "  <fileDescription>\n")
	fmt.Fprintf(b, "    <fileContent>\n")
	writeCVParam(b, "    ", TermMSLevel, "")
	if len(contentTerms) > 0 {
		writeCVParam(b, "    ", contentTerms[0], "")
	}
	if ct, ok := centroidTerm(centroidFromRun(w.run)); ok {
		writeCVParam(b, "    ", ct, "")
	}
	fmt.Fprintf(b, "    </fileContent>\n")
	fmt.Fprintf(b, "    <sourceFileList count=\"1\">\n")
	fmt.Fprintf(b, "      <sourceFile id=%q name=%q location=%q>\n", w.sfID,
		xmlAttr(orUnknown(w.src.Name)), xmlAttr(fileLocation(w.src)))
	writeCVParam(b, "      ", TermAndiMSFormat, "")
	if sum := sourceSHA256(w.src); sum != "" {
		writeCVParam(b, "      ", TermSHA256, sum)
	}
	writeCVParam(b, "      ", TermNoNativeID, "")
	fmt.Fprintf(b, "      </sourceFile>\n")
	fmt.Fprintf(b, "    </sourceFileList>\n")
	if w.opts.ContactName != "" || w.opts.ContactAffiliation != "" || w.opts.ContactEmail != "" {
		fmt.Fprintf(b, "    <contact>\n")
		if w.opts.ContactName != "" {
			writeCVParam(b, "      ", TermContactName, w.opts.ContactName)
		}
		if w.opts.ContactAffiliation != "" {
			writeCVParam(b, "      ", TermContactAffiliation, w.opts.ContactAffiliation)
		}
		if w.opts.ContactAddress != "" {
			writeCVParam(b, "      ", TermContactAddress, w.opts.ContactAddress)
		}
		if w.opts.ContactEmail != "" {
			writeCVParam(b, "      ", TermContactEmail, w.opts.ContactEmail)
		}
		fmt.Fprintf(b, "    </contact>\n")
	}
	fmt.Fprintf(b, "  </fileDescription>\n")

	// sampleList (optional)
	if name := metaString(w.run, "global:sample_name", "var:sample_name"); name != "" {
		w.sample = xmlID("sample_1")
		fmt.Fprintf(b, "  <sampleList count=\"1\">\n")
		fmt.Fprintf(b, "    <sample id=%q name=%q>\n", w.sample, xmlAttr(truncate(name, 255)))
		writeCVParam(b, "    ", TermSampleName, name)
		fmt.Fprintf(b, "    </sample>\n")
		fmt.Fprintf(b, "  </sampleList>\n")
	}

	// softwareList (required)
	softwares := []struct{ id, name, version string }{
		{w.swID, "cdf2ms", w.opts.SoftwareVersion},
	}
	if name := metaString(w.run, "global:ms_acquisition_software", "global:instrument_app_version"); name != "" {
		softwares = append(softwares, struct{ id, name, version string }{
			xmlID("acquisition_software"), name, metaString(w.run, "global:instrument_sw_version")})
	}
	fmt.Fprintf(b, "  <softwareList count=\"%d\">\n", len(softwares))
	for _, s := range softwares {
		if s.version == "" {
			s.version = "unknown"
		}
		fmt.Fprintf(b, "    <software id=%q version=%q>\n", s.id, xmlAttr(truncate(s.version, 128)))
		writeCVParam(b, "    ", TermSoftware, s.name)
		fmt.Fprintf(b, "    </software>\n")
	}
	fmt.Fprintf(b, "  </softwareList>\n")

	// instrumentConfigurationList (required)
	fmt.Fprintf(b, "  <instrumentConfigurationList count=\"1\">\n")
	fmt.Fprintf(b, "    <instrumentConfiguration id=%q>\n", w.icID)
	writeCVParam(b, "    ", TermMassSpectrometer, "")
	if src := w.run.Instrument.Manufacturer; src != "" {
		writeCVParam(b, "    ", TermInstrumentVendor, src)
	}
	if w.run.Instrument.Model != "" {
		writeCVParam(b, "    ", TermInstrumentModel, w.run.Instrument.Model)
	}
	if w.run.Instrument.SerialNumber != "" {
		writeCVParam(b, "    ", TermInstrumentSerial, w.run.Instrument.SerialNumber)
	}
	// Instrument identity that has no reliable CV mapping is preserved as
	// userParams rather than forced into an approximate CV term. userParams must
	// precede componentList: the schema orders the param group first.
	for _, u := range instrumentUserParams(w.run) {
		writeUserParam(b, "    ", u.name, u.value, "")
	}
	// componentList requires a source, an analyzer AND a detector, so it is
	// emitted only when all three are actually known. A partial list is both
	// schema-invalid and a guess about hardware.
	ionTerm, haveIon := MapIonization(w.run.Instrument.IonizationMode)
	anTerm, haveAnalyzer := MapAnalyzer(w.run.Instrument.Model, w.run.Instrument.Name,
		w.run.Instrument.Comments, w.run.Instrument.ScanFunction)
	detTerm, haveDetector := MapDetector(w.run.Instrument.DetectorType)
	if haveIon && haveAnalyzer && haveDetector {
		fmt.Fprintf(b, "      <componentList count=\"3\">\n")
		for i, c := range []CVTerm{ionTerm, anTerm, detTerm} {
			tag := []string{"source", "analyzer", "detector"}[i]
			fmt.Fprintf(b, "        <%s order=\"%d\">\n", tag, i+1)
			writeCVParam(b, "        ", c, "")
			fmt.Fprintf(b, "        </%s>\n", tag)
		}
		fmt.Fprintf(b, "      </componentList>\n")
	}
	if len(softwares) > 1 {
		fmt.Fprintf(b, "      <softwareRef ref=%q/>\n", softwares[1].id)
	}
	fmt.Fprintf(b, "    </instrumentConfiguration>\n")
	fmt.Fprintf(b, "  </instrumentConfigurationList>\n")

	// dataProcessingList (required)
	fmt.Fprintf(b, "  <dataProcessingList count=\"1\">\n")
	fmt.Fprintf(b, "    <dataProcessing id=%q>\n", w.dpID)
	fmt.Fprintf(b, "      <processingMethod order=\"1\" softwareRef=%q>\n", w.swID)
	writeCVParam(b, "      ", TermConversionToMzML, "")
	for _, u := range provenanceParams(w.run, w.src, w.opts) {
		writeUserParam(b, "      ", u.name, u.value, "")
	}
	fmt.Fprintf(b, "      </processingMethod>\n")
	fmt.Fprintf(b, "    </dataProcessing>\n")
	fmt.Fprintf(b, "  </dataProcessingList>\n")

	// run + spectrumList
	fmt.Fprintf(b, "  <run id=%q defaultInstrumentConfigurationRef=%q", xmlAttr(w.opts.DocumentID), w.icID)
	if w.sample != "" {
		fmt.Fprintf(b, " sampleRef=%q", w.sample)
	}
	if !w.opts.Timestamp.IsZero() {
		fmt.Fprintf(b, " startTimeStamp=%q", w.opts.Timestamp.UTC().Format("2006-01-02T15:04:05.000000"))
	}
	b.WriteString(">\n")
	fmt.Fprintf(b, "    <spectrumList count=\"%d\" defaultDataProcessingRef=%q>\n", w.declared, w.dpID)
	return b.Flush()
}

// Write appends one spectrum.
//
// sp and its arrays must remain valid for the duration of the call only.
func (w *Writer) Write(sp *msdata.Spectrum) error {
	if w.closed {
		return errors.New("mzml: writer is closed")
	}
	if sp.Index != w.written {
		return fmt.Errorf("%w: mzml: spectrum index %d arrived out of order (expected %d)",
			msdata.ErrValidationFailed, sp.Index, w.written)
	}
	if len(sp.MZ) != len(sp.Intensity) {
		return fmt.Errorf("%w: mzml: spectrum %d has %d m/z values and %d intensity values",
			msdata.ErrValidationFailed, sp.Index, len(sp.MZ), len(sp.Intensity))
	}
	indent := "      "
	b := w.bw
	fmt.Fprintf(b, "    <spectrum index=\"%d\" id=%q defaultArrayLength=\"%d\">\n",
		sp.Index, spectrumID(sp), len(sp.MZ))
	if sp.ScanNumber != nil && *sp.ScanNumber < 0 {
		w.noticeOnce("scan-number", msdata.Diagnostic{
			Code: msdata.CodeMZMLScanNumberOmitted,
			Scan: sp.Index,
			Message: fmt.Sprintf("source scan number %d is a missing marker, not a scan number; "+
				"spectrum ids use the index component alone and andi:actual_scan_number is omitted", *sp.ScanNumber),
			Detail: "first spectrum where it was seen",
		})
	}

	level := 1
	if sp.MSLevel != nil {
		level = *sp.MSLevel
	}
	writeCVParam(b, indent, TermMSLevel, strconv.Itoa(level))
	writeCVParam(b, indent, spectrumLevelTerm(level), "")
	if pt, ok := polarityTerm(sp.Polarity); ok {
		writeCVParam(b, indent, pt, "")
	}
	if ct, ok := centroidTerm(sp.Centroid); ok {
		writeCVParam(b, indent, ct, "")
	}
	if sp.LowestMZ != nil {
		writeCVParam(b, indent, TermLowestMZ, formatNum(*sp.LowestMZ))
	}
	if sp.HighestMZ != nil {
		writeCVParam(b, indent, TermHighestMZ, formatNum(*sp.HighestMZ))
	}
	if sp.BasePeakMZ != nil {
		writeCVParam(b, indent, TermBasePeakMZ, formatNum(*sp.BasePeakMZ))
	}
	if sp.BasePeakInt != nil {
		writeCVParam(b, indent, TermBasePeakInt, formatNum(*sp.BasePeakInt))
	}
	if sp.TIC != nil {
		writeCVParam(b, indent, TermTotalIonCurrent, formatNum(*sp.TIC))
	}

	// scanList
	fmt.Fprintf(b, "%s<scanList count=\"1\">\n", indent)
	writeCVParam(b, indent+"  ", TermNoCombination, "")
	b.WriteString(indent + "  <scan")
	if w.icID != "" {
		fmt.Fprintf(b, " instrumentConfigurationRef=%q", w.icID)
	}
	b.WriteString(">\n")
	if sp.RetentionTimeSet {
		writeCVParam(b, indent+"    ", TermScanStartTime, formatNum(sp.RetentionTime))
	} else {
		// A spectrum with no retention time is represented honestly: the scan
		// element stays empty instead of carrying a fabricated 0.
		w.recordWarning(msdata.Diagnostic{
			Code:    msdata.CodeANDIMissingRetentionTime,
			Scan:    sp.Index,
			Message: fmt.Sprintf("spectrum %d has no retention time; scan start time omitted", sp.Index),
			Detail:  "source value was absent or a netCDF fill value",
		})
	}
	// Optional ANDI timing metadata is only emitted when it is a measurement. A
	// negative duration or delay is a missing marker, not a quantity, and
	// publishing it hands consumers a physically impossible number to plot.
	if sp.ScanDuration != nil {
		if *sp.ScanDuration >= 0 {
			writeUserParam(b, indent+"    ", "andi:scan_duration_seconds", formatNum(*sp.ScanDuration), "second")
		} else {
			w.noticeNonPhysical("scan-duration", "andi:scan_duration_seconds", formatNum(*sp.ScanDuration), sp.Index)
		}
	}
	if sp.InterScanTime != nil {
		if *sp.InterScanTime >= 0 {
			writeUserParam(b, indent+"    ", "andi:inter_scan_delay_seconds", formatNum(*sp.InterScanTime), "second")
		} else {
			w.noticeNonPhysical("inter-scan", "andi:inter_scan_delay_seconds", formatNum(*sp.InterScanTime), sp.Index)
		}
	}
	if sp.ScanNumber != nil && *sp.ScanNumber >= 0 {
		writeUserParam(b, indent+"    ", "andi:actual_scan_number", strconv.FormatInt(*sp.ScanNumber, 10), "")
	}
	fmt.Fprintf(b, "%s  </scan>\n", indent)
	fmt.Fprintf(b, "%s</scanList>\n", indent)

	// precursorList
	if level > 1 && sp.PrecursorMZ != nil {
		fmt.Fprintf(b, "%s<precursorList count=\"1\">\n", indent)
		fmt.Fprintf(b, "%s  <precursor>\n", indent)
		iw := indent + "    "
		fmt.Fprintf(b, "%s<isolationWindow>\n", iw)
		writeCVParam(b, iw+"  ", TermIsolationTarget, formatNum(*sp.PrecursorMZ))
		fmt.Fprintf(b, "%s</isolationWindow>\n", iw)
		sil := iw + "  "
		fmt.Fprintf(b, "%s<selectedIonList count=\"1\">\n", sil)
		fmt.Fprintf(b, "%s  <selectedIon>\n", sil)
		writeCVParam(b, sil+"  ", TermSelectedIonMZ, formatNum(*sp.PrecursorMZ))
		if sp.PrecursorInt != nil {
			writeCVParam(b, sil+"  ", TermPeakIntensity, formatNum(*sp.PrecursorInt))
		}
		if sp.PrecursorZ != nil && *sp.PrecursorZ != 0 {
			writeCVParam(b, sil+"  ", TermChargeState, strconv.Itoa(*sp.PrecursorZ))
		}
		fmt.Fprintf(b, "%s  </selectedIon>\n", sil)
		fmt.Fprintf(b, "%s</selectedIonList>\n", sil)
		if sp.CollisionEn != nil {
			act := sil + "  "
			fmt.Fprintf(b, "%s<activation>\n", act)
			writeCVParam(b, act+"  ", TermCID, "")
			writeCVParam(b, act+"  ", TermCollisionEnergy, formatNum(*sp.CollisionEn))
			fmt.Fprintf(b, "%s</activation>\n", act)
		}
		fmt.Fprintf(b, "%s  </precursor>\n", indent)
		fmt.Fprintf(b, "%s</precursorList>\n", indent)
	}

	// binaryDataArrayList
	mzRaw, mzTerm, err := w.encodeArray(sp.MZ)
	if err != nil {
		return err
	}
	intRaw, intTerm, err := w.encodeArray(sp.Intensity)
	if err != nil {
		return err
	}
	mzB64 := base64.StdEncoding.EncodeToString(mzRaw)
	intB64 := base64.StdEncoding.EncodeToString(intRaw)

	arr := indent + "  "
	fmt.Fprintf(b, "%s<binaryDataArrayList count=\"2\">\n", indent)
	fmt.Fprintf(b, "%s<binaryDataArray encodedLength=\"%d\">\n", arr, len(mzB64))
	writeCVParam(b, arr+"  ", mzTerm, "")
	writeCVParam(b, arr+"  ", w.compressionTerm(), "")
	writeCVParam(b, arr+"  ", TermMZArray, "")
	fmt.Fprintf(b, "%s  <binary>%s</binary>\n", arr, mzB64)
	fmt.Fprintf(b, "%s</binaryDataArray>\n", arr)
	fmt.Fprintf(b, "%s<binaryDataArray encodedLength=\"%d\">\n", arr, len(intB64))
	writeCVParam(b, arr+"  ", intTerm, "")
	writeCVParam(b, arr+"  ", w.compressionTerm(), "")
	writeCVParam(b, arr+"  ", TermIntensityArr, "")
	fmt.Fprintf(b, "%s  <binary>%s</binary>\n", arr, intB64)
	fmt.Fprintf(b, "%s</binaryDataArray>\n", arr)
	fmt.Fprintf(b, "%s</binaryDataArrayList>\n", indent)

	fmt.Fprintf(b, "    </spectrum>\n")

	w.written++
	w.stats.Spectra++
	w.stats.Points += int64(len(sp.MZ))
	if err := b.Flush(); err != nil {
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "writing spectrum %d", sp.Index)
	}
	return nil
}

func (w *Writer) compressionTerm() CVTerm {
	if w.opts.Compress {
		return TermZlib
	}
	return TermNoCompression
}

// encodeArray converts values to the on-disk little-endian byte layout the mzML
// specification mandates, choosing 32- or 64-bit elements as configured.
func (w *Writer) encodeArray(vals []float64) ([]byte, CVTerm, error) {
	use32 := false
	switch w.opts.Precision {
	case PrecisionF32:
		use32 = true
		if bad := countNonRepresentable(vals); bad > 0 {
			w.recordWarning(msdata.Diagnostic{
				Code:    msdata.CodeNumericPrecisionLoss,
				Message: fmt.Sprintf("%d values are not exactly representable in 32-bit float", bad),
				Detail:  "--precision=f32 was requested; use auto or f64 for a lossless encoding",
			})
		}
	case PrecisionF64:
		use32 = false
	default:
		use32 = allFloat32Exact(vals)
	}
	if use32 {
		buf := make([]byte, 4*len(vals))
		for i, v := range vals {
			binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(float32(v)))
		}
		w.stats.F32Arrays++
		return w.maybeCompress(buf), TermFloat32, nil
	}
	buf := make([]byte, 8*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint64(buf[8*i:], math.Float64bits(v))
	}
	w.stats.F64Arrays++
	return w.maybeCompress(buf), TermFloat64, nil
}

func (w *Writer) maybeCompress(buf []byte) []byte {
	if !w.opts.Compress {
		return buf
	}
	var out strings.Builder
	zw, err := zlib.NewWriterLevel(&out, w.opts.CompressionLevel)
	if err != nil {
		zw = zlib.NewWriter(&out)
	}
	if _, err := zw.Write(buf); err != nil {
		return buf
	}
	if err := zw.Close(); err != nil {
		return buf
	}
	w.stats.CompressedArr++
	return []byte(out.String())
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

// Close finishes the document.
//
// If the number of spectra actually written differs from the count declared in
// <spectrumList count>, Close returns ErrCountMismatch and leaves the document
// at path.partial instead of a valid-looking .mzML.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	b := w.bw
	fmt.Fprintf(b, "    </spectrumList>\n")
	fmt.Fprintf(b, "  </run>\n")
	fmt.Fprintf(b, "</mzML>\n")
	if err := b.Flush(); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "flushing %s", w.path)
	}
	// The report must state what landed on disk; the temp file has the final size
	// at this point because everything is flushed.
	if st, err := w.fh.Stat(); err == nil {
		w.stats.Bytes = st.Size()
	}
	w.stats.OutputSHA256 = w.hasher.HexDigest()
	if err := w.fh.Sync(); err != nil {
		w.fh.Close()
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "syncing %s", w.path)
	}
	if err := w.fh.Close(); err != nil {
		return msdata.WrapError(msdata.CodeOutputWriteFailed, err, "closing %s", w.path)
	}
	if w.written != w.declared {
		rename := w.path + ".partial"
		_ = os.Rename(w.tmp, rename)
		return msdata.WrapError(msdata.CodeCountMismatch,
			fmt.Errorf("declared %d spectra, wrote %d", w.declared, w.written),
			"%s: spectrum count mismatch; document kept at %s for inspection",
			filepath.Base(w.path), filepath.Base(rename))
	}
	if err := msdata.Promote(w.tmp, w.path); err != nil {
		return msdata.WrapError(msdata.CodeOutputRenameFailed, err, "moving %s to %s", w.tmp, w.path)
	}
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

// recordWarning keeps writer diagnostics bounded: a pathological file must not
// turn per-spectrum warnings into unbounded memory growth. The first 64 are
// kept verbatim; the count of the rest is reported once.
// noticeNonPhysical reports, once per document, an optional ANDI parameter
// dropped because its value cannot be a measurement.
func (w *Writer) noticeNonPhysical(key, param, value string, scan int) {
	w.noticeOnce(key, msdata.Diagnostic{
		Code: msdata.CodeMZMLNonPhysicalOmitted,
		Scan: scan,
		Message: fmt.Sprintf("%s is %s, which cannot be a measurement; the parameter is omitted "+
			"instead of published as a value", param, value),
		Detail: "read as a missing-data marker",
	})
}

// noticeOnce records a diagnostic the first time its condition is seen. A
// sentinel that repeats on every spectrum of a run is one fact about the file,
// and reporting it once per spectrum would crowd every other diagnostic out of
// the bounded report.
func (w *Writer) noticeOnce(key string, d msdata.Diagnostic) {
	if w.noticed == nil {
		w.noticed = map[string]bool{}
	}
	if w.noticed[key] {
		return
	}
	w.noticed[key] = true
	w.recordWarning(d)
}

func (w *Writer) recordWarning(d msdata.Diagnostic) {
	if len(w.stats.Warnings) < 64 {
		w.stats.Warnings = append(w.stats.Warnings, d)
		return
	}
	if w.stats.DroppedWarnings == 0 {
		w.stats.Warnings = append(w.stats.Warnings, msdata.Diagnostic{
			Code:    msdata.CodeDiagnosticsTruncated,
			Message: "further writer warnings suppressed",
		})
	}
	w.stats.DroppedWarnings++
}

// ---- helpers ----

type kv struct{ name, value string }

func provenanceParams(run *msdata.Run, src msdata.SourceFile, opts Options) []kv {
	out := []kv{
		{"cdf2ms:version", opts.SoftwareVersion},
		{"cdf2ms:converted_at_utc", opts.Timestamp.UTC().Format(time.RFC3339Nano)},
		{"cdf2ms:source_file", src.Name},
		{"cdf2ms:source_encoding", src.Encoding},
		{"cdf2ms:source_size_bytes", strconv.FormatInt(src.Size, 10)},
	}
	if src.SHA256 != "" {
		out = append(out, kv{"cdf2ms:source_sha256", src.SHA256})
	}
	if run.RetentionTimeUnitOrigin != "" {
		out = append(out, kv{"cdf2ms:retention_time_unit", run.RetentionTimeUnit},
			kv{"cdf2ms:retention_time_unit_origin", run.RetentionTimeUnitOrigin})
	}
	for _, key := range []string{"derived:ms_level", "derived:polarity", "derived:centroid", "derived:scan_plan"} {
		if v := metaString(run, key); v != "" {
			out = append(out, kv{strings.ReplaceAll(key, ":", ":"), v})
		}
	}
	if run.SchemaFingerprint != "" {
		out = append(out, kv{"cdf2ms:schema_fingerprint", run.SchemaFingerprint},
			kv{"cdf2ms:vendor_fingerprint", run.VendorFingerprint})
	}
	// Statistics policy: recording it explicitly is cheaper than silently
	// implying the numbers came from the instrument.
	out = append(out, kv{"cdf2ms:spectrum_statistics_policy",
		"lowest/highest m/z, base peak and total ion current are emitted only when declared by the source"})
	return out
}

func instrumentUserParams(run *msdata.Run) []kv {
	out := []kv{}
	add := func(name, v string) {
		if v != "" {
			out = append(out, kv{name, v})
		}
	}
	add("instrument_name", run.Instrument.Name)
	add("instrument_id", run.Instrument.ID)
	add("instrument_serial_number", run.Instrument.SerialNumber)
	add("instrument_firmware_version", run.Instrument.FirmwareVer)
	add("instrument_os_version", run.Instrument.OSVersion)
	add("instrument_app_version", run.Instrument.AppVersion)
	add("instrument_comments", run.Instrument.Comments)
	add("acquisition_software", run.Instrument.SoftwareVer)
	add("ionization_mode_text", run.Instrument.IonizationMode)
	add("detector_type_text", run.Instrument.DetectorType)
	add("separation_type_text", run.Instrument.SeparationType)
	add("ms_inlet_text", run.Instrument.MSInlet)
	add("scan_function", run.Instrument.ScanFunction)
	add("scan_direction", run.Instrument.ScanDirection)
	add("scan_law", run.Instrument.ScanLaw)
	add("resolution_type", run.Instrument.ResolutionType)
	add("experiment_title", run.Instrument.ExperimentTitle)
	add("experiment_type", run.Instrument.ExperimentType)
	add("operator_name", run.Instrument.Operator)
	add("dataset_origin", run.Instrument.DataSetOrigin)
	// ANDI vendor extensions that have no CV mapping are preserved verbatim.
	keys := make([]string, 0, len(run.Metadata))
	for k := range run.Metadata {
		if strings.HasPrefix(k, "var:") || strings.HasPrefix(k, "global:") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := run.Metadata[k].Str
		if v == "" || len(v) > 512 {
			continue
		}
		add(strings.ReplaceAll(k, " ", "_"), v)
	}
	return out
}

func centroidFromRun(run *msdata.Run) msdata.CentroidState {
	if v := metaString(run, "derived:centroid"); v != "" {
		if strings.EqualFold(v, "centroid") {
			return msdata.CentroidCentroided
		}
		if strings.EqualFold(v, "profile") {
			return msdata.CentroidProfile
		}
	}
	return msdata.CentroidUnknown
}

func hasMSn(run *msdata.Run) bool {
	v := metaString(run, "derived:ms_level")
	return strings.HasPrefix(v, "derived:precursor") || v == "2"
}

func metaString(run *msdata.Run, keys ...string) string {
	for _, k := range keys {
		if v, ok := run.Metadata[k]; ok && v.Str != "" {
			return v.Str
		}
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func sourceSHA256(src msdata.SourceFile) string {
	return strings.TrimPrefix(src.SHA256, "sha256:")
}

func fileLocation(src msdata.SourceFile) string {
	if src.Path == "" {
		return "file://" + src.Name
	}
	if !strings.HasPrefix(src.Path, "/") {
		abs, err := filepath.Abs(src.Path)
		if err == nil {
			return "file://" + abs
		}
	}
	return "file://" + src.Path
}

func spectrumID(sp *msdata.Spectrum) string {
	// The schema requires the native ID shape "key=value [key=value...]".
	// index= is always present so IDs stay unique and ordered even when the
	// source numbers are absent, duplicated, or zero-based (some vendors number
	// scans from 0); the vendor scan number rides along when it was reported.
	//
	// A negative number is not a scan number but a missing marker (Agilent writes
	// -9999 for "undefined"), and consumers parse scan= out of native IDs to join
	// spectra across files. Publishing scan=-9999 would give every spectrum in the
	// run the same bogus join key, so the component is dropped instead.
	if sp.ScanNumber != nil && *sp.ScanNumber >= 0 {
		return "index=" + strconv.Itoa(sp.Index) + " scan=" + strconv.FormatInt(*sp.ScanNumber, 10)
	}
	return "index=" + strconv.Itoa(sp.Index)
}

func writeCVParam(b *bufio.Writer, indent string, t CVTerm, value string) {
	fmt.Fprintf(b, "%s<cvParam cvRef=%q accession=%q name=%q", indent, cvRefOf(t.Accession), t.Accession, xmlAttr(t.Name))
	if value != "" {
		fmt.Fprintf(b, " value=%q", xmlAttr(value))
	}
	if t.UnitTerm != nil {
		fmt.Fprintf(b, " unitCvRef=%q unitAccession=%q unitName=%q",
			cvRefOf(t.UnitTerm.Accession), t.UnitTerm.Accession, xmlAttr(t.UnitTerm.Name))
	}
	b.WriteString("/>\n")
}

func writeUserParam(b *bufio.Writer, indent, name, value, unitName string) {
	fmt.Fprintf(b, "%s<userParam name=%q", indent, xmlAttr(name))
	if value != "" {
		fmt.Fprintf(b, " value=%q", xmlAttr(value))
	}
	if unitName != "" {
		fmt.Fprintf(b, " unitName=%q", xmlAttr(unitName))
	}
	b.WriteString("/>\n")
}

func cvRefOf(accession string) string {
	if strings.HasPrefix(accession, "UO:") {
		return UORef
	}
	return MSRef
}

// formatNum renders a float with the shortest representation that round-trips,
// so a value written here parses back bit-identically.
func formatNum(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		// Neither is representable in XML schema doubles; callers must filter
		// non-finite numbers before reaching the writer.
		return "0"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// xmlAttr escapes a string for use inside a double-quoted XML attribute,
// dropping characters XML 1.0 does not allow at all.
func xmlAttr(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range sanitizeUTF8(s) {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\t':
			b.WriteString("&#9;")
		case '\n':
			b.WriteString("&#10;")
		case '\r':
			b.WriteString("&#13;")
		default:
			if r < 0x20 || (r >= 0x7f && r <= 0x84) || r == 0x86 || r == 0x9f ||
				(r >= 0xd800 && r <= 0xdfff) || r == 0xfffe || r == 0xffff {
				b.WriteString("&#xfffd;")
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// xmlID turns arbitrary text into an XML NCName.
func xmlID(s string) string {
	if s == "" {
		return "x"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
		if !ok {
			if c >= '0' && c <= '9' {
				if b.Len() == 0 {
					b.WriteByte('x')
				}
				b.WriteByte(c)
				continue
			}
			b.WriteByte('_')
			continue
		}
		b.WriteByte(c)
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// sanitizeUTF8 replaces invalid UTF-8 byte sequences with U+FFFD so the XML
// document stays well-formed.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune('\uFFFD')
			i++
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}
