package mzml

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

func f64(v float64) *float64 { return &v }

func testRun(n int) *msdata.Run {
	return &msdata.Run{
		ID:                      "fixture_run",
		ScanCount:               n,
		RetentionTimeUnit:       "second",
		RetentionTimeSourceUnit: "second",
		RetentionTimeUnitOrigin: "var:time_values.units",
		Instrument: msdata.Instrument{
			Name:           "Synthetic QMS",
			Manufacturer:   "Synthetic Instruments",
			Model:          "SIM-QMS",
			SerialNumber:   "SN-1",
			IonizationMode: "Electron Impact",
			DetectorType:   "Electron Multiplier",
			SeparationType: "GC",
			SoftwareVer:    "Acquisitor 3.1",
		},
		Metadata: map[string]msdata.MetadataValue{
			"global:sample_name":             {Kind: msdata.KindString, Str: "sample & <b1>", Origin: "global:sample_name"},
			"derived:ms_level":               {Kind: msdata.KindString, Str: "1", Origin: "derived:no_precursor_variables"},
			"derived:polarity":               {Kind: msdata.KindString, Str: "positive", Origin: "global:test_ionization_polarity"},
			"derived:centroid":               {Kind: msdata.KindString, Str: "centroid", Origin: "global:experiment_type"},
			"derived:scan_plan":              {Kind: msdata.KindString, Str: "scans=3", Origin: "var:scan_index+var:point_count"},
			"global:ms_acquisition_software": {Kind: msdata.KindString, Str: "Acquisitor", Origin: "global:ms_acquisition_software"},
		},
		SchemaFingerprint: "sha256:aaaabbbbccccdddd",
		VendorFingerprint: "sha256:1111222233334444",
		Source: msdata.SourceFile{
			Name: "fixture.CDF", Path: "/tmp/fixture.CDF", Size: 4096,
			SHA256:   "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Encoding: "CDF-1 (classic)",
		},
	}
}

func testSpectra(n int) []msdata.Spectrum {
	out := make([]msdata.Spectrum, 0, n)
	for i := 0; i < n; i++ {
		mz := []float64{100.5 + float64(i), 200.25 + float64(i), 300.125 + float64(i)}
		inten := []float64{1e3 + float64(i), 2e3, 3.5e3}
		lvl := 1
		if i%4 == 3 {
			lvl = 2
			mz = mz[:2]
			inten = inten[:2]
		}
		if i%7 == 6 {
			mz, inten = mz[:0], inten[:0]
		}
		sn := int64(1000 + i)
		sp := msdata.Spectrum{
			Index: i, MZ: mz, Intensity: inten,
			RetentionTime: float64(i) * 0.5, RetentionTimeSet: true,
			MSLevel: &lvl, ScanNumber: &sn,
			Polarity: msdata.PolarityPositive, Centroid: msdata.CentroidCentroided,
			TIC: f64(6500 + float64(i)),
		}
		if len(mz) > 0 {
			sp.LowestMZ = f64(mz[0])
			sp.HighestMZ = f64(mz[len(mz)-1])
			sp.BasePeakMZ = f64(200.25 + float64(i))
			sp.BasePeakInt = f64(3500)
		}
		if lvl == 2 {
			sp.PrecursorMZ = f64(445.5)
			sp.PrecursorInt = f64(1234)
			sp.PrecursorZ = func() *int { z := 2; return &z }()
			sp.CollisionEn = f64(35)
		}
		out = append(out, sp)
	}
	return out
}

