package convert_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metabolomics-us/cdf2ms/pkg/andiio"
	"github.com/metabolomics-us/cdf2ms/pkg/convert"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// writeRawANDI writes a synthetic file with no defaults applied, so tests can
// reproduce sources that omit the units attribute entirely.
func writeRawANDI(t *testing.T, dir, name string, opt testutil.ANDIOptions) string {
	t.Helper()
	path := filepath.Join(dir, name)
	spec, err := testutil.ANDI(opt)
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	if err := testutil.WriteNetCDF(path, spec); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func writeCorpus(t *testing.T, dir string, spectra, points int, rtUnit string, rtValue float64) string {
	t.Helper()
	path := filepath.Join(dir, "run01.cdf")
	if err := testutil.WriteANDI(path, testutil.ANDIOptions{
		ScanCount: spectra, PointsPerScan: []int{points}, RTUnit: rtUnit, RTValueScale: rtValue,
	}); err != nil {
		t.Fatalf("write corpus: %v", err)
	}
	return path
}

func TestConvertBothFormatsInOnePass(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 40, 60, "second", 1)
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		OutDir:    out,
		Overwrite: true,
		Workers:   2,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Converted != 1 || rep.Failed != 0 || rep.Skipped != 0 {
		t.Fatalf("report: %+v\n%s", rep, rep.Text())
	}
	if len(rep.Results[0].Outputs) != 2 {
		t.Fatalf("want 2 outputs, got %+v", rep.Results[0].Outputs)
	}
	if rep.Spectra != 40 {
		t.Errorf("spectra = %d, want 40", rep.Spectra)
	}
	if rep.Results[0].RTUnit != "second" {
		t.Errorf("rt unit = %q, want second", rep.Results[0].RTUnit)
	}
	for _, o := range rep.Results[0].Outputs {
		p := filepath.Join(out, filepath.Base(o.Path))
		st, err := os.Stat(p)
		if err != nil {
			// Summary carries the path actually written; accept either spelling.
			if _, err2 := os.Stat(o.Path); err2 != nil {
				t.Fatalf("missing output %s: %v / %v", o.Path, err, err2)
			}
			p = o.Path
		}
		if st.Size() == 0 {
			t.Errorf("empty output %s", p)
		}
	}
	// The mzXML report line must carry the format it wrote.
	if !strings.Contains(rep.Text(), ".mzXML") || !strings.Contains(rep.Text(), ".mzML") {
		t.Errorf("text report missing output names:\n%s", rep.Text())
	}
}

func TestVerifyRoundTripThroughConvert(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 25, 80, "second", 1)
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:         []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		Overwrite:       true,
		ReopenAndVerify: true,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Converted != 1 {
		t.Fatalf("verification failed: %+v", rep.Results[0])
	}
	verified := 0
	for _, w := range rep.Results[0].Warnings {
		if w.Code == msdata.CodeVerified {
			verified++
		}
	}
	if verified != 2 {
		t.Errorf("verified warnings = %d, want 2: %+v", verified, rep.Results[0].Warnings)
	}
}

func TestCollisionIsSkippedUnlessOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 5, 10, "second", 1)
	opts := convert.Options{Formats: []convert.Format{convert.FormatMzML}, Overwrite: true}
	if _, err := convert.Run(context.Background(), []string{src}, opts); err != nil {
		t.Fatal(err)
	}
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Results[0].Code != msdata.CodeCollision {
		t.Fatalf("collision not reported: %+v", rep.Results)
	}
	if !strings.Contains(rep.Results[0].Detail, "--overwrite") {
		t.Errorf("collision detail should explain the flag: %q", rep.Results[0].Detail)
	}
	rep, err = convert.Run(context.Background(), []string{src}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converted != 1 {
		t.Fatalf("overwrite should convert: %+v", rep.Results)
	}
}

