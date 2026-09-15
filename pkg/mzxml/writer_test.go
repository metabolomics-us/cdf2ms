package mzxml

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

func runMeta() (*msdata.Run, msdata.SourceFile) {
	run := &msdata.Run{
		ID:                      "run_1",
		ScanCount:               3,
		RetentionTimeUnit:       "second",
		RetentionTimeUnitOrigin: "var:time_values.units",
		Metadata: msdata.Metadata{
			"derived:scan_plan": {Kind: msdata.KindString, Str: "scans=3 points=6 source=var:scan_index+var:point_count"},
			"derived:ms_level":  {Kind: msdata.KindString, Str: "1"},
		},
		Instrument: msdata.Instrument{
			Manufacturer:   "Agilent",
			Model:          "6120 Quadrupole",
			IonizationMode: "Electron Impact",
			DetectorType:   "Electron Multiplier",
		},
		Source: msdata.SourceFile{
			Path: "x.CDF", Name: "x.CDF", Size: 1234, Encoding: "CDF-1 (classic)",
			SHA1: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64),
		},
	}
	return run, run.Source
}

func spectrum(i int, mz, inten []float64) *msdata.Spectrum {
	rt := float64(i)*0.5 + 0.25
	sp := &msdata.Spectrum{
		Index: i, RetentionTime: rt, RetentionTimeSet: true,
		MZ: mz, Intensity: inten,
	}
	lvl := 1
	sp.MSLevel = &lvl
	return sp
}

func ptrF(v float64) *float64 { return &v }
func ptrI(v int) *int         { return &v }

// doc is a structural parse of a generated document.
type doc struct {
	Root     xml.Name `xml:"mzXML"`
	Scans    []scanEl `xml:"msRun>scan"`
	Index    []index  `xml:"index"`
	IndexOff string   `xml:"indexOffset"`
	SHA1     string   `xml:"sha1"`
}

type index struct {
	Name    string        `xml:"name,attr"`
	Offsets []indexOffset `xml:"offset"`
}

type indexOffset struct {
	ID  int64 `xml:"id,attr"`
	Pos int64 `xml:",chardata"`
}

type scanEl struct {
	Num           int64         `xml:"num,attr"`
	MSLevel       int           `xml:"msLevel,attr"`
	PeaksCount    int           `xml:"peaksCount,attr"`
	Centroided    string        `xml:"centroided,attr"`
	Polarity      string        `xml:"polarity,attr"`
	RetentionTime string        `xml:"retentionTime,attr"`
	LowMz         string        `xml:"lowMz,attr"`
	HighMz        string        `xml:"highMz,attr"`
	TIC           string        `xml:"totIonCurrent,attr"`
	ScanType      string        `xml:"scanType,attr"`
	InstrumentID  string        `xml:"msInstrumentID,attr"`
	Peaks         peaksEl       `xml:"peaks"`
	Precursors    []precursorEl `xml:"precursorMz"`
}

type peaksEl struct {
	Precision     string `xml:"precision,attr"`
	ByteOrder     string `xml:"byteOrder,attr"`
	ContentType   string `xml:"contentType,attr"`
	Compression   string `xml:"compressionType,attr"`
	CompressedLen int    `xml:"compressedLen,attr"`
	Nil           string `xml:"http://www.w3.org/2001/XMLSchema-instance nil,attr"`
	Data          string `xml:",chardata"`
}

type precursorEl struct {
	Intensity string `xml:"precursorIntensity,attr"`
	Charge    string `xml:"precursorCharge,attr"`
	Value     string `xml:",chardata"`
}

func writeDoc(t *testing.T, opts Options, spectra ...*msdata.Spectrum) (string, *Writer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mzXML")
	run, src := runMeta()
	run.ScanCount = len(spectra)
	w, err := Create(path, run, src, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, sp := range spectra {
		if err := w.Write(sp); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, w
}

func parseDoc(t *testing.T, path string) doc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := xml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("re-parsing generated document: %v", err)
	}
	return d
}