func writeDoc(t *testing.T, opts Options, n int) (string, []msdata.Spectrum) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.mzML")
	spec := testSpectra(n)
	w, err := Create(path, testRun(n), testRun(n).Source, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := range spec {
		if err := w.Write(&spec[i]); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path, spec
}

// parsed holds structural facts re-read from a written document; numeric
// verification lives in readBackArrays.
type parsed struct {
	rootName       string
	ns             string
	cvCount        int
	declaredCount  int
	spectra        []parsedSpectrum
	elementOrder   []string
	cvParamsBySpec [][]string
	userParams     []string
}

type parsedSpectrum struct {
	index            int
	id               string
	defaultArrayLen  int
	arrayEncodedLens []int
	rt               float64
	hasRT            bool
	msLevel          int
}

// parseMzML re-reads the document with a strict XML decoder. Any
// not-well-formed document, or mismatched encodedLength, fails the test.
func parseMzML(t *testing.T, path string) parsed {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	dec.Strict = true
	var p parsed
	var cur *parsedSpectrum
	var inBinary bool
	var text strings.Builder
	var curArraySpec int = -1
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("XML parse error (document is not well-formed): %v", err)
		}
		switch se := tok.(type) {
		case xml.StartElement:
			if p.rootName == "" {
				p.rootName = se.Name.Local
				p.ns = se.Name.Space
			}
			p.elementOrder = append(p.elementOrder, se.Name.Local)
			switch se.Name.Local {
			case "cvList":
				p.cvCount = atoiAttr(t, se, "count")
			case "spectrumList":
				p.declaredCount = atoiAttr(t, se, "count")
			case "spectrum":
				p.spectra = append(p.spectra, parsedSpectrum{})
				cur = &p.spectra[len(p.spectra)-1]
				cur.index = atoiAttr(t, se, "index")
				cur.defaultArrayLen = atoiAttr(t, se, "defaultArrayLength")
				for _, a := range se.Attr {
					if a.Name.Local == "id" {
						cur.id = a.Value
					}
				}
				p.cvParamsBySpec = append(p.cvParamsBySpec, nil)
			case "cvParam":
				acc, name, value, unitName := "", "", "", ""
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "accession":
						acc = a.Value
					case "name":
						name = a.Value
					case "value":
						value = a.Value
					case "unitName":
						unitName = a.Value
					}
				}
				if n := len(p.cvParamsBySpec); n > 0 {
					p.cvParamsBySpec[n-1] = append(p.cvParamsBySpec[n-1], acc+"|"+name+"|"+value)
				}
				if cur != nil {
					switch acc {
					case "MS:1000016":
						cur.rt, _ = strconv.ParseFloat(value, 64)
						cur.hasRT = true
						if unitName != "second" {
							t.Errorf("scan start time unit %q, want second", unitName)
						}
					case "MS:1000511":
						cur.msLevel, _ = strconv.Atoi(value)
					}
				}
			case "userParam":
				var name, value string
				for _, a := range se.Attr {
					switch a.Name.Local {
					case "name":
						name = a.Value
					case "value":
						value = a.Value
					}
				}
				p.userParams = append(p.userParams, name+"="+value)
			case "binaryDataArray":
				curArraySpec = len(cur.arrayEncodedLens)
				encLen := atoiAttr(t, se, "encodedLength")
				if cur != nil {
					cur.arrayEncodedLens = append(cur.arrayEncodedLens, encLen)
				}
			case "binary":
				inBinary = true
				text.Reset()
			}
		case xml.CharData:
			if inBinary {
				text.Write([]byte(se))
			}
		case xml.EndElement:
			switch se.Name.Local {
			case "binary":
				inBinary = false
				raw, err := base64.StdEncoding.DecodeString(text.String())
				if err != nil {
					t.Fatalf("binary content is not valid base64: %v", err)
				}
				if cur != nil && curArraySpec >= 0 && curArraySpec < len(cur.arrayEncodedLens) {
					if cur.arrayEncodedLens[curArraySpec] != len(text.String()) {
						t.Errorf("encodedLength %d != actual base64 length %d (decoded %d bytes)",
							cur.arrayEncodedLens[curArraySpec], len(text.String()), len(raw))
					}
				}
			case "spectrum":
				cur = nil
				curArraySpec = -1
			}
		}
	}
	return p
}

func atoiAttr(t *testing.T, se xml.StartElement, name string) int {
	t.Helper()
	for _, a := range se.Attr {
		if a.Name.Local == name {
			v, err := strconv.Atoi(a.Value)
			if err != nil {
				t.Fatalf("attribute %s=%q is not an integer", name, a.Value)
			}
			return v
		}
	}
	t.Fatalf("attribute %s missing on %s", name, se.Name.Local)
	return 0
}

func decodeLE(raw []byte, bits int) []float64 {
	switch bits {
	case 32:
		out := make([]float64, 0, len(raw)/4)
		for i := 0; i+4 <= len(raw); i += 4 {
			out = append(out, float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i:]))))
		}
		return out
	case 64:
		out := make([]float64, 0, len(raw)/8)
		for i := 0; i+8 <= len(raw); i += 8 {
			out = append(out, math.Float64frombits(binary.LittleEndian.Uint64(raw[i:])))
		}
		return out
	}
	panic("unknown precision")
}

func TestWriterProducesWellFormedDocument(t *testing.T) {
	path, spec := writeDoc(t, Options{}, 12)
	p := parseMzML(t, path)
	if p.rootName != "mzML" {
		t.Errorf("root = %q, want mzML", p.rootName)
	}
	if p.ns != NamespaceURI {
		t.Errorf("namespace = %q, want %q", p.ns, NamespaceURI)
	}
	if p.cvCount != 2 {
		t.Errorf("cvList count = %d, want 2", p.cvCount)
	}
	if p.declaredCount != len(spec) {
		t.Errorf("spectrumList count = %d, want %d", p.declaredCount, len(spec))
	}
	if len(p.spectra) != p.declaredCount {
		t.Errorf("wrote %d spectra, header declares %d", len(p.spectra), p.declaredCount)
	}
	// Required top-level elements, in schema order.
	want := []string{"cvList", "fileDescription", "softwareList", "instrumentConfigurationList",
		"dataProcessingList", "run", "spectrumList"}
	idx := 0
	for _, w := range want {
		found := -1
		for i := idx; i < len(p.elementOrder); i++ {
			if p.elementOrder[i] == w {
				found = i
				break
			}
		}
		if found < 0 {
			t.Errorf("required element %s missing or out of order", w)
			continue
		}
		idx = found
	}
}

