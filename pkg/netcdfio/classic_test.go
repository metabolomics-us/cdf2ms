package netcdfio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

func writeTemp(t *testing.T, spec testutil.FileSpec) string {
	t.Helper()
	b, err := testutil.EncodeNetCDF(spec)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p := filepath.Join(t.TempDir(), "case.cdf")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestRoundTripRecordVariables(t *testing.T) {
	for _, version := range []int{1, 2, 5} {
		t.Run(map[int]string{1: "CDF-1", 2: "CDF-2", 5: "CDF-5"}[version], func(t *testing.T) {
			spec, err := testutil.ANDI(testutil.ANDIOptions{
				Version:               version,
				ScanCount:             7,
				PointsPerScan:         []int{3, 5, 2, 8, 1, 4, 6},
				RTUnit:                "Seconds",
				IncludeTimeValues:     true,
				PointTimeUnit:         "Seconds",
				IncludeInstrumentVars: true,
			})
			if err != nil {
				t.Fatalf("andi spec: %v", err)
			}
			path := writeTemp(t, spec)

			enc, _, err := Detect(path)
			if err != nil {
				t.Fatalf("detect: %v", err)
			}
			want := EncodingCDF1
			switch version {
			case 2:
				want = EncodingCDF2
			case 5:
				want = EncodingCDF5
			}
			if enc != want {
				t.Fatalf("encoding = %v, want %v", enc, want)
			}

			f, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			if f.NumRecs != 29 {
				t.Errorf("numrecs = %d, want 29", f.NumRecs)
			}
			if f.RecSize != 12 {
				t.Errorf("recsize = %d, want 12", f.RecSize)
			}
			mv, err := f.Var("mass_values")
			if err != nil {
				t.Fatalf("mass_values: %v", err)
			}
			if !mv.IsRecord {
				t.Error("mass_values should be a record variable")
			}
			if mv.Extent() != 29 {
				t.Errorf("mass_values extent = %d, want 29", mv.Extent())
			}

			// Read every scan and verify record-aware addressing.
			offsets := []int{0, 3, 8, 10, 18, 19, 23}
			counts := []int{3, 5, 2, 8, 1, 4, 6}
			pts := 0
			for scan, off := range offsets {
				vals, err := f.ReadFloats(mv, int64(off), int64(counts[scan]), nil)
				if err != nil {
					t.Fatalf("scan %d read: %v", scan, err)
				}
				if len(vals) != counts[scan] {
					t.Fatalf("scan %d got %d values, want %d", scan, len(vals), counts[scan])
				}
				for p, v := range vals {
					// mass_values is float32 on disk: compare against the float32
					// representation, which is exact.
					want := float64(float32(testutil.MassAt(scan, p)))
					if v != want {
						t.Fatalf("scan %d peak %d: mz %v want %v", scan, p, v, want)
					}
				}
				pts += counts[scan]
			}
			if pts != 29 {
				t.Errorf("total points %d, want 29", pts)
			}

			// Instrument char variables.
			if v, err := f.Var("instrument_model"); err == nil {
				s, err := f.ReadString(v)
				if err != nil {
					t.Fatalf("read string: %v", err)
				}
				if s != "SIM-QQQ-9000" {
					t.Errorf("instrument_model = %q", s)
				}
			} else if version > 0 {
				t.Fatalf("instrument_model: %v", err)
			}

			if a, ok := f.Globals["ms_template_revision"]; !ok || a.Str != "1.0.1" {
				t.Errorf("global ms_template_revision = %+v", a)
			}
		})
	}
}

func TestPackedAttributesAreTyped(t *testing.T) {
	spec, err := testutil.ANDI(testutil.ANDIOptions{ScanCount: 2, PointsPerScan: []int{3, 2}, Packing: true})
	if err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, spec)
	f, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	mv, _ := f.Var("mass_values")
	a, ok := mv.Attrs["scale_factor"]
	if !ok {
		t.Fatal("scale_factor missing")
	}
	if a.Kind() != "float" {
		t.Errorf("scale_factor kind = %s, want float", a.Kind())
	}
	v, ok := a.Float()
	if !ok || v != 0.1 {
		t.Errorf("scale_factor = %v, want 0.1", v)
	}
	vals, err := f.ReadFloats(mv, 0, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Raw packed values must be returned unmodified: 0.7*10 = 7 (rounded to int16)
	if vals[0] != 500 {
		t.Errorf("packed raw value = %v, want 500", vals[0])
	}
}

func TestTruncatedFileIsDetected(t *testing.T) {
	spec, err := testutil.ANDI(testutil.ANDIOptions{ScanCount: 5, PointsPerScan: []int{100}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := testutil.EncodeNetCDF(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "trunc.cdf")
	if err := os.WriteFile(path, b[:len(b)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(path)
	if err != nil {
		// Rejecting outright is also acceptable.
		return
	}
	defer f.Close()
	if !f.Truncated {
		t.Error("expected Truncated=true")
	}
	mv, _ := f.Var("mass_values")
	if _, err := f.ReadFloats(mv, 0, 500, nil); err == nil {
		t.Error("expected out-of-bounds/truncation error, got nil")
	} else if !(errors.Is(err, ErrShortFile) || errors.Is(err, ErrOutOfBounds)) {
		t.Errorf("unexpected error type: %v", err)
	}
}

func TestHostileHeadersDoNotPanic(t *testing.T) {
	cases := map[string][]byte{
		"empty":            {},
		"cdf1-nothing":     []byte{'C', 'D', 'F', 1},
		"zeros":            make([]byte, 64),
		"bad-dim-tag":      {'C', 'D', 'F', 1, 0, 0, 0, 2, 0, 0, 0, 0x99, 0, 0, 0, 1},
		"absurd-nelems":    {'C', 'D', 'F', 1, 0, 0, 0, 1, 0, 0, 0, 0x0A, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 1},
		"hdf5-magic":       {0x89, 'H', 'D', 'F', 0, 0, 0, 0},
		"negative-numrecs": {'C', 'D', 'F', 5, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "h.cdf")
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			_, _ = Open(path) // must not panic
		})
	}
}

func TestRealCorpusIfAvailable(t *testing.T) {
	dir := os.Getenv("CDF2MS_CORPUS")
	if dir == "" {
		t.Skip("CDF2MS_CORPUS not set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("corpus dir unreadable: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		enc, _, err := Detect(p)
		if err != nil {
			t.Errorf("%s: detect: %v", e.Name(), err)
			continue
		}
		if enc == EncodingHDF5 {
			continue
		}
		f, err := Open(p)
		if err != nil {
			t.Errorf("%s: open: %v", e.Name(), err)
			continue
		}
		if _, err := f.Var("mass_values"); err != nil {
			f.Close()
			continue
		}
		f.Close()
	}
}