func TestDiscoveryAndMissingPaths(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested", "deeper")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	a := writeCorpus(t, dir, 3, 5, "second", 1)
	b := filepath.Join(sub, "run02.CDF")
	if err := testutil.WriteANDI(b, testutil.ANDIOptions{ScanCount: 3, PointsPerScan: []int{5}}); err != nil {
		t.Fatal(err)
	}

	files, skipped, err := convert.Discover([]string{dir}, convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != a {
		t.Fatalf("non-recursive discovery = %v (want only the top-level file)", files)
	}
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	files, _, err = convert.Discover([]string{dir}, convert.Options{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("recursive discovery = %v, want both files", files)
	}
	// Case-insensitive suffix matching, because vendor corpora shout.
	if _, ok := contains(files, b); !ok {
		t.Errorf("recursive discovery missed %v", files)
	}
	_, skipped, err = convert.Discover([]string{filepath.Join(dir, "nope.cdf")}, convert.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Code != msdata.CodeFileNotFound {
		t.Fatalf("missing path not reported: %+v", skipped)
	}
}

func contains(hay []string, needle string) (int, bool) {
	for i, h := range hay {
		if h == needle {
			return i, true
		}
	}
	return -1, false
}

func TestUnconvertibleSourceDoesNotStopTheRun(t *testing.T) {
	dir := t.TempDir()
	good := writeCorpus(t, dir, 6, 8, "second", 1)
	bad := filepath.Join(dir, "broken.cdf")
	if err := os.WriteFile(bad, []byte("not a netcdf file at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := convert.Run(context.Background(), []string{bad, good}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML},
		Overwrite: true,
		Workers:   1,
	})
	if err != nil {
		t.Fatalf("run must not fail because of one bad file: %v", err)
	}
	if rep.Converted != 1 || rep.Failed != 2-1 {
		t.Fatalf("report: converted=%d failed=%d\n%s", rep.Converted, rep.Failed, rep.Text())
	}
	if rep.Results[0].Code == "" {
		t.Errorf("failure needs a stable code: %+v", rep.Results[0])
	}
}

func TestAmbiguousRTUnitRefusesWithoutPolicy(t *testing.T) {
	dir := t.TempDir()
	// No units attribute anywhere, and a scan spacing that is equally plausible as
	// seconds or minutes: the reader must refuse to guess rather than pick one.
	src := writeRawANDI(t, dir, "run01.cdf", testutil.ANDIOptions{
		ScanCount: 10, PointsPerScan: []int{10}, RTUnit: "", RTValueScale: 0.2,
	})
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML},
		Overwrite: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 || rep.Results[0].Code != msdata.CodeANDIAmbiguousUnits {
		t.Fatalf("ambiguous units must be refused: %+v", rep.Results)
	}
	if !strings.Contains(rep.Results[0].Detail, "--rt-unit") {
		t.Errorf("refusal must name the flag: %q", rep.Results[0].Detail)
	}
	if _, err := os.Stat(filepath.Join(dir, "run01.mzML")); err == nil {
		t.Errorf("a refused conversion must not leave an output behind")
	}
	// The same file converts once the operator states the unit.
	rep, err = convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML},
		Overwrite: true,
		RTUnit:    "minutes",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converted != 1 {
		t.Fatalf("explicit unit should convert: %+v", rep.Results)
	}
	if rep.Results[0].RTUnit != "second" {
		t.Errorf("rt unit in the report = %q, want the normalised second", rep.Results[0].RTUnit)
	}
	if !strings.Contains(rep.Results[0].RTUnitOrigin, "operator") {
		t.Errorf("rt unit origin = %q, want the user-override provenance", rep.Results[0].RTUnitOrigin)
	}
}

func TestCancellationIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 2000, 400, "second", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := convert.Run(ctx, []string{src}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML},
		Overwrite: true,
	})
	if err != nil {
		t.Fatalf("cancelled run should return a report, not an error: %v", err)
	}
	found := false
	for _, r := range rep.Results {
		if r.Code == msdata.CodeCancelled || r.Status == convert.StatusFailed {
			found = true
		}
	}
	if !found {
		t.Errorf("cancellation must be visible in the report: %s", rep.Text())
	}
	// No finished output may exist for a cancelled conversion.
	if st, err := os.Stat(filepath.Join(dir, "run01.mzML")); err == nil && st.Size() > 0 {
		got, _ := os.ReadFile(filepath.Join(dir, "run01.mzML"))
		if strings.Contains(string(got[len(got)-200:]), "</mzML>") {
			t.Errorf("a cancelled run left a completed document")
		}
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 4, 6, "second", 1)
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzXML}, Overwrite: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := rep.JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	var back convert.Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	if back.Converted != rep.Converted || back.Spectra != rep.Spectra || len(back.Results) != len(rep.Results) {
		t.Errorf("json round trip lost data: %+v vs %+v", back, rep)
	}
	if !strings.Contains(string(b), "\"mzXML\"") {
		t.Errorf("report should name the format: %s", b)
	}
}