func TestWriterRoundTripsNumbersExactly(t *testing.T) {
	for _, prec := range []Precision{PrecisionAuto, PrecisionF32, PrecisionF64} {
		t.Run(fmt.Sprint(prec), func(t *testing.T) {
			path, spec := writeDoc(t, Options{Precision: prec}, 9)
			p := parseMzML(t, path)
			if len(p.spectra) != len(spec) {
				t.Fatalf("spectra = %d, want %d", len(p.spectra), len(spec))
			}
			for i, ps := range p.spectra {
				want := spec[i]
				if ps.index != want.Index {
					t.Errorf("spectrum %d: index = %d", i, ps.index)
				}
				if ps.defaultArrayLen != len(want.MZ) {
					t.Errorf("spectrum %d: defaultArrayLength = %d, want %d", i, ps.defaultArrayLen, len(want.MZ))
				}
				if want.MSLevel != nil && ps.msLevel != *want.MSLevel {
					t.Errorf("spectrum %d: ms level = %d, want %d", i, ps.msLevel, *want.MSLevel)
				}
				if !ps.hasRT {
					t.Errorf("spectrum %d: no scan start time", i)
				} else if math.Abs(ps.rt-want.RetentionTime) > 0 {
					t.Errorf("spectrum %d: rt = %v, want %v (exact match required)", i, ps.rt, want.RetentionTime)
				}
				gotMZ, gotInt, err := readBackArrays(t, path, i)
				if err != nil {
					t.Fatal(err)
				}
				if len(gotMZ) != len(want.MZ) || len(gotInt) != len(want.Intensity) {
					t.Fatalf("spectrum %d: array lengths %d/%d, want %d/%d",
						i, len(gotMZ), len(gotInt), len(want.MZ), len(want.Intensity))
				}
				tol := 0.0
				if prec == PrecisionF32 {
					tol = 1e-6
				}
				for j := range want.MZ {
					if math.Abs(gotMZ[j]-want.MZ[j]) > tol {
						t.Errorf("spectrum %d peak %d: mz = %v, want %v", i, j, gotMZ[j], want.MZ[j])
					}
					if math.Abs(gotInt[j]-want.Intensity[j]) > tol {
						t.Errorf("spectrum %d peak %d: intensity = %v, want %v", i, j, gotInt[j], want.Intensity[j])
					}
				}
			}
		})
	}
}

// readBackArrays re-reads one spectrum's binary arrays straight from the file.
func readBackArrays(t *testing.T, path string, wantIndex int) ([]float64, []float64, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	var specIdx = -1
	var arrays [][]float64
	var cur []float64
	var bits, content int
	var inBin bool
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		switch se := tok.(type) {
		case xml.StartElement:
			switch se.Name.Local {
			case "spectrum":
				specIdx++
				arrays = nil
				for _, a := range se.Attr {
					if a.Name.Local == "index" {
						v, _ := strconv.Atoi(a.Value)
						specIdx = v
					}
				}
			case "binaryDataArray":
				cur, bits, content = nil, 0, 0
			case "cvParam":
				for _, a := range se.Attr {
					if a.Name.Local != "accession" {
						continue
					}
					switch a.Value {
					case "MS:1000521":
						bits = 32
					case "MS:1000523":
						bits = 64
					case "MS:1000514":
						content = 1
					case "MS:1000515":
						content = 2
					}
				}
			case "binary":
				inBin, text = true, strings.Builder{}
			}
		case xml.CharData:
			if inBin {
				text.Write([]byte(se))
			}
		case xml.EndElement:
			switch se.Name.Local {
			case "binary":
				inBin = false
				raw, err := base64.StdEncoding.DecodeString(text.String())
				if err != nil {
					return nil, nil, err
				}
				cur = decodeLE(raw, bits)
			case "binaryDataArray":
				if specIdx == wantIndex {
					if content == 2 {
						// intensity array: append after m/z
						arrays = append(arrays, cur)
					} else {
						arrays = append([][]float64{cur}, arrays...)
					}
				}
			case "spectrum":
				if specIdx == wantIndex {
					if len(arrays) != 2 {
						return nil, nil, fmt.Errorf("spectrum %d has %d binary arrays, want 2", wantIndex, len(arrays))
					}
					return arrays[0], arrays[1], nil
				}
			}
		}
	}
	return nil, nil, fmt.Errorf("spectrum %d not found", wantIndex)
}

