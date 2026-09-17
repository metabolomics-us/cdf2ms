package convert_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/andiio"
	"github.com/metabolomics-us/cdf2ms/pkg/convert"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// benchCorpus writes a synthetic ANDI file with the given scan/point geometry
// and returns its path.
func benchCorpus(b *testing.B, scans, points int) string {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.cdf")
	if err := testutil.WriteANDI(path, testutil.ANDIOptions{
		ScanCount: scans, PointsPerScan: []int{points}, RTUnit: "second", RTValueScale: 1,
	}); err != nil {
		b.Fatal(err)
	}
	return path
}

// BenchmarkConvertBothFormats measures the end-to-end cost of reading one source
// once and streaming it into both mzML and mzXML, without verification. This is
// the steady-state operation a corpus run performs per file.
func BenchmarkConvertBothFormats(b *testing.B) {
	src := benchCorpus(b, 200, 500) // 100k peaks
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := b.TempDir()
		rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
			Formats:   []convert.Format{convert.FormatMzML, convert.FormatMzXML},
			OutDir:    out,
			Overwrite: true,
		})
		if err != nil || rep.Converted != 1 {
			b.Fatalf("run: %v %+v", err, rep)
		}
	}
}

// BenchmarkConvertBothFormatsVerify adds the independent read-back pass that
// --verify performs on every produced document.
func BenchmarkConvertBothFormatsVerify(b *testing.B) {
	src := benchCorpus(b, 100, 400) // 40k peaks; verification dominates
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := b.TempDir()
		rep, err := convert.Run(context.Background(), []string{src}, convert.Options{
			Formats:         []convert.Format{convert.FormatMzML, convert.FormatMzXML},
			OutDir:          out,
			Overwrite:       true,
			ReopenAndVerify: true,
		})
		if err != nil || rep.Converted != 1 {
			b.Fatalf("run: %v %+v", err, rep)
		}
	}
}

// BenchmarkReadOnly measures the ANDI reader in isolation, which bounds the
// per-file cost of inspect/audit and of a corpus scan that reads without writing.
func BenchmarkReadOnly(b *testing.B) {
	src := benchCorpus(b, 500, 2000) // 1M peaks
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := readAll(src); err != nil {
			b.Fatal(err)
		}
	}
}

func readAll(src string) error {
	ctx := context.Background()
	rd, err := andiio.Open(src, andiio.Options{})
	if err != nil {
		return err
	}
	defer rd.Close()
	if _, err := rd.Metadata(ctx); err != nil {
		return err
	}
	for {
		_, err := rd.Next(ctx)
		if errors.Is(err, msdata.ErrEndOfData) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
