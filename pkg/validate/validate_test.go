package validate_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
	"github.com/metabolomics-us/cdf2ms/pkg/validate"
)

// corpus builds one synthetic ANDI file plus its mzML and mzXML conversions.
type corpus struct {
	src   string
	mzml  string
	mzxml string
}

func build(t *testing.T, opt testutil.ANDIOptions, prec string, compress bool) corpus {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "run01.cdf")
	if err := testutil.WriteANDI(src, opt); err != nil {
		t.Fatalf("write source: %v", err)
	}
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		Overwrite: true,
		Precision: prec,
		Compress:  compress,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if rep.Converted != 1 {
		t.Fatalf("conversion failed: %s", rep.Text())
	}
	return corpus{
		src:   src,
		mzml:  filepath.Join(dir, "run01.mzML"),
		mzxml: filepath.Join(dir, "run01.mzXML"),
	}
}

func defaultOpt() testutil.ANDIOptions {
	return testutil.ANDIOptions{ScanCount: 12, PointsPerScan: []int{20, 25, 30}}
}

// mutate copies path, applies fn to its bytes, and returns the new file.
func mutate(t *testing.T, path string, fn func([]byte) []byte) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := path + ".mutant"
	if err := os.WriteFile(out, fn(b), 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func check(t *testing.T, out, src string, opts validate.Options) (*validate.Result, error) {
	t.Helper()
	return validate.Document(context.Background(), out, src, opts)
}

func expectProblem(t *testing.T, out, src, why string) {
	t.Helper()
	res, err := check(t, out, src, validate.Options{})
	if err == nil && (res == nil || len(res.Problems) == 0) {
		t.Fatalf("verifier accepted a broken document (%s)\nresult: %+v", why, res)
	}
	t.Logf("caught %s: %v", why, err)
}

func TestHappyPathBothFormats(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	for _, f := range []string{c.mzml, c.mzxml} {
		res, err := check(t, f, c.src, validate.Options{})
		if err != nil {
			t.Fatalf("%s: %v (problems: %v)", filepath.Base(f), err, problemsOf(res))
		}
		if res.Spectra != 12 || res.Spectra != res.Declared || res.Declared != res.Written {
			t.Errorf("%s: counts %+v", filepath.Base(f), res)
		}
		if res.Points != 300 {
			t.Errorf("%s: points = %d, want 12 scans cycling 20/25/30 = 300", filepath.Base(f), res.Points)
		}
	}
}

func problemsOf(r *validate.Result) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Problems))
	for _, p := range r.Problems {
		out = append(out, fmt.Sprintf("%s: %s", p.Code, p.Message))
	}
	return out
}

func TestCompressionIsVerified(t *testing.T) {
	c := build(t, defaultOpt(), "auto", true)
	for _, f := range []string{c.mzml, c.mzxml} {
		if _, err := check(t, f, c.src, validate.Options{}); err != nil {
			t.Fatalf("compressed %s failed verification: %v", filepath.Base(f), err)
		}
	}
	// mzXML declares the compressed length; corrupting it must be caught.
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		return []byte(regexp.MustCompile(`compressedLen="[0-9]+"`).
			ReplaceAllString(string(b), `compressedLen="7"`))
	})
	expectProblem(t, mut, c.src, "wrong compressedLen")
}

// ---------- mzML mutations ----------

var reMzMLPayload = regexp.MustCompile(`(?s)(<binary>)([A-Za-z0-9+/=]{8,})(</binary>)`)

func TestMzMLDetectsSwappedArrays(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		s := string(b)
		// Swap the content terms of the first spectrum's two arrays: the document
		// stays schema-valid, but every number is filed under the wrong heading.
		i := strings.Index(s, `accession="MS:1000514"`)
		j := strings.Index(s, `accession="MS:1000515"`)
		if i < 0 || j < 0 || j < i {
			t.Fatalf("fixture layout changed: i=%d j=%d", i, j)
		}
		s = s[:i] + `accession="MS:1000999"` + s[i+len(`accession="MS:1000514"`):]
		s = s[:j] + `accession="MS:1000514"` + s[j+len(`accession="MS:1000515"`):]
		s = strings.Replace(s, `accession="MS:1000999"`, `accession="MS:1000515"`, 1)
		return []byte(s)
	})
	expectProblem(t, mut, c.src, "swapped m/z and intensity arrays")
}

func TestMzMLDetectsWrongByteOrder(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		s := string(b)
		m := reMzMLPayload.FindStringSubmatchIndex(s)
		if m == nil {
			t.Fatal("no base64 payload found")
		}
		raw, err := base64.StdEncoding.DecodeString(s[m[4]:m[5]])
		if err != nil {
			t.Fatalf("fixture payload is not base64: %v", err)
		}
		// Rewrite the array in big-endian order. mzML is little-endian, so this is
		// exactly the writer bug the verifier exists to catch.
		flipped := make([]byte, len(raw))
		for i := 0; i+4 <= len(raw); i += 4 {
			flipped[i], flipped[i+1], flipped[i+2], flipped[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
		}
		return []byte(s[:m[4]] + base64.StdEncoding.EncodeToString(flipped) + s[m[5]:])
	})
	expectProblem(t, mut, c.src, "big-endian mzML payload")
}