func TestPrecisionAndCompressionOptions(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 8, 64, "second", 1)
	// The synthetic values are float32-native, so "auto" already selects 32-bit
	// arrays; 64-bit must therefore be larger, and compression must be recorded
	// honestly whether or not the payload actually shrinks.
	cases := []struct {
		name     string
		prec     string
		compress bool
	}{
		{name: "auto", prec: "auto"},
		{name: "f64", prec: "f64"},
		{name: "f32", prec: "f32"},
		{name: "f32+zlib", prec: "f32", compress: true},
	}
	sizes := map[string]int64{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
				Formats: []convert.Format{convert.FormatMzML, convert.FormatMzXML},
				OutDir:  out, Overwrite: true, Precision: tc.prec, Compress: tc.compress,
				ReopenAndVerify: true, Now: fixedClock(),
			})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Converted != 1 {
				t.Fatalf("report: %+v", rep.Results)
			}
			total := int64(0)
			for _, o := range rep.Results[0].Outputs {
				total += o.Bytes
				if o.Compressed != tc.compress {
					t.Errorf("%s: summary Compressed = %v, want %v", o.Format, o.Compressed, tc.compress)
				}
			}
			sizes[tc.name] = total
		})
	}
	if sizes["f64"] <= sizes["auto"] {
		t.Errorf("64-bit arrays must be larger than the 32-bit auto choice: %v", sizes)
	}
	if sizes["auto"] != sizes["f32"] {
		t.Errorf("auto picked something other than float32 for float32-native data: %v", sizes)
	}
	// zlib on noise-like float data adds framing rather than shrinking it, so the
	// honest expectations are: never wildly bigger, smaller than 64-bit arrays, and
	// the summary flag set (the verifier already proves compressedLen and that
	// decompression yields bit-identical values).
	if sizes["f32+zlib"] > sizes["f32"]*11/10 {
		t.Errorf("zlib output should stay within 10%% of the raw size: %v", sizes)
	}
	if sizes["f32+zlib"] >= sizes["f64"] {
		t.Errorf("compressed 32-bit must beat uncompressed 64-bit: %v", sizes)
	}
	if len(sizes) != len(cases) {
		t.Fatalf("missing cases: %v", sizes)
	}
}

func TestAllEmptyScansStillProduceAValidDocument(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "emptypeaks.cdf")
	if err := testutil.WriteANDI(path, testutil.ANDIOptions{
		ScanCount: 3, PointsPerScan: []int{0}, ZeroScans: true,
	}); err != nil {
		t.Fatal(err)
	}
	src := path
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		Overwrite: true, ReopenAndVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Converted != 1 {
		t.Fatalf("scans with zero points are real scans and must convert: %+v", rep.Results)
	}
	if rep.Results[0].Spectra != 3 || rep.Results[0].Points != 0 {
		t.Errorf("spectra/points = %d/%d, want 3/0", rep.Results[0].Spectra, rep.Results[0].Points)
	}
	zeroPointReported := false
	for _, w := range rep.Results[0].Warnings {
		if w.Code == msdata.CodeANDIZeroPointScan {
			zeroPointReported = true
		}
	}
	if !zeroPointReported {
		t.Errorf("empty scans must be reported: %+v", rep.Results[0].Warnings)
	}
}

func TestSoftwareVersionIsRecorded(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 3, 5, "second", 1)
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML}, Overwrite: true, SoftwareVersion: "9.9.9-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tool != "9.9.9-test" {
		t.Errorf("tool = %q", rep.Tool)
	}
	b, err := os.ReadFile(filepath.Join(dir, "run01.mzML"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "9.9.9-test") {
		t.Errorf("document should record the software version")
	}
}

func TestDeterministicOrderingWithOneWorkerAndManyFiles(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, string(rune('a'+i))+".cdf")
		if err := testutil.WriteANDI(p, testutil.ANDIOptions{ScanCount: 3, PointsPerScan: []int{4}}); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	var last convert.Options
	last = convert.Options{Formats: []convert.Format{convert.FormatMzXML}, OutDir: out,
		Overwrite: true, Workers: 4, Now: func() time.Time { return time.Unix(0, 0).UTC() }}
	first, err := convert.Run(context.Background(), paths, last)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first.Results {
		if first.Results[i].Source != paths[i] {
			t.Fatalf("results must follow discovery order: %v", orderOf(first.Results))
		}
	}
	if first.StartedAt.IsZero() {
		t.Errorf("Now must be used for reproducible timestamps")
	}
}

func orderOf(rs []convert.FileResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = filepath.Base(r.Source)
	}
	return out
}