func TestWriterPrecisionAutoUsesFloat32OnlyWhenLossless(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auto.mzML")
	run := testRun(2)
	w, err := Create(path, run, run.Source, Options{Precision: PrecisionAuto})
	if err != nil {
		t.Fatal(err)
	}
	float32Exact := msdata.Spectrum{Index: 0, MZ: []float64{1.5, 2.25}, Intensity: []float64{1, 2},
		RetentionTime: 1, RetentionTimeSet: true}
	needsDouble := msdata.Spectrum{Index: 1, MZ: []float64{1.0000000000000002},
		Intensity: []float64{1e300}, RetentionTime: 2, RetentionTimeSet: true}
	for _, sp := range []*msdata.Spectrum{&float32Exact, &needsDouble} {
		if err := w.Write(sp); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.stats.F32Arrays != 2 || w.stats.F64Arrays != 2 {
		t.Errorf("precision split = f32:%d f64:%d, want f32:2 f64:2", w.stats.F32Arrays, w.stats.F64Arrays)
	}
	// The double spectrum must survive exactly.
	mz, _, err := readBackArrays(t, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mz[0] != 1.0000000000000002 {
		t.Errorf("64-bit value not preserved: %v", mz[0])
	}
}

func TestWriterCountMismatchIsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.mzML")
	run := testRun(5)
	w, err := Create(path, run, run.Source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	spec := testSpectra(3)
	for i := range spec {
		if err := w.Write(&spec[i]); err != nil {
			t.Fatal(err)
		}
	}
	err = w.Close()
	if err == nil {
		t.Fatal("Close accepted a truncated spectrum list")
	}
	if !strings.Contains(err.Error(), "count mismatch") {
		t.Errorf("error = %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("invalid document was published at the final path")
	}
	if _, statErr := os.Stat(path + ".partial"); statErr != nil {
		t.Error("truncated document was not preserved as .partial for inspection")
	}
}

func TestWriterEscapesHostileMetadata(t *testing.T) {
	run := testRun(1)
	run.Metadata["global:sample_name"] = msdata.MetadataValue{Kind: msdata.KindString,
		Str: "bad \" & <tag> \x01 \xc3\x28 end", Origin: "global:sample_name"}
	run.Metadata["var:instrument_name"] = msdata.MetadataValue{Kind: msdata.KindString,
		Str: "unicode: \u00e9\u4e2d\u6587 emoji \U0001F600", Origin: "var:instrument_name"}
	path := filepath.Join(t.TempDir(), "esc.mzML")
	sp := testSpectra(1)[0]
	w, err := Create(path, run, run.Source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&sp); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	p := parseMzML(t, path) // fails the test if not well-formed
	found := false
	for _, u := range p.userParams {
		if strings.Contains(u, "unicode:") {
			found = true
			if !strings.Contains(u, "\u00e9") || !strings.Contains(u, "\U0001F600") {
				t.Errorf("unicode metadata was mangled: %q", u)
			}
		}
	}
	if !found {
		t.Error("unicode metadata missing from output")
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "\x01") {
		t.Error("a raw control byte reached the document")
	}
}

func TestWriterEmitsPrecursorOnlyForMSn(t *testing.T) {
	path, _ := writeDoc(t, Options{}, 8)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if n := strings.Count(doc, "<precursorList"); n != 2 {
		t.Errorf("precursorList count = %d, want 2 (MS2 spectra only)", n)
	}
	if strings.Contains(doc, `isolation window lower offset`) {
		t.Error("writer invented an isolation window width the source never declared")
	}
}

func TestWriterOmitsUnknownCentroidAndPolarity(t *testing.T) {
	run := testRun(1)
	delete(run.Metadata, "derived:centroid")
	delete(run.Metadata, "derived:polarity")
	path := filepath.Join(t.TempDir(), "unknown.mzML")
	sp := testSpectra(1)[0]
	sp.Centroid = msdata.CentroidUnknown
	sp.Polarity = msdata.PolarityUnknown
	w, err := Create(path, run, run.Source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&sp); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	doc := string(raw)
	if strings.Contains(doc, "MS:1000127") || strings.Contains(doc, "MS:1000128") {
		t.Error("writer asserted a centroid state the source did not state")
	}
	if strings.Contains(doc, "MS:1000129") || strings.Contains(doc, "MS:1000130") {
		t.Error("writer asserted a polarity the source did not state")
	}
}

func TestWriterRejectsOutOfOrderIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.mzML")
	run := testRun(3)
	w, err := Create(path, run, run.Source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sp := msdata.Spectrum{Index: 1}
	if err := w.Write(&sp); err == nil {
		t.Error("accepted a spectrum with index 1 as the first spectrum")
	}
	w.Abort()
	if _, err := os.Stat(path); err == nil {
		t.Error("Abort left a document behind")
	}
}
