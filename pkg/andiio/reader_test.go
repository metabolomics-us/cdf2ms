package andiio

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/netcdfio"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

type sweepResult struct {
	spectra     int
	points      int64
	firstRT     float64
	lastRT      float64
	sumMZ       float64
	sumInt      float64
	minMZ       float64
	maxMZ       float64
	firstLen    int
	lastLen     int
	msLevelSeen map[int]int
	emptyScans  int
	maxAbsMZErr float64
	maxAbsIntEr float64
}

// sweep drains the reader, verifying array lengths and (when want is non-nil)
// every single value against the deterministic generator.
// wantFn returns the exact value the file must contain for (scan, point).
type wantFn func(scan, point int) (mz, inten float64)

// syntheticWant reproduces testutil's stored values, including the quantisation
// introduced by the netCDF packing convention, so a mis-addressed read can never
// pass by accident.
func syntheticWant(packed bool) wantFn {
	return func(scan, point int) (float64, float64) {
		mz := testutil.MassAt(scan, point)
		inten := testutil.IntensityAt(scan, point)
		if packed {
			return float64(int16(mz*10)) * 0.1, float64(int32(inten))
		}
		return float64(float32(mz)), float64(float32(inten))
	}
}

func sweep(t *testing.T, r *Reader, pointsPerScan []int, want wantFn) sweepResult {
	t.Helper()
	res := sweepResult{minMZ: math.Inf(1), maxMZ: math.Inf(-1), msLevelSeen: map[int]int{}}
	var prevRT = math.Inf(-1)
	idx := 0
	for {
		sp, err := r.Next(context.Background())
		if errors.Is(err, msdata.ErrEndOfData) {
			break
		}
		if err != nil {
			t.Fatalf("Next at scan %d: %v", res.spectra, err)
		}
		if sp.Index != res.spectra {
			t.Fatalf("scan index = %d, want %d", sp.Index, res.spectra)
		}
		if len(sp.MZ) != len(sp.Intensity) {
			t.Fatalf("scan %d: mz/int len %d/%d", sp.Index, len(sp.MZ), len(sp.Intensity))
		}
		if len(sp.MZ) == 0 {
			res.emptyScans++
		}
		if sp.MSLevel != nil {
			res.msLevelSeen[*sp.MSLevel]++
		}
		for p := range sp.MZ {
			z := sp.MZ[p]
			if math.IsNaN(z) || math.IsInf(z, 0) {
				t.Fatalf("scan %d point %d: non-finite m/z %v", sp.Index, p, z)
			}
			res.sumMZ += z
			res.sumInt += sp.Intensity[p]
			if z < res.minMZ {
				res.minMZ = z
			}
			if z > res.maxMZ {
				res.maxMZ = z
			}
			if want != nil {
				wantMZ, wantInt := want(sp.Index, p)
				if d := math.Abs(z - wantMZ); d > res.maxAbsMZErr {
					res.maxAbsMZErr = d
				}
				if d := math.Abs(sp.Intensity[p] - wantInt); d > res.maxAbsIntEr {
					res.maxAbsIntEr = d
				}
				if math.Abs(z-wantMZ) > 1e-6 && math.Abs(z-wantMZ) > 1e-6*math.Abs(wantMZ) {
					t.Fatalf("scan %d point %d: m/z = %v, want %v (global point %d)", sp.Index, p, z, wantMZ, idx)
				}
				if math.Abs(sp.Intensity[p]-wantInt) > 1e-3 {
					t.Fatalf("scan %d point %d: intensity = %v, want %v", sp.Index, p, sp.Intensity[p], wantInt)
				}
			}
			idx++
		}
		res.points += int64(len(sp.MZ))
		if res.spectra == 0 {
			res.firstRT = sp.RetentionTime
			res.firstLen = len(sp.MZ)
		}
		res.lastRT = sp.RetentionTime
		res.lastLen = len(sp.MZ)
		res.spectra++
		if sp.RetentionTimeSet {
			if sp.RetentionTime < prevRT {
				t.Fatalf("scan %d: RT went backwards (%v after %v)", sp.Index, sp.RetentionTime, prevRT)
			}
			prevRT = sp.RetentionTime
		}
	}
	if pointsPerScan != nil {
		wantTotal := 0
		for _, n := range pointsPerScan {
			wantTotal += n
		}
		if res.points != int64(wantTotal) {
			t.Fatalf("read %d points, want %d", res.points, wantTotal)
		}
	}
	return res
}

