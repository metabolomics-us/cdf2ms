package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// runCLI invokes the command dispatcher with stdout and stderr captured, so the
// integration tests exercise the real flag parsing and report rendering without
// forking a subprocess.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	code = run(context.Background(), args)
	wOut.Close()
	wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	outB, _ := io.ReadAll(rOut)
	errB, _ := io.ReadAll(rErr)
	rOut.Close()
	rErr.Close()
	return code, string(outB), string(errB)
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := runCLI(t, "version")
	if code != 0 {
		t.Fatalf("version exit=%d", code)
	}
	if !strings.Contains(out, "cdf2ms") || !strings.Contains(out, "mzML 1.1.0") {
		t.Errorf("version output missing facts:\n%s", out)
	}
	code, out, _ = runCLI(t, "version", "-json")
	if code != 0 {
		t.Fatalf("version -json exit=%d", code)
	}
	var vi versionInfo
	if err := json.Unmarshal([]byte(out), &vi); err != nil {
		t.Fatalf("version -json not parseable: %v\n%s", err, out)
	}
	if vi.MzML != "1.1.0" || vi.MzXML != "3.2" || !vi.PureGo {
		t.Errorf("version info wrong: %+v", vi)
	}
}

func TestFixturesThenInspectThenAudit(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := runCLI(t, "fixtures", "-variant", "plain,unitless-ambiguous", dir)
	if code != 0 {
		t.Fatalf("fixtures exit=%d stderr=%s", code, errOut)
	}
	// The ambiguous fixture must not be silently convertible.
	code, out, _ := runCLI(t, "audit", filepath.Join(dir, "plain-01.cdf"))
	if code != 0 {
		t.Fatalf("audit plain exit=%d", code)
	}
	if !strings.Contains(out, "second") {
		t.Errorf("audit plain should report seconds:\n%s", out)
	}
	code, _, _ = runCLI(t, "audit", filepath.Join(dir, "unitless-ambiguous-01.cdf"))
	if code != 2 {
		t.Fatalf("audit ambiguous should exit 2 (not convertible), got %d", code)
	}
	code, out, _ = runCLI(t, "inspect", filepath.Join(dir, "plain-01.cdf"))
	if code != 0 {
		t.Fatalf("inspect exit=%d", code)
	}
	if !strings.Contains(out, "NetCDF") && !strings.Contains(out, "CDF") {
		t.Errorf("inspect should describe the container:\n%s", out)
	}
}

func TestConvertVerifyAndReportSummary(t *testing.T) {
	dir := t.TempDir()
	code, _, _ := runCLI(t, "fixtures", "-variant", "plain", dir)
	if code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	report := filepath.Join(dir, "run.jsonl")
	code, _, _ = runCLI(t, "convert", "-overwrite", "-verify",
		"-report-jsonl", report, filepath.Join(dir, "plain-01.cdf"))
	if code != 0 {
		t.Fatalf("convert exit=%d", code)
	}
	if _, err := os.Stat(report); err != nil {
		t.Fatalf("jsonl report missing: %v", err)
	}
	code, out, _ := runCLI(t, "report", "summarize", report)
	if code != 0 {
		t.Fatalf("report summarize exit=%d", code)
	}
	if !strings.Contains(out, "success:    1") {
		t.Errorf("summary should show one success:\n%s", out)
	}
	// Both output files should exist and verify.
	for _, f := range []string{"plain-01.mzML", "plain-01.mzXML"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing output %s: %v", f, err)
		}
	}
}

func TestConvertCollisionExitAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	code, _, _ := runCLI(t, "fixtures", "-variant", "plain", dir)
	if code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	src := filepath.Join(dir, "plain-01.cdf")
	if code, _, _ := runCLI(t, "convert", src); code != 0 {
		t.Fatalf("first convert exit=%d", code)
	}
	// Second convert without -overwrite collides and skips (reported, not fatal).
	if code, out, _ := runCLI(t, "convert", src); code != 0 {
		t.Fatalf("collision convert exit=%d (want 0: a skip, not a failure)", code)
	} else if !strings.Contains(out, "COLLISION") && !strings.Contains(out, "exists") {
		t.Errorf("collision not reported:\n%s", out)
	}
	// With -overwrite it succeeds.
	if code, _, _ := runCLI(t, "convert", "-overwrite", src); code != 0 {
		t.Fatalf("overwrite convert exit=%d", code)
	}
}

func TestConvertAmbiguousNeedsRTUnitFlag(t *testing.T) {
	dir := t.TempDir()
	code, _, _ := runCLI(t, "fixtures", "-variant", "unitless-ambiguous", dir)
	if code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	src := filepath.Join(dir, "unitless-ambiguous-01.cdf")
	if code, _, _ := runCLI(t, "convert", src); code != 2 {
		t.Fatalf("ambiguous convert without flag should exit 2, got %d", code)
	}
	if code, _, _ := runCLI(t, "convert", "-rt-unit", "seconds", src); code != 0 {
		t.Fatalf("ambiguous convert with -rt-unit seconds should exit 0, got %d", code)
	}
}