func TestMzMLDetectsWrongEncodedLength(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `encodedLength="`, `encodedLength="1" data-x="`, 1))
	})
	expectProblem(t, mut, c.src, "encodedLength that does not match the payload")
}

func TestMzMLDetectsMissingPrecisionTerm(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `<cvParam cvRef="MS" accession="MS:1000521" name="32-bit float"/>`,
			`<cvParam cvRef="MS" accession="MS:1000576" name="no compression"/>`, 1))
	})
	expectProblem(t, mut, c.src, "binary array without a precision term")
}

func TestMzMLDetectsTruncatedDocument(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		s := string(b)
		i := strings.LastIndex(s, "<spectrum ")
		if i <= 0 {
			t.Fatal("no spectrum element found")
		}
		return []byte(s[:i])
	})
	if res, err := check(t, mut, c.src, validate.Options{}); err == nil {
		t.Fatalf("truncated document accepted: %+v", res)
	}
}

func TestMzMLDetectsMissingSpectrum(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		s := string(b)
		start := strings.Index(s, "<spectrum ")
		end := strings.Index(s, "</spectrum>")
		if start < 0 || end < 0 {
			t.Fatal("no spectrum element found")
		}
		// Remove one whole spectrum: the XML stays well-formed, so only the declared
		// count reveals that a spectrum went missing.
		return []byte(s[:start] + s[end+len("</spectrum>"):])
	})
	res, err := check(t, mut, c.src, validate.Options{})
	if err == nil {
		t.Fatalf("a document missing a spectrum was accepted: %+v", res)
	}
	if msdata.CodeOf(err) != msdata.CodeCountMismatch {
		t.Errorf("code = %s, want %s (err=%v)", msdata.CodeOf(err), msdata.CodeCountMismatch, err)
	}
}

func TestMzMLDetectsSpectraBeyondSource(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzml, func(b []byte) []byte {
		s := string(b)
		start := strings.Index(s, "<spectrum ")
		end := strings.Index(s, "</spectrum>")
		if start < 0 || end < 0 {
			t.Fatal("no spectrum element found")
		}
		first := s[start : end+len("</spectrum>")]
		dup := strings.Replace(first, `index="0"`, `index="12"`, 1)
		dup = strings.Replace(dup, `id="index=0`, `id="index=12`, 1)
		// Raise the declared count so the extra spectrum is not caught by the
		// count check alone: the source itself has run out.
		s = strings.Replace(s, `spectrumList count="12"`, `spectrumList count="13"`, 1)
		idx := strings.LastIndex(s, "</spectrumList>")
		return []byte(s[:idx] + dup + s[idx:])
	})
	expectProblem(t, mut, c.src, "more spectra than the source contains")
}

// ---------- mzXML mutations ----------

var reMzXMLPayload = regexp.MustCompile(`(?s)contentType="m/z-int"[^>]*>([A-Za-z0-9+/=]{8,})<`)

func TestMzXMLDetectsWrongContentType(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `contentType="m/z-int"`, `contentType="zlib"`+`"`, 1))
	})
	expectProblem(t, mut, c.src, "peaks contentType that is not m/z-int")
}

func TestMzXMLDetectsLittleEndianPayload(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		s := string(b)
		m := reMzXMLPayload.FindStringSubmatchIndex(s)
		if m == nil {
			t.Fatal("no peaks payload found")
		}
		raw, err := base64.StdEncoding.DecodeString(s[m[2]:m[3]])
		if err != nil {
			t.Fatalf("fixture payload is not base64: %v", err)
		}
		swapped := make([]byte, len(raw))
		for i := 0; i+8 <= len(raw); i += 8 {
			// Reverse each 4-byte word inside both interleaved streams: the reader
			// expects network order, so the decoded numbers become garbage.
			swapped[i], swapped[i+1], swapped[i+2], swapped[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
			swapped[i+4], swapped[i+5], swapped[i+6], swapped[i+7] = raw[i+7], raw[i+6], raw[i+5], raw[i+4]
		}
		return []byte(s[:m[2]] + base64.StdEncoding.EncodeToString(swapped) + s[m[3]:])
	})
	expectProblem(t, mut, c.src, "little-endian mzXML payload")
}

func TestMzXMLDetectsBrokenIndexOffset(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		re := regexp.MustCompile(`(<offset id="[0-9]+">)([0-9]+)(</offset>)`)
		m := re.FindSubmatchIndex(b)
		if m == nil {
			t.Fatal("no index offset found")
		}
		// Shift one offset by a few bytes: it still lands inside a <scan>, but not
		// on its start, which is what an indexed consumer requires.
		var v int
		if _, err := fmt.Sscanf(string(b[m[4]:m[5]]), "%d", &v); err != nil {
			t.Fatalf("bad offset %q", b[m[4]:m[5]])
		}
		v += 7
		return append(append(append([]byte{}, b[:m[4]]...),
			[]byte(fmt.Sprintf("%d", v))...), b[m[5]:]...)
	})
	expectProblem(t, mut, c.src, "index offset that does not point at <scan>")
}