func expandPoints(opt testutil.ANDIOptions) []int {
	if opt.ZeroScans {
		return make([]int, opt.ScanCount)
	}
	pps := opt.PointsPerScan
	if len(pps) == 0 {
		pps = []int{5}
	}
	out := make([]int, opt.ScanCount)
	for i := range out {
		out[i] = pps[i%len(pps)]
	}
	return out
}

func openFixture(t *testing.T, dir string, name string, opt testutil.ANDIOptions, mutate func(*Options)) *Reader {
	t.Helper()
	spec, err := testutil.ANDI(opt)
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	path := filepath.Join(dir, name+".cdf")
	if err := testutil.WriteNetCDF(path, spec); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	opts := DefaultOptions()
	if mutate != nil {
		mutate(&opts)
	}
	r, err := Open(path, opts)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestReaderVariants(t *testing.T) {
	base := testutil.ANDIOptions{ScanCount: 24, PointsPerScan: []int{12, 7, 20}}
	cases := []struct {
		name    string
		mutate  func(*testutil.ANDIOptions)
		policy  RTUnitPolicy
		rtScale float64
		packed  bool
	}{
		{"classic", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds" }, RTPolicyAuto, 1, false},
		{"cdf2", func(o *testutil.ANDIOptions) { o.Version = 2; o.RTUnit = "Seconds" }, RTPolicyAuto, 1, false},
		{"cdf5", func(o *testutil.ANDIOptions) { o.Version = 5; o.RTUnit = "Seconds" }, RTPolicyAuto, 1, false},
		{"packed", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.Packing = true }, RTPolicyAuto, 1, true},
		{"minutes", func(o *testutil.ANDIOptions) { o.RTUnit = "Minutes" }, RTPolicyAuto, 60, false},
		{"minutes_abbrev", func(o *testutil.ANDIOptions) { o.RTUnit = "min" }, RTPolicyAuto, 60, false},
		{"no_units_attr_decisive", func(o *testutil.ANDIOptions) { o.RTValueScale = 60 }, RTPolicyAuto, 1, false},
		{"index_base_1", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.ScanIndexBase = 1 }, RTPolicyAuto, 1, false},
		{"no_point_count", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.OmitPointCount = true }, RTPolicyAuto, 1, false},
		{"no_scan_index", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.OmitScanIndex = true }, RTPolicyAuto, 1, false},
		{"no_total_intensity", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.OmitTotalIntensity = true }, RTPolicyAuto, 1, false},
		{"with_time_values", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.IncludeTimeValues = true }, RTPolicyAuto, 1, false},
		{"instrument_vars", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.IncludeInstrumentVars = true }, RTPolicyAuto, 1, false},
		{"ms_level_var", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds"; o.MSLevelVar = true }, RTPolicyAuto, 1, false},
		{"strict_seconds", func(o *testutil.ANDIOptions) { o.RTUnit = "Seconds" }, RTPolicyStrict, 1, false},
	}
	dir := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := base
			tc.mutate(&opt)
			r := openFixture(t, dir, tc.name, opt, func(o *Options) { o.RTUnit = tc.policy })
			pps := expandPoints(opt)
			res := sweep(t, r, pps, syntheticWant(tc.packed))

			if res.spectra != opt.ScanCount {
				t.Errorf("spectra = %d, want %d", res.spectra, opt.ScanCount)
			}
			scale := opt.RTValueScale
			if scale == 0 {
				scale = 1
			}
			wantFirst := 0.25 * scale * tc.rtScale
			wantLast := (float64(opt.ScanCount-1)*0.5 + 0.25) * scale * tc.rtScale
			if math.Abs(res.firstRT-wantFirst) > 1e-6 {
				t.Errorf("first RT = %v, want %v", res.firstRT, wantFirst)
			}
			if math.Abs(res.lastRT-wantLast) > 1e-6*max(1.0, wantLast) {
				t.Errorf("last RT = %v, want %v", res.lastRT, wantLast)
			}
			if res.firstLen != pps[0] || res.lastLen != pps[len(pps)-1] {
				t.Errorf("scan lengths first=%d last=%d want %d/%d", res.firstLen, res.lastLen, pps[0], pps[len(pps)-1])
			}
			if r.Plan().TotalPoints != res.points {
				t.Errorf("plan total points = %d, read %d", r.Plan().TotalPoints, res.points)
			}
			run, err := r.Metadata(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if run.RetentionTimeUnit != "second" {
				t.Errorf("RT unit = %q, want second", run.RetentionTimeUnit)
			}
			if run.ScanCount != opt.ScanCount {
				t.Errorf("run.ScanCount = %d, want %d", run.ScanCount, opt.ScanCount)
			}
			if run.SchemaFingerprint == "" || run.VendorFingerprint == "" {
				t.Error("fingerprints not populated")
			}
		})
	}
}