// decodePeaks decodes a peaks element the way an independent reader must:
// big-endian interleaved m/z, intensity.
func decodePeaks(t *testing.T, p peaksEl) (mz, inten []float64) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Data))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if p.Compression == "zlib" {
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("zlib reader: %v", err)
		}
		raw, err = io.ReadAll(zr)
		if err != nil {
			t.Fatalf("zlib decompress: %v", err)
		}
	}
	if p.ByteOrder != "network" {
		t.Fatalf("byteOrder = %q, want network (mzXML is big-endian)", p.ByteOrder)
	}
	if p.ContentType != "m/z-int" {
		t.Fatalf("contentType = %q, want m/z-int", p.ContentType)
	}
	switch p.Precision {
	case "32":
		if len(raw)%8 != 0 {
			t.Fatalf("32-bit interleaved array length %d not a multiple of 8", len(raw))
		}
		for i := 0; i+8 <= len(raw); i += 8 {
			mz = append(mz, float64(math.Float32frombits(binary.BigEndian.Uint32(raw[i:]))))
			inten = append(inten, float64(math.Float32frombits(binary.BigEndian.Uint32(raw[i+4:]))))
		}
	case "64":
		if len(raw)%16 != 0 {
			t.Fatalf("64-bit interleaved array length %d not a multiple of 16", len(raw))
		}
		for i := 0; i+16 <= len(raw); i += 16 {
			mz = append(mz, math.Float64frombits(binary.BigEndian.Uint64(raw[i:])))
			inten = append(inten, math.Float64frombits(binary.BigEndian.Uint64(raw[i+8:])))
		}
	default:
		t.Fatalf("precision = %q", p.Precision)
	}
	return mz, inten
}

func TestWriteReadBackNumericFidelity(t *testing.T) {
	mz := []float64{50.5, 100.25, 200.125}
	inten := []float64{1, 2.5, 1e7}
	path, _ := writeDoc(t, Options{}, spectrum(0, mz, inten), spectrum(1, mz, inten), spectrum(2, mz, inten))
	d := parseDoc(t, path)
	if len(d.Scans) != 3 {
		t.Fatalf("parsed %d scans, want 3", len(d.Scans))
	}
	for i, sc := range d.Scans {
		if sc.Num != int64(i+1) {
			t.Errorf("scan %d num = %d", i, sc.Num)
		}
		if sc.PeaksCount != 3 {
			t.Errorf("scan %d peaksCount = %d", i, sc.PeaksCount)
		}
		gotMZ, gotInt := decodePeaks(t, sc.Peaks)
		for j := range mz {
			if gotMZ[j] != mz[j] || gotInt[j] != inten[j] {
				t.Fatalf("scan %d point %d: got (%v,%v) want (%v,%v)", i, j, gotMZ[j], gotInt[j], mz[j], inten[j])
			}
		}
		wantRT := fmt.Sprintf("PT%gS", float64(i)*0.5+0.25)
		if sc.RetentionTime != wantRT {
			t.Errorf("scan %d retentionTime = %q want %q", i, sc.RetentionTime, wantRT)
		}
	}
}

func TestPrecisionAutoUsesF64WhenLossy(t *testing.T) {
	// This value is not representable as float32, so auto must choose 64-bit.
	lossy := 1.0000000000000002
	path, w := writeDoc(t, Options{}, spectrum(0, []float64{lossy}, []float64{1}))
	d := parseDoc(t, path)
	if d.Scans[0].Peaks.Precision != "64" {
		t.Fatalf("precision = %q, want 64", d.Scans[0].Peaks.Precision)
	}
	mz, _ := decodePeaks(t, d.Scans[0].Peaks)
	if mz[0] != lossy {
		t.Fatalf("value degraded: %v", mz[0])
	}
	if w.stats.F64Arrays != 1 {
		t.Fatalf("F64Arrays = %d", w.stats.F64Arrays)
	}

	path2, _ := writeDoc(t, Options{}, spectrum(0, []float64{0.5}, []float64{2}))
	d2 := parseDoc(t, path2)
	if d2.Scans[0].Peaks.Precision != "32" {
		t.Fatalf("lossless data should use precision 32, got %q", d2.Scans[0].Peaks.Precision)
	}
}

