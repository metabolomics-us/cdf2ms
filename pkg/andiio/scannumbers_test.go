package andiio

import (
	"context"
	"errors"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// scanNumbers reads every spectrum and returns its scan number and origin.
func scanNumbers(t *testing.T, r *Reader) ([]int64, []string) {
	t.Helper()
	var nums []int64
	var origins []string
	for {
		sp, err := r.Next(context.Background())
		if errors.Is(err, msdata.ErrEndOfData) {
			return nums, origins
		}
		if err != nil {
			t.Fatal(err)
		}
		if sp.ScanNumber == nil {
			t.Fatalf("spectrum %d has no scan number", sp.Index)
		}
		nums = append(nums, *sp.ScanNumber)
		origins = append(origins, sp.ScanNumberOrigin)
	}
}

func hasWarning(r *Reader, code msdata.Code) bool {
	for _, w := range r.Diagnostics().Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestReaderScanNumberSentinel pins the regression seen on LECO GC-TOF exports:
// actual_scan_number filled with -9999 and no _FillValue reached every mzML
// nativeID as "scan=-9999". Negative or fill values anywhere make the whole
// file fall back to ordinals, so numbers never mix and never repeat.
func TestReaderScanNumberSentinel(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		numbers []int64
	}{
		{"all_sentinel", []int64{-9999, -9999, -9999, -9999, -9999}},
		// A lone bad value late in the file still disqualifies the variable:
		// keeping 1, 2, 3 and substituting the ordinal 5 for the gap would mix
		// schemes inside one file.
		{"one_sentinel", []int64{1, 2, 3, -9999, 5}},
		{"netcdf_int_fill", []int64{10, 11, -2147483647, 13, 14}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opt := testutil.ANDIOptions{ScanCount: 5, PointsPerScan: []int{3}, RTUnit: "Seconds", ScanNumbers: tc.numbers}
			r := openFixture(t, dir, tc.name, opt, nil)
			defer r.Close()
			nums, origins := scanNumbers(t, r)
			for i, n := range nums {
				if n != int64(i+1) || origins[i] != "derived:ordinal" {
					t.Fatalf("spectrum %d: scan %d (%s), want ordinal %d", i, n, origins[i], i+1)
				}
			}
			if !hasWarning(r, msdata.CodeANDIScanNumbersUnusable) {
				t.Error("no ANDI_SCAN_NUMBERS_UNUSABLE warning")
			}
		})
	}
}

// TestReaderScanNumbersKeptWhenValid keeps real numbering, including a
// zero-based source, which is a genuine vendor convention rather than a fill.
func TestReaderScanNumbersKeptWhenValid(t *testing.T) {
	dir := t.TempDir()
	numbers := []int64{0, 1, 2, 3, 4}
	opt := testutil.ANDIOptions{ScanCount: 5, PointsPerScan: []int{3}, RTUnit: "Seconds", ScanNumbers: numbers}
	r := openFixture(t, dir, "zero_based", opt, nil)
	defer r.Close()
	nums, origins := scanNumbers(t, r)
	for i, n := range nums {
		if n != numbers[i] || origins[i] != "var:actual_scan_number" {
			t.Fatalf("spectrum %d: scan %d (%s), want %d from the source", i, n, origins[i], numbers[i])
		}
	}
	if hasWarning(r, msdata.CodeANDIScanNumbersUnusable) {
		t.Error("valid numbering was flagged unusable")
	}
}