func TestReaderMSLevelAndPolarity(t *testing.T) {
	dir := t.TempDir()
	t.Run("ms_level_variable", func(t *testing.T) {
		opt := testutil.ANDIOptions{ScanCount: 9, PointsPerScan: []int{4}, RTUnit: "Seconds", MSLevelVar: true}
		r := openFixture(t, dir, "mslevel", opt, nil)
		res := sweep(t, r, expandPoints(opt), syntheticWant(false))
		if res.msLevelSeen[2] != 3 || res.msLevelSeen[1] != 6 {
			t.Errorf("ms levels = %v, want {1:6, 2:3}", res.msLevelSeen)
		}
	})
	t.Run("ms_level_default_is_reported", func(t *testing.T) {
		opt := testutil.ANDIOptions{ScanCount: 5, PointsPerScan: []int{4}, RTUnit: "Seconds"}
		r := openFixture(t, dir, "mslevel_default", opt, nil)
		sawWarn := false
		for _, w := range r.Diagnostics().Warnings {
			if w.Code == msdata.CodeANDIMSLevelUnknown {
				sawWarn = true
			}
		}
		if !sawWarn {
			t.Error("missing ANDI.MS_LEVEL_UNKNOWN warning for a file without ms_level")
		}
		run, _ := r.Metadata(context.Background())
		v, ok := run.Metadata["derived:ms_level"]
		if !ok || v.Origin == "" {
			t.Error("derived ms_level provenance not recorded")
		}
	})
	t.Run("run_level_polarity", func(t *testing.T) {
		opt := testutil.ANDIOptions{ScanCount: 4, PointsPerScan: []int{4}, RTUnit: "Seconds",
			GlobalAttributes: map[string]string{"test_ionization_polarity": "Positive Polarity"}}
		r := openFixture(t, dir, "polarity", opt, nil)
		res := sweep(t, r, expandPoints(opt), syntheticWant(false))
		_ = res
		sp, err := r.Next(context.Background())
		if !errors.Is(err, msdata.ErrEndOfData) {
			t.Fatalf("expected end of data, got %v", err)
		}
		_ = sp
	})
}

func TestReaderAmbiguousRTUnit(t *testing.T) {
	dir := t.TempDir()
	// 0.5 unit spacing with no stated unit: equally plausible as seconds or
	// minutes, so the reader must refuse rather than guess.
	opt := testutil.ANDIOptions{ScanCount: 20, PointsPerScan: []int{3}}
	spec, err := testutil.ANDI(opt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ambig.cdf")
	if err := testutil.WriteNetCDF(path, spec); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path, DefaultOptions())
	if err == nil {
		t.Fatal("expected ErrUnitUndetermined")
	}
	if !errors.Is(err, msdata.ErrUnitUndetermined) {
		t.Fatalf("err = %v, want ErrUnitUndetermined", err)
	}
	if msdata.CodeOf(err) != msdata.CodeANDIUnknownRTUnit {
		t.Errorf("code = %s", msdata.CodeOf(err))
	}
	// An explicit override must work and rescale.
	r, err := Open(path, Options{RTUnit: RTPolicyMinutes, Plan: DefaultPlanOptions(), HeaderLimits: netcdfio.DefaultLimits()})
	if err != nil {
		t.Fatalf("open with override: %v", err)
	}
	defer r.Close()
	res := sweep(t, r, expandPoints(opt), syntheticWant(false))
	if math.Abs(res.firstRT-0.25*60) > 1e-9 {
		t.Errorf("first RT = %v, want %v", res.firstRT, 0.25*60)
	}
}