// TestConvertGlobalUnitsNeedsNoFlag is the regression for a reported file that
// would not load: scan_acquisition_time states no unit, the scan spacing fits
// both clocks, and only the ASTM general-data `units` global says seconds. It
// must convert under the default policy, with the provenance recorded.
func TestConvertGlobalUnitsNeedsNoFlag(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI(t, "fixtures", "-variant", "global-units", dir); code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	src := filepath.Join(dir, "global-units-01.cdf")
	report := filepath.Join(dir, "run.jsonl")
	if code, out, errOut := runCLI(t, "convert", "-report-jsonl", report, src); code != 0 {
		t.Fatalf("global-units convert should exit 0 with no -rt-unit, got %d\n%s\n%s", code, out, errOut)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ANDI_USED_GLOBAL_TIME_UNIT") {
		t.Errorf("report should record where the unit came from:\n%s", body)
	}
	if strings.Contains(string(body), "ANDI_AMBIGUOUS_UNITS") {
		t.Errorf("a file that states its unit anywhere is not ambiguous:\n%s", body)
	}
}

func TestUnknownCommandAndUsage(t *testing.T) {
	if code, _, _ := runCLI(t, "frobnicate"); code != 1 {
		t.Fatalf("unknown command should exit 1, got %d", code)
	}
	if code, _, _ := runCLI(t, "convert"); code != 1 {
		t.Fatalf("convert with no args should exit 1, got %d", code)
	}
	if code, _, _ := runCLI(t, "help"); code != 0 {
		t.Fatalf("help should exit 0, got %d", code)
	}
}

func TestValidateCommand(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI(t, "fixtures", "-variant", "plain", dir); code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	src := filepath.Join(dir, "plain-01.cdf")
	if code, _, _ := runCLI(t, "convert", "-overwrite", src); code != 0 {
		t.Fatalf("convert exit=%d", code)
	}
	if code, out, _ := runCLI(t, "validate", src, filepath.Join(dir, "plain-01.mzML")); code != 0 {
		t.Fatalf("validate mzML exit=%d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "validate", src, filepath.Join(dir, "plain-01.mzXML")); code != 0 {
		t.Fatalf("validate mzXML exit=%d\n%s", code, out)
	}
	// A tampered output must fail validation.
	if err := os.WriteFile(filepath.Join(dir, "plain-01.mzML"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "validate", src, filepath.Join(dir, "plain-01.mzML")); code == 0 {
		t.Fatalf("validate should fail on a tampered output")
	}
}

func TestVerifyCorpusCommand(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI(t, "fixtures", "-variant", "plain,minutes", dir); code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	scratch := filepath.Join(dir, "scratch")
	if code, _, _ := runCLI(t, "verify-corpus", "-temp-dir", scratch, dir); code != 0 {
		t.Fatalf("verify-corpus exit=%d", code)
	}
	// With -keep (default) outputs remain.
	if n := countFiles(scratch); n != 4 { // 2 sources x 2 formats
		t.Fatalf("verify-corpus keep produced %d outputs, want 4", n)
	}
	if code, _, _ := runCLI(t, "verify-corpus", "-temp-dir", scratch, "-delete", dir); code != 0 {
		t.Fatalf("verify-corpus -delete exit=%d", code)
	}
	if n := countFiles(scratch); n != 0 {
		t.Fatalf("verify-corpus -delete left %d outputs, want 0", n)
	}
}

func TestConvertFileList(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI(t, "fixtures", "-variant", "plain,minutes", dir); code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	list := filepath.Join(dir, "list.txt")
	os.WriteFile(list, []byte("# corpus shard\n"+filepath.Join(dir, "plain-01.cdf")+"\n"), 0o644)
	if code, _, _ := runCLI(t, "convert", "-file-list", list, "-out-dir", filepath.Join(dir, "out"), "-quiet"); code != 0 {
		t.Fatalf("file-list convert exit=%d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "plain-01.mzML")); err != nil {
		t.Errorf("file-list output missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "minutes-01.mzML")); err == nil {
		t.Errorf("file-list should only convert listed file")
	}
}

func TestConvertShardingIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := runCLI(t, "fixtures", "-variant", "plain,cdf2,cdf5,packed,minutes", dir); code != 0 {
		t.Fatalf("fixtures exit=%d", code)
	}
	shard := func(index int) []string {
		out := filepath.Join(dir, "sh", "s"+string(rune('0'+index)))
		if code, _, _ := runCLI(t, "convert", "-shard-index", strconv.Itoa(index), "-shard-count", "3",
			"-out-dir", out, "-overwrite", "-quiet", dir); code != 0 {
			t.Fatalf("shard %d convert exit=%d", index, code)
		}
		var names []string
		filepath.WalkDir(out, func(p string, d os.DirEntry, e error) error {
			if e != nil {
				return nil
			}
			if !d.IsDir() {
				names = append(names, filepath.Base(p))
			}
			return nil
		})
		return names
	}
	a := shard(1)
	b := shard(1)
	if len(a) != len(b) {
		t.Fatalf("shard sizes differ: %v vs %v", a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sharding not deterministic: %v vs %v", a, b)
		}
	}
	// The union of all shards covers every source basename.
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		for _, n := range shard(i) {
			seen[strings.TrimSuffix(n, filepath.Ext(n))] = true
		}
	}
	for _, want := range []string{"plain-01", "cdf2-01", "cdf5-01", "packed-01", "minutes-01"} {
		if !seen[want] {
			t.Errorf("shards do not cover %s", want)
		}
	}
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, e error) error {
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
