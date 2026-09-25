// Package validate re-opens documents cdf2ms produced and proves they say what
// the source said.
//
// It is a second, independent implementation of the read path: it parses the XML
// from scratch with encoding/xml, decodes base64 and zlib itself, applies the
// byte order each format mandates (mzML little-endian, mzXML network order), and
// compares every value against a freshly opened ANDI reader. A bug in the writer
// and a matching bug in a writer-aware reader would cancel each other out; this
// reader shares no encoding code with either writer, so it cannot.
//
// The Python validators in scripts/ remain the outermost gate: they check the
// official schemas with lxml and cross-read with netCDF4 and pyteomics.
package validate

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/andiio"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// Options controls a verification pass.
type Options struct {
	// Tolerance is the permitted absolute difference. Zero means every value must
	// be bit-identical after decoding, which is what cdf2ms guarantees for
	// auto/float64 output; float32 output needs a tolerance.
	Tolerance float64

	// MaxSpectra caps the comparison (0 = all).
	MaxSpectra int64

	// RTUnit must match the policy used for conversion; retention times are
	// compared against the source in seconds.
	RTUnit string

	// SkipSHA1 omits the mzXML <sha1> re-computation, which reads the whole file.
	SkipSHA1 bool

	// SkipIndex omits the mzXML byte-offset index verification.
	SkipIndex bool
}

// Problem is one conformance or value discrepancy.
type Problem struct {
	ScanIndex int         `json:"scanIndex,omitempty"`
	Code      msdata.Code `json:"code"`
	Message   string      `json:"message"`
}

// Result is the outcome of one verification pass.
type Result struct {
	Path     string    `json:"path"`
	Format   string    `json:"format"`
	Spectra  int64     `json:"spectra"`
	Points   int64     `json:"points"`
	Problems []Problem `json:"problems,omitempty"`
	IndexOK  bool      `json:"indexOk,omitempty"`
	SHA1OK   bool      `json:"sha1Ok,omitempty"`
	Declared int64     `json:"declaredSpectra"`
	Written  int64     `json:"actualSpectra"`
}

// ErrProblems reports that verification found discrepancies; the details are on
// the Result.
var ErrProblems = errors.New("cdf2ms: output verification found problems")

const maxReported = 40

// Document verifies one output file against the ANDI source it was made from.
//
// A returned error means verification could not be carried out (unreadable file,
// unsupported format) or that it found a hard problem; consult Result.Problems
// for the itemised list either way.
func Document(ctx context.Context, outPath, srcPath string, opts Options) (*Result, error) {
	format, err := sniffFormat(outPath)
	if err != nil {
		return nil, err
	}
	policy, err := andiio.ParseRTUnitPolicy(opts.RTUnit)
	if err != nil {
		return nil, msdata.WrapError(msdata.CodeInvalidOption, err, "verifying %s", outPath)
	}
	src, err := andiio.Open(srcPath, andiio.Options{RTUnit: policy})
	if err != nil {
		return nil, msdata.WrapError(msdata.CodeOf(err), err, "opening source %s for verification", srcPath)
	}
	defer src.Close()

	res := &Result{Path: outPath, Format: format}
	switch format {
	case "mzML":
		err = verifyMzML(ctx, outPath, src, opts, res)
	case "mzXML":
		err = verifyMzXML(ctx, outPath, src, opts, res)
	default:
		return nil, fmt.Errorf("validate: unsupported format %q", format)
	}
	if err != nil {
		return res, err
	}
	if len(res.Problems) > 0 {
		return res, msdata.WrapError(codeOfProblems(res.Problems), ErrProblems,
			"%s: %d problems (first: %s)", outPath, len(res.Problems), res.Problems[0].Message)
	}
	return res, nil
}

func codeOfProblems(ps []Problem) msdata.Code {
	for _, p := range ps {
		if p.Code == msdata.CodeCountMismatch {
			return msdata.CodeCountMismatch
		}
	}
	for _, p := range ps {
		if p.Code != "" {
			return p.Code
		}
	}
	return msdata.CodeNumericMismatch
}

func sniffFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", msdata.WrapError(msdata.CodeFileNotFound, err, "opening %s", path)
	}
	defer f.Close()
	dec := xml.NewDecoder(f)
	for {
		tk, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("validate: %s contains no XML element", path)
		}
		if err != nil {
			return "", msdata.WrapError(msdata.CodeMZMLValidationFailed, err, "parsing %s", path)
		}
		if se, ok := tk.(xml.StartElement); ok {
			switch se.Name.Local {
			case "mzML":
				return "mzML", nil
			case "mzXML":
				return "mzXML", nil
			}
			return "", fmt.Errorf("validate: %s has root <%s>, expected mzML or mzXML", path, se.Name.Local)
		}
	}
}

// spectrumData is what an independent reader must recover from one spectrum.
type spectrumData struct {
	declaredLen int
	mz          []float64
	intensity   []float64
	rt          float64
	rtSet       bool
	msLevel     int
	polarity    msdata.Polarity
	centroid    msdata.CentroidState
}

// ---------- mzML ----------

const mzMLNS = "http://psi.hupo.org/ms/mzml"

func verifyMzML(ctx context.Context, path string, src *andiio.Reader, opts Options, res *Result) error {
	f, err := os.Open(path)
	if err != nil {
		return msdata.WrapError(msdata.CodeFileNotFound, err, "opening %s", path)
	}
	defer f.Close()
	dec := xml.NewDecoder(bufReader(f))
	dec.Strict = true

	var declared int64 = -1
	var pos int64
	buf := &arrayBuf{}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tk, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res.fail(msdata.CodeMZMLValidationFailed, "XML parse error: %v", err)
		}
		se, ok := tk.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "spectrumList":
			declared = attrInt(se, "count", -1)
			res.Declared = declared
		case "spectrum":
			if opts.MaxSpectra > 0 && pos >= opts.MaxSpectra {
				continue
			}
			sd := &spectrumData{}
			idx := attrInt(se, "index", -1)
			if idx != pos {
				res.problem(int(pos), msdata.CodeCountMismatch,
					"spectrum @index is %d at document position %d", idx, pos)
			}
			length := attrInt(se, "defaultArrayLength", -1)
			sd.declaredLen = int(length)
			if err := readMzMLSpectrum(dec, se, sd, buf); err != nil {
				return res.fail(msdata.CodeMZMLValidationFailed, "%v", err)
			}
			if err := compareSpectrum(src, pos, sd, opts, res); err != nil {
				return err
			}
			res.Spectra++
			res.Points += int64(len(sd.mz))
			pos++
		}
	}
	if declared < 0 {
		return res.fail(msdata.CodeMZMLValidationFailed, "no <spectrumList count=...> was found")
	}
	stats := src.Stats()
	if declared != stats.Spectra {
		res.problem(0, msdata.CodeCountMismatch,
			"spectrumList @count is %d but the source has %d spectra", declared, stats.Spectra)
	}
	res.Written = pos
	if pos != declared {
		res.problem(0, msdata.CodeCountMismatch,
			"spectrumList @count is %d but %d <spectrum> elements were written", declared, pos)
	}
	return nil
}