func TestReaderStrictRejectsUnknownUnit(t *testing.T) {
	dir := t.TempDir()
	// An uninterpretable units attribute plus a magnitude that *is* decisive:
	// auto mode must fall through to the magnitude test, strict must not.
	opt := testutil.ANDIOptions{ScanCount: 10, PointsPerScan: []int{4}, RTUnit: "beats", RTValueScale: 60}
	spec, err := testutil.ANDI(opt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "beats.cdf")
	if err := testutil.WriteNetCDF(path, spec); err != nil {
		t.Fatal(err)
	}
	strict := DefaultOptions()
	strict.RTUnit = RTPolicyStrict
	if _, err := Open(path, strict); err == nil {
		t.Fatal("strict mode accepted an undocumented unit")
	}
	// auto mode decides by magnitude and says so.
	r, err := Open(path, DefaultOptions())
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	defer r.Close()
	if r.RTUnit().Confidence != "inferred" {
		t.Errorf("confidence = %s, want inferred", r.RTUnit().Confidence)
	}
	found := false
	for _, w := range r.Diagnostics().Warnings {
		if w.Code == msdata.CodeANDIUnitInferredFromMag {
			found = true
		}
	}
	if !found {
		t.Error("no ANDI.UNIT_INFERRED_FROM_MAGNITUDE warning")
	}
}

func TestReaderIndexWarnings(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		opt  testutil.ANDIOptions
		code msdata.Code
	}{
		{"one_based_index", testutil.ANDIOptions{ScanCount: 6, PointsPerScan: []int{5, 9}, RTUnit: "Seconds",
			ScanIndexBase: 1}, msdata.CodeANDIScanIndexBase},
		{"derived_counts", testutil.ANDIOptions{ScanCount: 6, PointsPerScan: []int{5, 9}, RTUnit: "Seconds",
			OmitPointCount: true}, msdata.CodeANDIPointCountMismatch},
		{"prefix_sum", testutil.ANDIOptions{ScanCount: 6, PointsPerScan: []int{5, 9}, RTUnit: "Seconds",
			OmitScanIndex: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := openFixture(t, dir, tc.name, tc.opt, nil)
			res := sweep(t, r, expandPoints(tc.opt), syntheticWant(false))
			if res.spectra != tc.opt.ScanCount {
				t.Errorf("spectra = %d, want %d", res.spectra, tc.opt.ScanCount)
			}
			if tc.code != "" {
				found := false
				for _, w := range r.Diagnostics().Warnings {
					if w.Code == tc.code {
						found = true
					}
				}
				if !found {
					t.Errorf("expected warning %s; got %v", tc.code, r.Diagnostics().Warnings)
				}
			}
		})
	}
}

func TestReaderZeroScanFile(t *testing.T) {
	dir := t.TempDir()
	opt := testutil.ANDIOptions{ScanCount: 4, ZeroScans: true, RTUnit: "Seconds"}
	r := openFixture(t, dir, "zero", opt, nil)
	res := sweep(t, r, nil, nil)
	if res.spectra != 4 || res.points != 0 || res.emptyScans != 4 {
		t.Errorf("got spectra=%d points=%d empty=%d", res.spectra, res.points, res.emptyScans)
	}
}

func TestReaderNonUTF8Metadata(t *testing.T) {
	dir := t.TempDir()
	opt := testutil.ANDIOptions{ScanCount: 4, PointsPerScan: []int{3}, RTUnit: "Seconds", NonUTF8Title: true}
	r := openFixture(t, dir, "latin1", opt, nil)
	found := false
	for _, w := range r.Diagnostics().Warnings {
		if w.Code == msdata.CodeANDIMetadataNotUTF8 {
			found = true
		}
	}
	if !found {
		t.Error("non-UTF-8 global did not raise ANDI.METADATA_NOT_UTF8")
	}
	run, _ := r.Metadata(context.Background())
	if v := run.Metadata["global:experiment_title"]; v.Str == "" {
		t.Error("experiment_title was dropped instead of sanitised")
	}
}

