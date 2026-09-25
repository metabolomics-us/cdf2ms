package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/metabolomics-us/cdf2ms/pkg/andiio"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// TestAuditElapsedIsReported pins the same named-result regression in audit:
// elapsedMs was always 0 in audit reports.
func TestAuditElapsedIsReported(t *testing.T) {
	prev := auditClock
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	auditClock = func() time.Time { now = now.Add(250 * time.Millisecond); return now }
	defer func() { auditClock = prev }()

	path := filepath.Join(t.TempDir(), "run.cdf")
	if err := testutil.WriteANDI(path, testutil.ANDIOptions{ScanCount: 3, PointsPerScan: []int{4}, RTUnit: "second"}); err != nil {
		t.Fatal(err)
	}
	rep := auditOne(context.Background(), path, andiio.RTPolicyAuto, false)
	if rep.ElapsedMS < 250 {
		t.Errorf("elapsedMs = %d, want >= 250 (verdict %s)", rep.ElapsedMS, rep.Verdict)
	}
}