func TestCompressionSetsCompressedLen(t *testing.T) {
	n := 200
	mz := make([]float64, n)
	inten := make([]float64, n)
	for i := range mz {
		mz[i] = float64(i)
		inten[i] = float64(i * 3)
	}
	path, _ := writeDoc(t, Options{Compress: true}, spectrum(0, mz, inten))
	d := parseDoc(t, path)
	p := d.Scans[0].Peaks
	if p.Compression != "zlib" {
		t.Fatalf("compressionType = %q", p.Compression)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.Data))
	if err != nil {
		t.Fatal(err)
	}
	if p.CompressedLen != len(raw) {
		t.Fatalf("compressedLen = %d, actual compressed bytes = %d", p.CompressedLen, len(raw))
	}
	gotMZ, gotInt := decodePeaks(t, p)
	if len(gotMZ) != n {
		t.Fatalf("decoded %d points", len(gotMZ))
	}
	for i := range mz {
		if gotMZ[i] != mz[i] || gotInt[i] != inten[i] {
			t.Fatalf("point %d mismatch after decompression", i)
		}
	}
}

// TestIndexOffsetsPointAtScans proves the byte offsets are real file offsets:
// seeking to each one must land on the opening tag of the matching scan.
func TestIndexOffsetsPointAtScans(t *testing.T) {
	spectra := []*msdata.Spectrum{spectrum(0, []float64{1}, []float64{2}), spectrum(1, []float64{3}, []float64{4})}
	path, _ := writeDoc(t, Options{}, spectra...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := parseDoc(t, path)
	if len(d.Index) != 1 || d.Index[0].Name != "scan" {
		t.Fatalf("index = %+v", d.Index)
	}
	if len(d.Index[0].Offsets) != 2 {
		t.Fatalf("index has %d offsets", len(d.Index[0].Offsets))
	}
	for k, off := range d.Index[0].Offsets {
		if off.ID != int64(k+1) {
			t.Errorf("offset id = %d want %d", off.ID, k+1)
		}
		if off.Pos < 0 || int(off.Pos) >= len(raw) {
			t.Fatalf("offset %d out of range (%d bytes)", off.Pos, len(raw))
		}
		window := string(raw[off.Pos:min(off.Pos+40, int64(len(raw)))])
		if !strings.HasPrefix(window, "<scan ") {
			t.Fatalf("offset %d does not point at a scan element: %q", off.Pos, window)
		}
		wantNum := `num="` + strconv.FormatInt(off.ID, 10) + `"`
		if !strings.Contains(window, wantNum) {
			t.Fatalf("offset %d points at a scan without %s: %q", off.Pos, wantNum, window)
		}
	}
	idxOff, err := strconv.ParseInt(d.IndexOff, 10, 64)
	if err != nil {
		t.Fatalf("indexOffset %q: %v", d.IndexOff, err)
	}
	if !bytes.HasPrefix(raw[idxOff:], []byte("<index name=\"scan\">")) {
		t.Fatalf("indexOffset %d does not point at <index>: %q", idxOff, string(raw[idxOff:idxOff+40]))
	}
}

// TestSHA1CoversPrefix verifies the checksum really covers the document up to and
// including the <sha1> opening tag, the rule the schema documents.
func TestSHA1CoversPrefix(t *testing.T) {
	path, _ := writeDoc(t, Options{}, spectrum(0, []float64{1.5}, []float64{2.5}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(raw, []byte("<sha1>"))
	if i < 0 {
		t.Fatal("no <sha1> element")
	}
	prefix := raw[:i+len("<sha1>")]
	h := sha1.Sum(prefix)
	want := fmt.Sprintf("%x", h)
	d := parseDoc(t, path)
	if d.SHA1 != want {
		t.Fatalf("sha1 = %q, recomputed %q", d.SHA1, want)
	}
	if len(d.SHA1) != 40 {
		t.Fatalf("sha1 must be exactly 40 characters, got %d", len(d.SHA1))
	}
}

func TestCountMismatchKeepsPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.mzXML")
	run, src := runMeta()
	run.ScanCount = 5
	w, err := Create(path, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if err == nil {
		t.Fatal("expected count mismatch error")
	}
	if msdata.CodeOf(err) != msdata.CodeCountMismatch {
		t.Fatalf("code = %s want %s (%v)", msdata.CodeOf(err), msdata.CodeCountMismatch, err)
	}
	if !errors.Is(err, msdata.ErrCountMismatch) {
		t.Fatalf("error must wrap ErrCountMismatch: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("a document with a lying scanCount must not be published at the final path")
	}
	if _, statErr := os.Stat(path + ".partial"); statErr != nil {
		t.Fatalf("partial document was not kept: %v", statErr)
	}
}

func TestEmptyRunRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.mzXML")
	run, src := runMeta()
	run.ScanCount = 0
	w, err := Create(path, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = w.Close()
	if err == nil {
		t.Fatal("an empty mzXML document is schema-invalid and must be refused")
	}
	if msdata.CodeOf(err) != msdata.CodeMZXMLValidationFailed {
		t.Fatalf("code = %s", msdata.CodeOf(err))
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("no document should exist")
	}
	if _, statErr := os.Stat(path + ".writing"); statErr == nil {
		t.Fatal("temp file left behind")
	}
}

func TestEmptySpectrumWritesNilPeaks(t *testing.T) {
	path, w := writeDoc(t, Options{}, spectrum(0, nil, nil))
	d := parseDoc(t, path)
	p := d.Scans[0].Peaks
	if p.Nil != "true" {
		t.Fatalf("expected xsi:nil=\"true\", got %q", p.Nil)
	}
	if d.Scans[0].PeaksCount != 0 {
		t.Fatalf("peaksCount = %d", d.Scans[0].PeaksCount)
	}
	if p.Compression != "none" || p.CompressedLen != 0 {
		t.Fatalf("nil peaks must declare compressionType=none compressedLen=0, got %q/%d", p.Compression, p.CompressedLen)
	}
	if w.stats.Empty != 1 {
		t.Fatalf("Empty = %d", w.stats.Empty)
	}
}

func TestOptionalFactsOmittedWhenUnknown(t *testing.T) {
	sp := spectrum(0, []float64{1}, []float64{2})
	sp.RetentionTimeSet = false
	sp.MSLevel = nil
	path, _ := writeDoc(t, Options{}, sp)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc := string(raw)
	// The document is re-parsed rather than grepped blindly.
	d := parseDoc(t, path)
	if strings.Contains(d.Scans[0].RetentionTime, "P") {
		t.Fatalf("retentionTime written for an unset retention time: %q", d.Scans[0].RetentionTime)
	}
	if d.Scans[0].Centroided != "" {
		t.Fatalf("centroided written for an unknown centroid state: %q", d.Scans[0].Centroided)
	}
	if d.Scans[0].Polarity != "" {
		t.Fatalf("polarity written for an unknown polarity: %q", d.Scans[0].Polarity)
	}
	if d.Scans[0].MSLevel != 1 {
		t.Fatalf("msLevel must still satisfy xs:positiveInteger, got %d", d.Scans[0].MSLevel)
	}
	// msRun endTime is optional and must be absent, not guessed.
	if strings.Contains(sc, "endTime=") {
		t.Fatalf("endTime invented: %s", sc[:400])
	}
}

func TestKnownOptionalFactsAreWritten(t *testing.T) {
	sp := spectrum(0, []float64{1}, []float64{2})
	sp.Centroid = msdata.CentroidCentroided
	sp.Polarity = msdata.PolarityNegative
	sp.LowestMZ = ptrF(50)
	sp.HighestMZ = ptrF(950)
	sp.TIC = ptrF(1234.5)
	path, _ := writeDoc(t, Options{}, sp)
	d := parseDoc(t, path)
	got := d.Scans[0]
	if got.Centroided != "1" || got.Polarity != "-" {
		t.Fatalf("centroided=%q polarity=%q", got.Centroided, got.Polarity)
	}
	if got.LowMz != "50" || got.HighMz != "950" || got.TIC != "1234.5" {
		t.Fatalf("lowMz=%q highMz=%q tic=%q", got.LowMz, got.HighMz, got.TIC)
	}
}

func TestNonFiniteOptionalValuesAreDropped(t *testing.T) {
	sp := spectrum(0, []float64{1}, []float64{2})
	sp.TIC = ptrF(math.NaN())
	sp.LowestMZ = ptrF(math.Inf(1))
	path, _ := writeDoc(t, Options{}, sp)
	d := parseDoc(t, path)
	if d.Scans[0].TIC != "" || d.Scans[0].LowMz != "" {
		t.Fatalf("non-finite values were written: tic=%q lowMz=%q", d.Scans[0].TIC, d.Scans[0].LowMz)
	}
}

func TestPrecursorOnlyWhenKnown(t *testing.T) {
	ms2 := spectrum(0, []float64{1}, []float64{2})
	lvl := 2
	ms2.MSLevel = &lvl
	ms2.PrecursorMZ = ptrF(411.2)
	ms2.PrecursorZ = ptrI(2)
	path, _ := writeDoc(t, Options{}, ms2)
	d := parseDoc(t, path)
	if len(d.Scans[0].Precursors) != 1 {
		t.Fatalf("precursors = %+v", d.Scans[0].Precursors)
	}
	p := d.Scans[0].Precursors[0]
	if p.Value != "411.2" {
		t.Fatalf("precursor m/z text = %q", p.Value)
	}
	if p.Charge != "2" {
		t.Fatalf("charge = %q", p.Charge)
	}
	// precursorIntensity is schema-required; 0 marks "not reported".
	if p.Intensity != "0" {
		t.Fatalf("precursorIntensity = %q, want the documented 0 placeholder", p.Intensity)
	}

	path2, _ := writeDoc(t, Options{}, spectrum(0, []float64{1}, []float64{2}))
	d2 := parseDoc(t, path2)
	if len(d2.Scans[0].Precursors) != 0 {
		t.Fatalf("MS1 gained a precursor: %+v", d2.Scans[0].Precursors)
	}
}

func TestInstrumentComponentsAndPlaceholders(t *testing.T) {
	path, _ := writeDoc(t, Options{}, spectrum(0, []float64{1}, []float64{2}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`<msManufacturer category="msManufacturer" value="Agilent"/>`,
		`<msModel category="msModel" value="6120 Quadrupole"/>`,
		`<msIonisation category="msIonisation" value="Electron Impact"/>`,
		`<msMassAnalyzer category="msMassAnalyzer" value="quadrupole"/>`,
		`<msDetector category="msDetector" value="Electron Multiplier"/>`,
		`<software type="acquisition"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s\n%s", want, s[:1200])
		}
	}

	// A source that reports nothing about the hardware omits msInstrument
	// entirely (mzXML has no partial-instrument form) and says so.
	path2 := filepath.Join(t.TempDir(), "bare.mzXML")
	run, src := runMeta()
	run.ScanCount = 1
	run.Instrument = msdata.Instrument{}
	w2, err := Create(path2, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(path2)
	if strings.Contains(string(raw2), "<msInstrument") {
		t.Fatal("msInstrument invented for an instrument with no reported identity")
	}
	if !hasCode(w2.stats.Warnings, msdata.CodeMZXMLInstrumentUnreported) {
		t.Fatalf("expected an unreported-instrument warning, got %+v", w2.stats.Warnings)
	}
}

func TestUnknownAnalyzerIsNotGuessedFromScanFunction(t *testing.T) {
	// The default fixture model names a quadrupole. Rebuild with a model that
	// does not name an analyzer: ANDI's test_resolution_type ("Constant
	// Resolution") must not be laundered into an analyzer assertion.
	dir := t.TempDir()
	out := filepath.Join(dir, "x.mzXML")
	run, src := runMeta()
	run.ScanCount = 1
	run.Instrument.Model = "GC-MS 7890"
	run.Instrument.ResolutionType = "Constant Resolution"
	run.Instrument.ScanFunction = "Mass Scan"
	run.Instrument.Manufacturer = ""
	wt, err := Create(out, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := wt.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(raw), `<msMassAnalyzer category="msMassAnalyzer" value="Unknown"/>`) {
		t.Fatalf("analyzer was invented:\n%s", string(raw)[:1500])
	}
	if !hasCode(wt.stats.Warnings, msdata.CodeMZXMLInstrumentPlaceholder) {
		t.Fatalf("placeholder use must be reported: %+v", wt.stats.Warnings)
	}
}

func TestSourceSHA1IsComputed(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.CDF")
	if err := os.WriteFile(srcPath, []byte("andi bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mzXML")
	run, src := runMeta()
	run.ScanCount = 1
	src.Path = srcPath
	src.SHA1 = "" // force the writer to compute the required digest
	w, err := Create(out, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := sha1.Sum([]byte("andi bytes"))
	got := fmt.Sprintf("%x", want)
	if w.stats.SourceSHA1 != got {
		t.Fatalf("source sha1 = %q want %q", w.stats.SourceSHA1, got)
	}
	raw, _ := os.ReadFile(out)
	if !strings.Contains(string(raw), `fileSha1="`+got+`"`) {
		t.Fatalf("parentFile sha1 missing:\n%s", string(raw)[:800])
	}
}

func TestFileNameIsAValidURI(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "weird name [1].CDF")
	if err := os.WriteFile(srcPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.mzXML")
	run, src := runMeta()
	run.ScanCount = 1
	src.Path = srcPath
	w, err := Create(out, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	var d doc
	if err := xml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(raw), `fileName="`)
	if i < 0 {
		t.Fatal("no fileName")
	}
	val := string(raw[i+len(`fileName="`):])
	val = val[:strings.Index(val, `"`)]
	if strings.ContainsAny(val, " []") {
		t.Fatalf("fileName is not URI-escaped: %q", val)
	}
	if !strings.HasPrefix(val, "file:///") {
		t.Fatalf("fileName should be an absolute file URI, got %q", val)
	}
}

func TestHostileMetadataIsEscaped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evil.mzXML")
	run, src := runMeta()
	run.ScanCount = 1
	run.Instrument.Model = `"><scan num="999" msLevel="1" peaksCount="0"/><x"`
	run.Instrument.Comments = "line1\nline2\t<tag/>"
	run.RetentionTimeUnitOrigin = "&amp;"
	w, err := Create(path, run, src, Options{ExtraComments: []string{"a & b < c > d"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	d := parseDoc(t, path)
	if len(d.Scans) != 1 {
		t.Fatalf("metadata injection produced extra scans: %d", len(d.Scans))
	}
	if len(d.Index) != 1 || len(d.Index[0].Offsets) != 1 {
		t.Fatalf("index corrupted: %+v", d.Index)
	}
}

func TestInvalidUTF8IsSanitized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latin1.mzXML")
	run, src := runMeta()
	run.ScanCount = 1
	run.Instrument.Comments = "caf\xe9" // invalid UTF-8
	run.Instrument.Model = "Instrument \xff"
	w, err := Create(path, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !utf8.ValidString(string(raw)) {
		t.Fatal("generated document is not valid UTF-8")
	}
	var d doc
	if err := xml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("invalid UTF-8 leaked into the document: %v", err)
	}
}

func TestOutOfOrderSpectrumRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.mzXML")
	run, src := runMeta()
	run.ScanCount = 2
	w, err := Create(path, run, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(spectrum(0, []float64{1}, []float64{2})); err == nil {
		t.Fatal("duplicate/out-of-order index accepted")
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(path + ".writing"); statErr == nil {
		t.Fatal("Abort left the temp file behind")
	}
}

func TestNonMonotonicSourceNumbersKeepNumbersAndDropIndex(t *testing.T) {
	a := spectrum(0, []float64{1}, []float64{2})
	a.ScanNumber = ptrI64(10)
	b := spectrum(1, []float64{1}, []float64{2})
	b.ScanNumber = ptrI64(5) // not strictly increasing
	path, w := writeDoc(t, Options{}, a, b)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	d := parseDoc(t, path)
	if d.Scans[0].Num != 10 || d.Scans[1].Num != 5 {
		t.Fatalf("source scan numbers must be preserved verbatim, got %d and %d", d.Scans[0].Num, d.Scans[1].Num)
	}
	if len(d.Index) != 0 {
		t.Fatalf("an index over unordered keys would break lookup; it must be omitted: %+v", d.Index)
	}
	if !strings.Contains(string(raw), `<indexOffset xsi:nil="true">`) {
		t.Fatal("expected a nil indexOffset")
	}
	if !hasCode(w.stats.Warnings, msdata.CodeMZXMLScanNumberFallback) {
		t.Fatalf("expected a fallback warning: %+v", w.stats.Warnings)
	}
}

func TestNonPositiveSourceScanNumberUsesOrdinal(t *testing.T) {
	a := spectrum(0, []float64{1}, []float64{2})
	a.ScanNumber = ptrI64(0) // vendors sometimes emit 0-based scan numbers
	b := spectrum(1, []float64{1}, []float64{2})
	b.ScanNumber = ptrI64(0)
	path, w := writeDoc(t, Options{}, a, b)
	d := parseDoc(t, path)
	if d.Scans[0].Num != 1 || d.Scans[1].Num != 2 {
		t.Fatalf("non-positive scan numbers must fall back to 1-based ordinals, got %d and %d",
			d.Scans[0].Num, d.Scans[1].Num)
	}
	if !hasCode(w.stats.Warnings, msdata.CodeMZXMLScanNumberFallback) {
		t.Fatalf("expected a fallback warning: %+v", w.stats.Warnings)
	}
}

// TestZeroBasedSourceNumberingShiftsWholeDocument locks the behaviour a real
// vendor file forces: gc01_0812_066.cdf numbers scans 0..9864 in
// actual_scan_number. Deciding the scheme per spectrum produced 1,1,2,3,... which
// silently collapsed two distinct spectra onto the same mzXML key, so the scheme
// is chosen once and applied everywhere.
func TestZeroBasedSourceNumberingShiftsWholeDocument(t *testing.T) {
	n := 5
	spectra := make([]*msdata.Spectrum, n)
	for i := range spectra {
		sp := spectrum(i, []float64{float64(i) + 1}, []float64{2})
		num := int64(i) // 0,1,2,...
		sp.ScanNumber = &num
		spectra[i] = sp
	}
	path, w := writeDoc(t, Options{}, spectra...)
	d := parseDoc(t, path)
	seen := map[int64]int{}
	for i, sc := range d.Scans {
		if sc.Num != int64(i+1) {
			t.Fatalf("scan %d num = %d, want %d", i, sc.Num, i+1)
		}
		seen[sc.Num]++
	}
	for num, count := range seen {
		if count != 1 {
			t.Fatalf("scan num %d used by %d spectra; mzXML keys must be unique", num, count)
		}
	}
	if len(d.Index) != 1 || len(d.Index[0].Offsets) != n {
		t.Fatalf("a dense 1-based numbering must keep the index usable: %+v", d.Index)
	}
	if !hasCode(w.stats.Warnings, msdata.CodeMZXMLScanNumberFallback) {
		t.Fatalf("the 0-based shift must be reported: %+v", w.stats.Warnings)
	}
}

func TestSourceScanNumbersPreserved(t *testing.T) {
	a := spectrum(0, []float64{1}, []float64{2})
	a.ScanNumber = ptrI64(19)
	b := spectrum(1, []float64{1}, []float64{2})
	b.ScanNumber = ptrI64(20)
	path, _ := writeDoc(t, Options{}, a, b)
	d := parseDoc(t, path)
	if d.Scans[0].Num != 19 || d.Scans[1].Num != 20 {
		t.Fatalf("source scan numbers were not preserved: %d, %d", d.Scans[0].Num, d.Scans[1].Num)
	}
	if d.Index[0].Offsets[0].ID != 19 {
		t.Fatalf("index key should follow scan/@num, got %d", d.Index[0].Offsets[0].ID)
	}
}

func TestDisableIndexWritesNilOffset(t *testing.T) {
	path, _ := writeDoc(t, Options{DisableIndex: true}, spectrum(0, []float64{1}, []float64{2}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "<index name=") {
		t.Fatal("index written although disabled")
	}
	if !strings.Contains(string(raw), `<indexOffset xsi:nil="true"></indexOffset>`) {
		t.Fatalf("disabled index must write a nil indexOffset:\n%s", string(raw)[len(raw)-400:])
	}
	var d doc
	if err := xml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Scans) != 1 {
		t.Fatalf("scans = %d", len(d.Scans))
	}
}

func TestDurationFormatting(t *testing.T) {
	cases := []struct {
		in   float64
		want string
		ok   bool
	}{
		{0, "PT0S", true},
		{305.582, "PT305.582S", true},
		{-1.5, "-PT1.5S", true},
		{1e21, "PT1000000000000000000000S", true}, // no exponent form in xs:duration
		{math.NaN(), "", false},
		{math.Inf(-1), "", false},
	}
	for _, c := range cases {
		got, ok := formatDuration(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("formatDuration(%v) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestDocumentHeaderStructure(t *testing.T) {
	path, _ := writeDoc(t, Options{}, spectrum(0, []float64{1}, []float64{2}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML declaration: %q", s[:60])
	}
	if !strings.Contains(s, `xmlns="`+Namespace+`"`) {
		t.Fatal("missing mzXML 3.2 namespace")
	}
	if !strings.Contains(s, `xsi:schemaLocation="`+SchemaLocation+`"`) {
		t.Fatal("missing schemaLocation")
	}
	// Element order inside msRun is fixed by the schema sequence.
	order := []string{"<parentFile ", "<msInstrument ", "<dataProcessing", "<scan ", "</msRun>"}
	pos := -1
	for _, tag := range order {
		i := strings.Index(s, tag)
		if i < 0 {
			t.Fatalf("missing %s", tag)
		}
		if i < pos {
			t.Fatalf("element %s out of schema order", tag)
		}
		pos = i
	}
	if !strings.Contains(s, `scanCount="1"`) {
		t.Fatal("scanCount missing")
	}
	if !strings.Contains(s, `startTime="PT0.25S"`) {
		t.Fatal("startTime missing")
	}
}

func TestProvenanceIsRecorded(t *testing.T) {
	path, _ := writeDoc(t, Options{SoftwareVersion: "1.2.3"}, spectrum(0, []float64{1}, []float64{2}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{
		`<software type="conversion" name="cdf2ms" version="1.2.3"/>`,
		`<processingOperation name="ANDI/MS to mzXML 3.2 conversion"`,
		"source SHA-256 " + strings.Repeat("b", 64),
		"scans=3 points=6",
		`<comment>` + "converted ",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("provenance missing %q", want)
		}
	}
}

func hasCode(ds []msdata.Diagnostic, c msdata.Code) bool {
	for _, d := range ds {
		if d.Code == c {
			return true
		}
	}
	return false
}

func ptrI64(v int64) *int64 { return &v }
