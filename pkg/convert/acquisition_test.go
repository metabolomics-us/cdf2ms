package convert_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

var startTimeStampAttr = regexp.MustCompile(`<run [^>]*startTimeStamp="([^"]*)"`)

// convertStamp converts one synthetic file whose experiment_date_time_stamp is
// stamp (omitted when empty) and returns the mzML run startTimeStamp, or "" if
// the attribute is absent.
func convertStamp(t *testing.T, stamp string) string {
	t.Helper()
	dir := t.TempDir()
	opt := testutil.ANDIOptions{ScanCount: 4, PointsPerScan: []int{3}, RTUnit: "Seconds"}
	if stamp != "" {
		opt.ExtraGlobals = map[string]string{"experiment_date_time_stamp": stamp}
	}
	src := writeRawANDI(t, dir, "run.cdf", opt)
	if _, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML}, Overwrite: true, Now: fixedClock(),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "run.mzML"))
	if err != nil {
		t.Fatal(err)
	}
	if m := startTimeStampAttr.FindSubmatch(b); m != nil {
		return string(m[1])
	}
	return ""
}

// TestStartTimeStampIsTheAcquisitionTime pins the regression where every mzML
// run@startTimeStamp carried the conversion time. Downstream acquisition
// tracking reads this attribute, so a 2017 run was recorded as acquired on the
// day it was converted.
func TestStartTimeStampIsTheAcquisitionTime(t *testing.T) {
	// The value and shape of a real LECO export: 2017-03-20 23:52:39 at UTC-8.
	if got, want := convertStamp(t, "20170320235239-0800"), "2017-03-21T07:52:39Z"; got != want {
		t.Errorf("startTimeStamp = %q, want %q (acquisition time in UTC)", got, want)
	}
}

// TestStartTimeStampOmittedWithoutAnInstant keeps the never-invent contract:
// with no stamp, or one without a UTC offset, the attribute is left out rather
// than filled with the conversion time or a guessed zone.
func TestStartTimeStampOmittedWithoutAnInstant(t *testing.T) {
	for name, stamp := range map[string]string{
		"absent":    "",
		"no_offset": "20170320235239",
		"garbage":   "last tuesday",
	} {
		t.Run(name, func(t *testing.T) {
			if got := convertStamp(t, stamp); got != "" {
				t.Errorf("startTimeStamp = %q, want it omitted", got)
			}
		})
	}
}
