package convert_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
)

// steppingClock advances by step on every reading, so any code path that times
// a file reports a non-zero duration without depending on real run time.
func steppingClock(step time.Duration) func() time.Time {
	var mu sync.Mutex
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(step)
		return t
	}
}

// TestFileElapsedIsReported pins the regression where every per-file record
// (report results, the JSONL journal's duration_ms) carried 0: the timing was
// written by a defer onto a local copy after the return value was taken.
func TestFileElapsedIsReported(t *testing.T) {
	defer convert.SetElapsedClock(steppingClock(250 * time.Millisecond))()
	dir := t.TempDir()
	src := writeCorpus(t, dir, 3, 5, "second", 1)
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML}, Overwrite: true, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(rep.Results))
	}
	if got := rep.Results[0].ElapsedMS; got < 250 {
		t.Errorf("elapsedMs = %d, want >= 250", got)
	}
}
