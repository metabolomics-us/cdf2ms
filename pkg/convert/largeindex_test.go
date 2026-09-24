package convert_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// TestVerifyFindsAnMzXMLIndexLargerThanOneMegabyte pins the regression where
// verification read only the last 1 MB of an mzXML file to find its scan index.
// The index grows about 40 bytes per scan; four production GC-TOF runs of
// 56,660 scans (2.3 MB index) were written correctly and still failed with "no
// scan index was found". 45,000 scans put the index start past 1 MB here.
func TestVerifyFindsAnMzXMLIndexLargerThanOneMegabyte(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 45,000-scan document")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "long.cdf")
	if err := testutil.WriteANDI(src, testutil.ANDIOptions{
		ScanCount: 45000, PointsPerScan: []int{1}, RTUnit: "second",
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzXML}, Overwrite: true,
		ReopenAndVerify: true, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := rep.Results[0]
	if r.Status != convert.StatusConverted {
		t.Fatalf("status = %s (%s: %s), want converted", r.Status, r.Code, r.Detail)
	}
	b, err := os.ReadFile(filepath.Join(dir, "long.mzXML"))
	if err != nil {
		t.Fatal(err)
	}
	if fromEnd := len(b) - bytes.LastIndex(b, []byte("<index name=")); fromEnd <= 1<<20 {
		t.Fatalf("index starts %d bytes from the end; the fixture no longer exercises a >1 MB index", fromEnd)
	}
}
