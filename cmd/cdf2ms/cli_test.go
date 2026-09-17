package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
