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

var (
	runIDAttr   = regexp.MustCompile(`<run id="([^"]*)"`)
	docIDAttr   = regexp.MustCompile(`<mzML [^>]*\bid="([^"]*)"`)
	asciiNCName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)
)

// TestRunIDIsAnXMLIDForDateNamedSamples pins the regression where run@id was
// the raw sample name. Lab names start with the acquisition date, an xs:ID
// cannot start with a digit, and every production mzML failed the official
// schema on this attribute.
func TestRunIDIsAnXMLIDForDateNamedSamples(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "130824ceasa17_1.cdf")
	if err := testutil.WriteANDI(src, testutil.ANDIOptions{ScanCount: 3, PointsPerScan: []int{4}, RTUnit: "second"}); err != nil {
		t.Fatal(err)
	}
	if _, err := convert.Run(context.Background(), []string{src}, convert.Options{
		Formats: []convert.Format{convert.FormatMzML}, Overwrite: true, Now: fixedClock(),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "130824ceasa17_1.mzML"))
	if err != nil {
		t.Fatal(err)
	}
	run := runIDAttr.FindSubmatch(b)
	if run == nil || !asciiNCName.Match(run[1]) || string(run[1]) != "_130824ceasa17_1" {
		t.Errorf("run@id = %q, want the valid xs:ID %q", run, "_130824ceasa17_1")
	}
	if doc := docIDAttr.FindSubmatch(b); doc == nil || string(doc[1]) != "130824ceasa17_1" {
		t.Errorf("mzML@id = %q, want the unmodified sample name", doc)
	}
}