// readMzMLSpectrum consumes the remainder of one <spectrum> element.
func readMzMLSpectrum(dec *xml.Decoder, se xml.StartElement, sd *spectrumData, buf *arrayBuf) error {
	depth := 1
	for depth > 0 {
		tk, err := dec.Token()
		if err != nil {
			return err
		}
		switch el := tk.(type) {
		case xml.StartElement:
			depth++
			switch el.Name.Local {
			case "cvParam":
				acc := attrStr(el, "accession")
				val := attrStr(el, "value")
				switch acc {
				case "MS:1000016": // scan start time
					if u := attrStr(el, "unitName"); u != "second" {
						return fmt.Errorf("scan start time unit is %q, expected \"second\"", u)
					}
					if f, err := strconv.ParseFloat(val, 64); err == nil {
						sd.rt, sd.rtSet = f, true
					}
				case "MS:1000511":
					sd.msLevel, _ = strconv.Atoi(val)
				case "MS:1000130":
					sd.polarity = msdata.PolarityPositive
				case "MS:1000129":
					sd.polarity = msdata.PolarityNegative
				case "MS:1000127":
					sd.centroid = msdata.CentroidCentroided
				case "MS:1000128":
					sd.centroid = msdata.CentroidProfile
				}
			case "binaryDataArray":
				encodedLen := int(attrInt(el, "encodedLength", -1))
				info, text, err := readBinaryElement(dec)
				// readBinaryElement consumed the array's own EndElement, so the
				// increment above has to be undone here or the spectrum would never
				// close and the next spectrum's arrays would land on this one.
				depth--
				if err != nil {
					return err
				}
				if encodedLen >= 0 && encodedLen != info.encodedChars {
					return fmt.Errorf("%s: encodedLength is %d but the base64 payload has %d characters",
						info.label, encodedLen, info.encodedChars)
				}
				switch info.kind {
				case "mz":
					if sd.mz != nil {
						return fmt.Errorf("document has two m/z arrays in one spectrum")
					}
					sd.mz = info.values
					buf.mzOwned = text
				case "intensity":
					if sd.intensity != nil {
						return fmt.Errorf("document has two intensity arrays in one spectrum")
					}
					sd.intensity = info.values
					buf.intOwned = text
				default:
					return fmt.Errorf("binary array carries neither %s nor %s",
						"MS:1000514 (m/z array)", "MS:1000515 (intensity array)")
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

type arrayInfo struct {
	values       []float64
	encodedChars int
	label        string
	kind         string // "mz" | "intensity" | ""
}

// readBinaryElement reads one <binaryDataArray> body: its precision, compression
// and content parameters plus the base64 (optionally zlib-compressed) payload.
// The content term is what distinguishes the m/z array from the intensity array,
// so an array without one is a hard failure: guessing by array order would let a
// writer that swapped the two pass unnoticed.
func readBinaryElement(dec *xml.Decoder) (arrayInfo, string, error) {
	depth := 1
	bits := -1
	compressed := false
	kind := ""
	label := "binary array"
	var payload strings.Builder
	for depth > 0 {
		tk, err := dec.Token()
		if err != nil {
			return arrayInfo{}, "", err
		}
		switch el := tk.(type) {
		case xml.StartElement:
			depth++
			if el.Name.Local == "cvParam" {
				switch attrStr(el, "accession") {
				case "MS:1000521":
					bits = 32
				case "MS:1000523":
					bits = 64
				case "MS:1000574":
					compressed = true
				case "MS:1000513", "MS:1000514", "MS:1000515":
					label = attrStr(el, "name")
					switch attrStr(el, "accession") {
					case "MS:1000514":
						kind = "mz"
					case "MS:1000515":
						kind = "intensity"
					}
				}
			}
		case xml.CharData:
			if strings.TrimSpace(string(el)) != "" {
				payload.Write(el)
			}
		case xml.EndElement:
			depth--
		}
	}
	text := strings.TrimSpace(payload.String())
	if bits < 0 {
		return arrayInfo{}, text, fmt.Errorf("%s: no 32-bit (MS:1000521) or 64-bit (MS:1000523) precision term", label)
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return arrayInfo{}, text, fmt.Errorf("%s: base64 decode: %w", label, err)
	}
	if compressed {
		raw, err = zlibDecompress(raw)
		if err != nil {
			return arrayInfo{}, text, fmt.Errorf("%s: %w", label, err)
		}
	}
	vals, err := decodeLittleEndian(raw, bits)
	if err != nil {
		return arrayInfo{}, text, fmt.Errorf("%s: %w", label, err)
	}
	return arrayInfo{values: vals, encodedChars: len(text), label: label, kind: kind}, text, nil
}

func zlibDecompress(raw []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("zlib reader: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("zlib decompress: %w", err)
	}
	return out, nil
}

// arrayBuf holds reusable scratch buffers so verification of a large corpus does
// not allocate per spectrum.
// decodeLittleEndian turns an mzML binary payload into float64 values.
//
// mzML mandates little-endian ("the byte order is not a matter of taste"), so this
// reader assumes it. A writer that emitted network order would yield values that
// fail the comparison against the source, which is exactly the point.
func decodeLittleEndian(raw []byte, bits int) ([]float64, error) {
	if bits != 32 && bits != 64 {
		return nil, fmt.Errorf("unsupported array precision %d bits", bits)
	}
	if len(raw)%(bits/8) != 0 {
		return nil, fmt.Errorf("payload of %d bytes is not a whole number of %d-bit values", len(raw), bits)
	}
	n := len(raw) / (bits / 8)
	out := make([]float64, n)
	if bits == 32 {
		for i := 0; i < n; i++ {
			b := raw[i*4:]
			u := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
			out[i] = float64(math.Float32frombits(u))
		}
		return out, nil
	}
	for i := 0; i < n; i++ {
		b := raw[i*8:]
		u := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		out[i] = math.Float64frombits(u)
	}
	return out, nil
}

// arrayBuf keeps the decoded payloads' backing text alive so a caller can inspect
// the exact characters that were written; the slices themselves are freshly
// allocated per array because comparison happens before the next spectrum.
type arrayBuf struct {
	mzOwned  string
	intOwned string
}

// bufReader wraps a file in a large buffer: XML tokenising a multi-hundred-megabyte
// document one sysread at a time dominates runtime otherwise.
func bufReader(f *os.File) *bufio.Reader { return bufio.NewReaderSize(f, 1<<20) }

var reOffset = regexp.MustCompile(`<offset id="(-?[0-9]+)">(-?[0-9]+)</offset>`)

// parseISODuration converts an xs:duration of the PT...S form to seconds.
func parseISODuration(s string) (float64, bool) {
	m := reDuration.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	sign := 1.0
	if m[1] == "-" {
		sign = -1
	}
	total := 0.0
	any := false
	add := func(part string, mult float64) {
		if part == "" {
			return
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return
		}
		total += v * mult
		any = true
	}
	add(m[2], 3600) // hours
	add(m[3], 60)   // minutes
	add(m[4], 1)    // seconds
	if !any {
		return 0, false
	}
	return sign * total, true
}

var reDuration = regexp.MustCompile(`^(-)?PT(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?$`)

// ---------- mzXML ----------

const mzXMLNS = "http://sashimi.sourceforge.net/schema_revision/mzXML_3.2"

func verifyMzXML(ctx context.Context, path string, src *andiio.Reader, opts Options, res *Result) error {
	f, err := os.Open(path)
	if err != nil {
		return msdata.WrapError(msdata.CodeFileNotFound, err, "opening %s", path)
	}
	defer f.Close()
	dec := xml.NewDecoder(bufReader(f))

	var declared int64 = -1
	var pos int64
	var nums []int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tk, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return res.fail(msdata.CodeMZXMLValidationFailed, "XML parse error: %v", err)
		}
		se, ok := tk.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "msRun":
			declared = attrInt(se, "scanCount", -1)
			res.Declared = declared
		case "scan":
			if opts.MaxSpectra > 0 && pos >= opts.MaxSpectra {
				continue
			}
			num := attrInt(se, "num", -1)
			if num != pos+1 && !numsContainExpected(nums, pos, num) {
				res.problem(int(pos), msdata.CodeCountMismatch,
					"scan @num is %d at document position %d (expected %d)", num, pos, pos+1)
			}
			nums = append(nums, num)
			sd := &spectrumData{}
			sd.declaredLen = int(attrInt(se, "peaksCount", -1))
			if v := attrStr(se, "retentionTime"); v != "" {
				secs, ok := parseISODuration(v)
				if !ok {
					return res.fail(msdata.CodeMZXMLValidationFailed,
						"scan @num %d: retentionTime %q is not an ISO 8601 duration", num, v)
				}
				sd.rt, sd.rtSet = secs, true
			}
			sd.msLevel = int(attrInt(se, "msLevel", 0))
			switch attrStr(se, "polarity") {
			case "+":
				sd.polarity = msdata.PolarityPositive
			case "-":
				sd.polarity = msdata.PolarityNegative
			}
			switch attrStr(se, "centroided") {
			case "1", "true":
				sd.centroid = msdata.CentroidCentroided
			case "0", "false":
				sd.centroid = msdata.CentroidProfile
			}
			if err := readMzXMLPeaks(dec, sd); err != nil {
				return res.fail(msdata.CodeMZXMLValidationFailed, "%v", err)
			}
			if err := compareSpectrum(src, pos, sd, opts, res); err != nil {
				return err
			}
			res.Spectra++
			res.Points += int64(len(sd.mz))
			pos++
		}
	}
	if declared < 0 {
		return res.fail(msdata.CodeMZXMLValidationFailed, "no <msRun scanCount=...> was found")
	}
	stats := src.Stats()
	if declared != stats.Spectra {
		res.problem(0, msdata.CodeCountMismatch,
			"msRun @scanCount is %d but the source has %d spectra", declared, stats.Spectra)
	}
	res.Written = pos
	if pos != declared {
		res.problem(0, msdata.CodeCountMismatch,
			"msRun @scanCount is %d but %d <scan> elements were written", declared, pos)
	}
	if !opts.SkipIndex || !opts.SkipSHA1 {
		if err := verifyMzXMLIndex(path, nums, res, opts); err != nil {
			return err
		}
	}
	return nil
}

// numsContainExpected tolerates a document whose @num values are the source's own
// scan numbers, as long as they form a strictly increasing sequence.
func numsContainExpected(nums []int64, pos, num int64) bool {
	if pos == 0 {
		return num > 0
	}
	return num > nums[pos-1]
}

func readMzXMLPeaks(dec *xml.Decoder, sd *spectrumData) error {
	depth := 0
	var (
		precision = 32
		byteOrder string
		content   string
		compType  string
		compLen   int64 = -1
		isNil     bool
		payload   strings.Builder
		seenPeaks bool
	)
	for {
		tk, err := dec.Token()
		if err != nil {
			return err
		}
		switch el := tk.(type) {
		case xml.StartElement:
			if el.Name.Local != "peaks" {
				continue // precursorMz and friends are compared by the source
			}
			seenPeaks = true
			depth = 1
			precision = int(attrInt(el, "precision", 32))
			byteOrder = attrStr(el, "byteOrder")
			content = attrStr(el, "contentType")
			compType = attrStr(el, "compressionType")
			compLen = attrInt(el, "compressedLen", -1)
			if attrXsiNil(el) {
				isNil = true
			}
		case xml.CharData:
			if seenPeaks && depth == 1 && strings.TrimSpace(string(el)) != "" {
				payload.Write(el)
			}
		case xml.EndElement:
			if seenPeaks {
				depth--
				if depth == 0 {
					return finishPeaks(sd, precision, byteOrder, content, compType, compLen, isNil,
						strings.TrimSpace(payload.String()))
				}
			}
		}
	}
}

func finishPeaks(sd *spectrumData, precision int, byteOrder, content, compType string, compLen int64,
	isNil bool, payload string) error {
	switch byteOrder {
	case "network":
	default:
		return fmt.Errorf("peaks byteOrder is %q; mzXML requires \"network\" (big-endian)", byteOrder)
	}
	if content != "m/z-int" {
		return fmt.Errorf("peaks contentType is %q; cdf2ms writes interleaved \"m/z-int\"", content)
	}
	if isNil {
		if sd.declaredLen != 0 {
			return fmt.Errorf("peaks are xsi:nil but peaksCount is %d", sd.declaredLen)
		}
		if compType != "none" || compLen != 0 {
			return fmt.Errorf("nil peaks must declare compressionType=\"none\" compressedLen=\"0\"")
		}
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return fmt.Errorf("peaks base64 decode: %w", err)
	}
	switch compType {
	case "none":
		if compLen != 0 {
			return fmt.Errorf("compressedLen is %d for uncompressed data", compLen)
		}
	case "zlib":
		if compLen != int64(len(raw)) {
			return fmt.Errorf("compressedLen is %d but the compressed payload is %d bytes", compLen, len(raw))
		}
		raw, err = zlibDecompress(raw)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown compressionType %q", compType)
	}
	width := precision / 8
	if width != 4 && width != 8 {
		return fmt.Errorf("precision %d is neither 32 nor 64 bits", precision)
	}
	if len(raw)%(2*width) != 0 {
		return fmt.Errorf("interleaved payload of %d bytes is not a whole number of %d-bit pairs",
			len(raw), precision)
	}
	n := len(raw) / (2 * width)
	if sd.declaredLen >= 0 && sd.declaredLen != n {
		return fmt.Errorf("peaksCount is %d but the payload holds %d pairs", sd.declaredLen, n)
	}
	sd.mz = make([]float64, n)
	sd.intensity = make([]float64, n)
	for i := 0; i < n; i++ {
		base := raw[i*2*width:]
		if width == 4 {
			sd.mz[i] = float64(be32(base))
			sd.intensity[i] = float64(be32(base[4:]))
		} else {
			sd.mz[i] = be64(base)
			sd.intensity[i] = be64(base[8:])
		}
	}
	return nil
}

func be32(b []byte) float32 {
	u := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return math.Float32frombits(u)
}

func be64(b []byte) float64 {
	u := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
	return math.Float64frombits(u)
}

// verifyMzXMLIndex checks the byte-offset index and the <sha1> checksum against
// the bytes actually on disk, the way an indexed consumer reads them.
func verifyMzXMLIndex(path string, nums []int64, res *Result, opts Options) error {
	f, err := os.Open(path)
	if err != nil {
		return msdata.WrapError(msdata.CodeFileNotFound, err, "opening %s", path)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()

	// Find the index the way an indexed consumer does: follow <indexOffset> from
	// the document tail to the start of <index>. A fixed tail window is not
	// enough -- the index grows about 40 bytes per scan, and a 56,660-scan GC-TOF
	// run has a 2.3 MB index whose start lay outside a 1 MB window, so a correct
	// document was reported as having no index at all.
	sect, err := readMzXMLIndex(f, size, res)
	if err != nil {
		return err
	}
	offsets := []int64{}
	ids := []int64{}
	for _, m := range reOffset.FindAllStringSubmatch(sect, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		off, _ := strconv.ParseInt(m[2], 10, 64)
		ids = append(ids, id)
		offsets = append(offsets, off)
	}
	if opts.SkipIndex {
		res.IndexOK = true
	} else {
		if len(offsets) == 0 {
			if len(nums) > 0 {
				res.problem(0, msdata.CodeMZXMLValidationFailed,
					"no scan index was found although %d scans were written", len(nums))
			}
		} else {
			if len(offsets) != len(nums) {
				res.problem(0, msdata.CodeMZXMLValidationFailed,
					"index has %d offsets for %d scans", len(offsets), len(nums))
			}
			head := make([]byte, 96)
			bad := 0
			for i, off := range offsets {
				if off <= 0 || off >= size {
					if bad < 5 {
						res.problem(i, msdata.CodeMZXMLValidationFailed,
							"index offset %d is outside the file (%d bytes)", off, size)
					}
					bad++
					continue
				}
				if i < len(ids) && len(nums) == len(ids) && ids[i] != nums[i] {
					if bad < 5 {
						res.problem(i, msdata.CodeMZXMLValidationFailed,
							"index key %d does not match scan/@num %d", ids[i], nums[i])
					}
					bad++
					continue
				}
				n, err := f.ReadAt(head, off)
				if n < 8 && err != nil {
					return err
				}
				window := string(head[:n])
				if !strings.HasPrefix(window, "<scan ") {
					if bad < 5 {
						res.problem(i, msdata.CodeMZXMLValidationFailed,
							"index offset %d does not point at a <scan> element: %q", off, first40(window))
					}
					bad++
					continue
				}
				if i < len(nums) {
					want := `num="` + strconv.FormatInt(nums[i], 10) + `"`
					if !strings.Contains(window, want) {
						if bad < 5 {
							res.problem(i, msdata.CodeMZXMLValidationFailed,
								"index offset %d points at a scan without %s: %q", off, want, first40(window))
						}
						bad++
						continue
					}
				}
			}
			res.IndexOK = bad == 0
		}
	}

	if opts.SkipSHA1 {
		res.SHA1OK = true
		return nil
	}
	// <sha1> covers every byte of the file up to and including its own opening tag.
	shaIdx, err := findTag(f, size, "<sha1>")
	if err != nil {
		return err
	}
	if shaIdx < 0 {
		res.problem(0, msdata.CodeMZXMLValidationFailed, "no <sha1> element was found")
		return nil
	}
	hashEnd := shaIdx + int64(len("<sha1>"))
	h := sha1.New()
	if _, err := io.CopyN(h, io.NewSectionReader(f, 0, hashEnd), hashEnd); err != nil && err != io.EOF {
		return err
	}
	want := fmt.Sprintf("%x", h.Sum(nil))
	rest := make([]byte, 64)
	n, _ := f.ReadAt(rest, hashEnd)
	got := strings.TrimSpace(strings.SplitN(string(rest[:n]), "</sha1>", 2)[0])
	if got != want {
		res.problem(0, msdata.CodeMZXMLValidationFailed,
			"<sha1> is %s but the SHA-1 of the document prefix is %s", got, want)
		return nil
	}
	res.SHA1OK = true
	return nil
}

func first40(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

// mzXMLTailWindow bounds the read that locates <indexOffset>; after it come
// only <sha1> and the closing tag.
const mzXMLTailWindow = 64 << 10

var reIndexOffset = regexp.MustCompile(`<indexOffset>\s*(-?[0-9]+)\s*</indexOffset>`)

// readMzXMLIndex returns the <index name="scan"> section that <indexOffset>
// points at, or "" when the document declares no index. An offset that does
// not land on the index is reported as a problem, since consumers seek there.
func readMzXMLIndex(f *os.File, size int64, res *Result) (string, error) {
	tail := int64(mzXMLTailWindow)
	if size < tail {
		tail = size
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, size-tail); err != nil && err != io.EOF {
		return "", err
	}
	all := reIndexOffset.FindAllSubmatch(buf, -1)
	if len(all) == 0 {
		return "", nil
	}
	off, err := strconv.ParseInt(string(all[len(all)-1][1]), 10, 64)
	if err != nil || off <= 0 || off >= size {
		res.problem(0, msdata.CodeMZXMLValidationFailed,
			"indexOffset %s is outside the file (%d bytes)", all[len(all)-1][1], size)
		return "", nil
	}
	sect := make([]byte, size-off)
	if _, err := f.ReadAt(sect, off); err != nil && err != io.EOF {
		return "", err
	}
	const open = "<index name=\"scan\">"
	if !strings.HasPrefix(string(sect), open) {
		res.problem(0, msdata.CodeMZXMLValidationFailed,
			"indexOffset %d does not point at %s (found %q)", off, open, first40(string(sect)))
		return "", nil
	}
	if end := strings.Index(string(sect), "</index>"); end > 0 {
		sect = sect[:end]
	}
	return string(sect), nil
}

// findTag locates the first occurrence of a literal tag by scanning the file in
// chunks, keeping enough overlap that a tag cannot straddle a boundary.
func findTag(f *os.File, size int64, tag string) (int64, error) {
	const chunk = 1 << 20
	buf := make([]byte, chunk+len(tag))
	var base int64
	for base < size {
		n, err := f.ReadAt(buf, base)
		if n <= 0 {
			if err != nil && err != io.EOF {
				return -1, err
			}
			return -1, nil
		}
		i := strings.Index(string(buf[:n]), tag)
		if i >= 0 {
			return base + int64(i), nil
		}
		if n < len(buf) {
			return -1, nil
		}
		base += chunk
	}
	return -1, nil
}

// ---------- comparison ----------

func compareSpectrum(src *andiio.Reader, pos int64, got *spectrumData, opts Options, res *Result) error {
	want, err := src.Next(context.Background())
	if errors.Is(err, msdata.ErrEndOfData) {
		res.problem(int(pos), msdata.CodeCountMismatch,
			"the document has more spectra than the source (source ended after %d)", pos)
		return res.errFromProblems()
	}
	if err != nil {
		return msdata.WrapError(msdata.CodeOf(err), err, "re-reading source spectrum %d", pos)
	}
	if len(want.MZ) != len(got.mz) {
		res.problem(int(pos), msdata.CodeNumericMismatch,
			"spectrum %d: document holds %d peaks, source has %d after the fill rule",
			pos, len(got.mz), len(want.MZ))
		return nil
	}
	tol := opts.Tolerance
	for i := range want.MZ {
		if !closeEnough(got.mz[i], want.MZ[i], tol) {
			res.problem(int(pos), msdata.CodeNumericMismatch,
				"spectrum %d point %d: m/z is %v, source says %v", pos, i, got.mz[i], want.MZ[i])
			return nil
		}
		if !closeEnough(got.intensity[i], want.Intensity[i], tol) {
			res.problem(int(pos), msdata.CodeNumericMismatch,
				"spectrum %d point %d: intensity is %v, source says %v", pos, i, got.intensity[i], want.Intensity[i])
			return nil
		}
	}
	if want.RetentionTimeSet {
		if !got.rtSet {
			res.problem(int(pos), msdata.CodeNumericMismatch,
				"spectrum %d: the source has a retention time but the document does not", pos)
		} else if !closeEnough(got.rt, want.RetentionTime, tol) {
			res.problem(int(pos), msdata.CodeNumericMismatch,
				"spectrum %d: retention time is %v s, source says %v s", pos, got.rt, want.RetentionTime)
		}
	} else if got.rtSet {
		res.problem(int(pos), msdata.CodeNumericMismatch,
			"spectrum %d: a retention time was written for a source value that is missing", pos)
	}
	if got.declaredLen >= 0 && got.declaredLen != len(want.MZ) {
		res.problem(int(pos), msdata.CodeNumericMismatch,
			"spectrum %d: declared array length %d, source has %d points", pos, got.declaredLen, len(want.MZ))
	}
	if want.MSLevel != nil && got.msLevel != 0 && *want.MSLevel != got.msLevel {
		res.problem(int(pos), msdata.CodeNumericMismatch,
			"spectrum %d: msLevel is %d, source-derived level is %d", pos, got.msLevel, *want.MSLevel)
	}
	if want.Centroid != msdata.CentroidUnknown && got.centroid != msdata.CentroidUnknown &&
		want.Centroid != got.centroid {
		res.problem(int(pos), msdata.CodeNumericMismatch,
			"spectrum %d: centroid state is %v, source-derived state is %v", pos, got.centroid, want.Centroid)
	}
	if want.Polarity != msdata.PolarityUnknown && got.polarity != msdata.PolarityUnknown &&
		want.Polarity != got.polarity {
		res.problem(int(pos), msdata.CodeNumericMismatch,
			"spectrum %d: polarity is %v, source-derived polarity is %v", pos, got.polarity, want.Polarity)
	}
	return nil
}

func closeEnough(a, b, tol float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	if a == b {
		return true
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// ---------- helpers ----------

func (r *Result) problem(scan int, code msdata.Code, format string, args ...any) {
	if len(r.Problems) < maxReported {
		r.Problems = append(r.Problems, Problem{ScanIndex: scan, Code: code, Message: fmt.Sprintf(format, args...)})
	} else if len(r.Problems) == maxReported {
		r.Problems = append(r.Problems, Problem{
			Code:    msdata.CodeDiagnosticsTruncated,
			Message: "further problems suppressed",
		})
	}
}

func (r *Result) fail(code msdata.Code, format string, args ...any) error {
	// A structural failure stops the pass: continuing would compare spectra
	// against the wrong source rows.
	return msdata.NewError(code, format, args...)
}

func (r *Result) errFromProblems() error {
	if len(r.Problems) > 0 {
		return msdata.WrapError(codeOfProblems(r.Problems), ErrProblems, "%s", r.Problems[0].Message)
	}
	return nil
}

func attrStr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func attrInt(e xml.StartElement, name string, def int64) int64 {
	v := attrStr(e, name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func attrXsiNil(e xml.StartElement) bool {
	for _, a := range e.Attr {
		if a.Name.Local == "nil" &&
			(a.Name.Space == "http://www.w3.org/2001/XMLSchema-instance" || a.Name.Space == "xsi") {
			return a.Value == "true" || a.Value == "1"
		}
	}
	return false
}