func TestReaderInstrumentMapping(t *testing.T) {
	dir := t.TempDir()
	opt := testutil.ANDIOptions{ScanCount: 3, PointsPerScan: []int{3}, RTUnit: "Seconds",
		IncludeInstrumentVars: true,
		GlobalAttributes: map[string]string{
			"test_ionization_mode": "Electron Ionization",
			"test_ms_inlet":        "GC/MS",
			"experiment_type":      "Centroided Mass Spectrum",
		}}
	r := openFixture(t, dir, "inst", opt, nil)
	run, _ := r.Metadata(context.Background())
	if run.Instrument.Name != "Synthetic QMS" {
		t.Errorf("instrument name = %q", run.Instrument.Name)
	}
	if run.Instrument.Model != "SIM-QQQ-9000" {
		t.Errorf("instrument model = %q", run.Instrument.Model)
	}
	if run.Instrument.Manufacturer != "Synthetic Instruments Inc." {
		t.Errorf("manufacturer = %q", run.Instrument.Manufacturer)
	}
	if run.Instrument.IonizationMode != "Electron Ionization" {
		t.Errorf("ionization = %q", run.Instrument.IonizationMode)
	}
	if run.Instrument.MSInlet != "GC/MS" {
		t.Errorf("inlet = %q", run.Instrument.MSInlet)
	}
	if st, _ := DeriveCentroid(run.Metadata["global:experiment_type"]); st != msdata.CentroidCentroided {
		t.Errorf("centroid state = %s", st)
	}
	if pol, _ := DerivePolarity(netcdfio.Attribute{IsChar: true, Str: "Positive Polarity"}, 0, false); pol != msdata.PolarityPositive {
		t.Error("polarity mapping broken")
	}
}

func TestReaderTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	opt := testutil.ANDIOptions{ScanCount: 40, PointsPerScan: []int{50}, RTUnit: "Seconds"}
	spec, err := testutil.ANDI(opt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "trunc.cdf")
	if err := testutil.WriteNetCDF(path, spec); err != nil {
		t.Fatal(err)
	}
	full, _ := os.ReadFile(path)
	// Cut the last 40% of the data section.
	if err := os.WriteFile(path, full[:int64(len(full))*6/10], 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, DefaultOptions())
	if err != nil {
		// Either a hard truncation error or a successful open that reports
		// truncation is acceptable; silently succeeding is not.
		if !errors.Is(err, netcdfio.ErrShortFile) && msdata.CodeOf(err) != msdata.CodeSourceTruncated {
			t.Fatalf("open error = %v", err)
		}
		return
	}
	defer r.Close()
	found := false
	for _, w := range r.Diagnostics().Warnings {
		if w.Code == msdata.CodeSourceTruncated || w.Code == msdata.CodeSourceUnreadablePoints {
			found = true
		}
	}
	if !found {
		t.Error("truncated file produced no truncation diagnostic")
	}
}

func TestReaderHDF5Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nc4.cdf")
	if err := os.WriteFile(path, []byte("\x89HDF\r\n\x1a\n\x00\x00\x00\x00\x00\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, DefaultOptions())
	if err == nil {
		t.Fatal("HDF5 file was accepted")
	}
	if msdata.CodeOf(err) != msdata.CodeNCUnsupportedEncoding {
		t.Errorf("code = %s, want %s", msdata.CodeOf(err), msdata.CodeNCUnsupportedEncoding)
	}
}

// TestReaderRealCorpus cross-checks against real vendor files when available.
func TestReaderRealCorpus(t *testing.T) {
	dir := os.Getenv("CDF2MS_CORPUS")
	if dir == "" {
		t.Skip("set CDF2MS_CORPUS to a directory of real ANDI .cdf files")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.[Cc][Dd][Ff]"))
	if len(files) == 0 {
		t.Skipf("no .cdf files under %s", dir)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			r, err := Open(path, DefaultOptions())
			if err != nil {
				if errors.Is(err, netcdfio.ErrUnsupportedEncoding) {
					t.Skipf("unsupported container: %v", err)
				}
				t.Fatalf("Open: %v", err)
			}
			defer r.Close()
			res := sweep(t, r, nil, nil)
			t.Logf("%s: spectra=%d points=%d mz=[%.2f,%.2f] rt=[%.3f,%.3f] unit=%s/%s plan=%s",
				filepath.Base(path), res.spectra, res.points, res.minMZ, res.maxMZ,
				res.firstRT, res.lastRT, r.RTUnit().Name, r.RTUnit().Confidence, r.Plan().SanityReport())
			if res.spectra == 0 {
				t.Fatal("no spectra")
			}
			if res.minMZ <= 0 || res.minMZ > 10000 {
				t.Errorf("implausible min m/z %v", res.minMZ)
			}
			if res.firstRT < 0 || res.firstRT > 86400 {
				t.Errorf("implausible first RT %v s", res.firstRT)
			}
			if r.Plan().EmptyScans == r.Plan().Scans && r.Plan().Scans > 0 {
				t.Error("all scans are empty")
			}
		})
	}
}