func TestResumeSkipsMatchingOutputs(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 6, 12, "second", 1)
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o755)

	opts := convert.Options{
		Formats:    []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		OutDir:     out,
		Overwrite:  true,
		HashSource: boolPtr(true),
		HashOutput: true,
	}
	first, err := convert.Run(context.Background(), []string{src}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Converted != 1 {
		t.Fatalf("first run: %+v", first.Results[0])
	}
	// Record the first run as a JSONL report.
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	for _, r := range first.Results {
		if err := enc.Encode(fileResultToReportRecord(r)); err != nil {
			t.Fatal(err)
		}
	}
	report := filepath.Join(dir, "run.jsonl")
	if err := os.WriteFile(report, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:    []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		OutDir:     out,
		Overwrite:  true,
		ResumeFrom: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped != 1 || second.Results[0].Code != msdata.CodeSkipResumed {
		t.Fatalf("resume did not skip: %+v", second.Results)
	}
	// Tamper with one output; resume must now refuse to trust it.
	target := first.Results[0].Outputs[0].Path
	os.WriteFile(target, []byte("garbage"), 0o644)
	third, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:    []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		OutDir:     out,
		Overwrite:  true,
		ResumeFrom: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.Converted != 1 {
		t.Fatalf("tampered output should be re-converted, got: %+v", third.Results)
	}
}

func TestFailFastStopsFeedingButReportsInflight(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 4, 8, "second", 1)
	bad := filepath.Join(dir, "bad.txt")
	os.WriteFile(bad, []byte("not a netcdf file at all"), 0o644)

	opts := convert.Options{
		Formats:   []convert.Format{convert.FormatMzML},
		Overwrite: true,
		FailFast:  true,
	}
	rep, err := convert.Run(context.Background(), []string{src, bad}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 1 {
		t.Fatalf("want 1 failure, got %+v", rep)
	}
	// At least one file must be present in the report as converted or skipped.
	found := map[convert.Status]int{}
	for _, r := range rep.Results {
		found[r.Status]++
	}
	if found[convert.StatusConverted] == 0 && found[convert.StatusSkipped] == 0 {
		t.Fatalf("no file was processed at all under fail-fast: %+v", rep.Results)
	}
}

func TestHashOutputPopulatesDigest(t *testing.T) {
	dir := t.TempDir()
	src := writeCorpus(t, dir, 5, 10, "second", 1)
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats:    []convert.Format{convert.FormatMzML, convert.FormatMzXML},
		Overwrite:  true,
		HashOutput: true,
		HashSource: boolPtr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Results[0]
	if r.SourceSHA256 == "" || r.SourceSHA1 == "" {
		t.Errorf("source digests not populated: %+v", r)
	}
	for _, o := range r.Outputs {
		if len(o.SHA256) != 64 {
			t.Errorf("output %s digest = %q, want 64 hex chars", o.Path, o.SHA256)
		}
		// Independent check: hash the file on disk.
		sum, err := andiio.SourceChecksum(o.Path)
		if err != nil {
			t.Fatal(err)
		}
		if sum != o.SHA256 {
			t.Errorf("output digest mismatch: summary=%s disk=%s", o.SHA256, sum)
		}
	}
}

func TestOnResultReceivesEveryFinishedFile(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "b"), 0o755)
	src1 := writeCorpus(t, filepath.Join(dir, "a"), 3, 6, "second", 1)
	src2 := writeCorpus(t, filepath.Join(dir, "b"), 4, 8, "second", 1)
	var mu sync.Mutex
	seen := map[string]convert.Status{}
	rep, err := convert.Run(context.Background(), []string{src1, src2}, convert.Options{
		Formats:   []convert.Format{convert.FormatMzML},
		Overwrite: true,
		OnResult: func(r convert.FileResult) {
			mu.Lock()
			seen[r.Source] = r.Status
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("OnResult saw %d files, want 2: %v", len(seen), seen)
	}
	for _, r := range rep.Results {
		if seen[r.Source] != convert.StatusConverted {
			t.Errorf("source %s OnResult status = %s", r.Source, seen[r.Source])
		}
	}
}

func boolPtr(b bool) *bool { return &b }

// fileResultToReportRecord renders a FileResult into the JSONL shape the resume
// loader expects (status success/warning/failed/skipped, outputs with path/bytes/sha256).
func fileResultToReportRecord(r convert.FileResult) map[string]any {
	status := "skipped"
	switch r.Status {
	case convert.StatusConverted:
		status = "success"
	case convert.StatusFailed:
		status = "failed"
	}
	outs := make([]map[string]any, 0, len(r.Outputs))
	for _, o := range r.Outputs {
		outs = append(outs, map[string]any{
			"path":   o.Path,
			"bytes":  o.Bytes,
			"sha256": o.SHA256,
		})
	}
	return map[string]any{
		"source":        r.Source,
		"source_sha256": r.SourceSHA256,
		"source_size":   r.SourceSize,
		"status":        status,
		"outputs":       outs,
	}
}

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
}