// TestMzXMLDetectsWrongIndexOffsetElement: consumers seek to <indexOffset> to
// find the index, so an indexOffset that misses <index> is a broken document
// even when the index itself is intact.
func TestMzXMLDetectsWrongIndexOffsetElement(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		re := regexp.MustCompile(`<indexOffset>([0-9]+)</indexOffset>`)
		m := re.FindSubmatchIndex(b)
		if m == nil {
			t.Fatal("no indexOffset element found")
		}
		var v int
		if _, err := fmt.Sscanf(string(b[m[2]:m[3]]), "%d", &v); err != nil {
			t.Fatalf("bad indexOffset %q", b[m[2]:m[3]])
		}
		return append(append(append([]byte{}, b[:m[2]]...),
			[]byte(fmt.Sprintf("%d", v-3))...), b[m[3]:]...)
	})
	expectProblem(t, mut, c.src, "indexOffset that does not point at <index>")
}

func TestMzXMLDetectsTamperedSHA1(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		re := regexp.MustCompile(`(?s)<sha1>([0-9a-f]{40})</sha1>`)
		m := re.FindSubmatchIndex(b)
		if m == nil {
			t.Fatal("no sha1 element found")
		}
		digits := []byte(b[m[2]:m[3]])
		if digits[0] == 'a' {
			digits[0] = 'b'
		} else {
			digits[0] = 'a'
		}
		return append(append(append([]byte{}, b[:m[2]]...), digits...), b[m[3]:]...)
	})
	expectProblem(t, mut, c.src, "sha1 that does not cover the document prefix")
}

func TestMzXMLDetectsPeaksCountMismatch(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		return []byte(strings.Replace(string(b), `peaksCount="20"`, `peaksCount="19"`, 1))
	})
	expectProblem(t, mut, c.src, "peaksCount that does not match the payload")
}

func TestMzXMLDetectsRenumberedScans(t *testing.T) {
	c := build(t, defaultOpt(), "auto", false)
	mut := mutate(t, c.mzxml, func(b []byte) []byte {
		// Every scan number doubled: strictly increasing, so monotonic, but no
		// longer matching the index keys the document itself advertises.
		re := regexp.MustCompile(`num="([0-9]+)"`)
		i := 0
		return re.ReplaceAllFunc(b, func(m []byte) []byte {
			i++
			return []byte(fmt.Sprintf(`num="%d"`, 100+i))
		})
	})
	expectProblem(t, mut, c.src, "scan numbers that disagree with the index")
}

// ---------- tolerance semantics ----------

func TestToleranceIsHonouredAndNotAssumed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "f64.cdf")
	// Double-precision source values: a float32 output cannot be exact.
	spec, err := testutil.ANDI(testutil.ANDIOptions{
		ScanCount: 6, PointsPerScan: []int{16}, RTUnit: "second", RTValueScale: 1,
		MassType: testutil.NCDouble, IncludeInstrumentVars: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.WriteNetCDF(src, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML}, Overwrite: true, Precision: "f32",
	}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "f64.mzML")

	if _, err := check(t, out, src, validate.Options{}); err == nil {
		t.Errorf("strict verification must reject a lossy document whose numbers differ")
	}
	res, err := check(t, out, src, validate.Options{Tolerance: 1e-3})
	if err != nil {
		t.Errorf("a stated tolerance should accept float32 rounding: %v (problems: %v)", err, problemsOf(res))
	}
}

func TestUnknownFormatAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "x.mzML")
	if err := os.WriteFile(bogus, []byte("<html><body>nope</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := check(t, bogus, filepath.Join(dir, "missing.cdf"), validate.Options{}); err == nil {
		t.Errorf("a non-mzML root must be refused")
	}
	if _, err := check(t, filepath.Join(dir, "nope.mzML"), filepath.Join(dir, "missing.cdf"),
		validate.Options{}); err == nil {
		t.Errorf("a missing output must be refused")
	}
}

func TestEmptySpectraVerify(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.cdf")
	if err := testutil.WriteANDI(src, testutil.ANDIOptions{ScanCount: 2, PointsPerScan: []int{0},
		ZeroScans: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML, convert.FormatMzXML}, Overwrite: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"empty.mzML", "empty.mzXML"} {
		res, err := check(t, filepath.Join(dir, name), src, validate.Options{})
		if err != nil {
			t.Errorf("%s: %v (problems: %v)", name, err, problemsOf(res))
		}
		if res == nil || res.Spectra != 2 || res.Points != 0 {
			t.Errorf("%s: %+v", name, res)
		}
	}
}
