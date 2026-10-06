package andiio

import (
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// ambiguousSample returns acquisition times whose spacing fits both the seconds
// and the minutes hypothesis, so only a stated unit can decide the question.
// It mirrors the file that prompted this ladder step: 0.0588 s between scans,
// which reads as 58.8 ms (a fast TOF) or 3.5 s (a slow quadrupole).
func ambiguousSample(n int, step, start float64) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = start + step*float64(i)
	}
	return v
}

func warned(ws []msdata.Diagnostic, code msdata.Code) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestResolveRTUnitFileGlobalUnit(t *testing.T) {
	rt := &VarRef{Name: "scan_acquisition_time"} // no units attribute, as Agilent writes it
	sample := ambiguousSample(24, 0.1, 0.05)

	tests := []struct {
		name       string
		policy     RTUnitPolicy
		globals    map[string]string
		wantUnit   string
		wantScale  float64
		wantOrigin string
		wantWarn   msdata.Code
		wantErr    bool
	}{
		{
			name: "seconds capitalised plural", policy: RTPolicyAuto,
			globals:  map[string]string{"units": "Seconds"},
			wantUnit: "second", wantScale: 1, wantOrigin: "global:units",
			wantWarn: msdata.CodeANDIUsedGlobalTimeUnit,
		},
		{
			name: "minutes", policy: RTPolicyAuto,
			globals:  map[string]string{"units": "minutes"},
			wantUnit: "minute", wantScale: 60, wantOrigin: "global:units",
			wantWarn: msdata.CodeANDIUsedGlobalTimeUnit,
		},
		{
			name: "time_units spelling", policy: RTPolicyAuto,
			globals:  map[string]string{"time_units": "s"},
			wantUnit: "second", wantScale: 1, wantOrigin: "global:time_units",
			wantWarn: msdata.CodeANDIUsedGlobalTimeUnit,
		},
		{
			name: "strict accepts a stated unit", policy: RTPolicyStrict,
			globals:  map[string]string{"units": "Seconds"},
			wantUnit: "second", wantScale: 1, wantOrigin: "global:units",
			wantWarn: msdata.CodeANDIUsedGlobalTimeUnit,
		},
		{
			// A global `units` is not necessarily a time unit: m/z and intensity
			// attributes carry their own. Accepting those by coincidence would
			// silently mis-scale the time axis, so the file must still refuse.
			name: "unrelated global unit is not a clock", policy: RTPolicyAuto,
			globals: map[string]string{"units": "M/Z"},
			wantErr: true,
		},
		{
			name: "empty global unit", policy: RTPolicyAuto,
			globals: map[string]string{"units": ""},
			wantErr: true,
		},
		{
			name: "no globals at all", policy: RTPolicyAuto,
			globals: nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ResolveRTUnit(tc.policy, rt, "", tc.globals, sample)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected refusal, got unit %+v", res.Unit)
				}
				if !errors.Is(err, msdata.ErrUnitUndetermined) {
					t.Errorf("err = %v, want ErrUnitUndetermined", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRTUnit: %v", err)
			}
			if res.Unit.Name != tc.wantUnit || res.Unit.Scale != tc.wantScale {
				t.Errorf("unit = %s/%g, want %s/%g", res.Unit.Name, res.Unit.Scale, tc.wantUnit, tc.wantScale)
			}
			if res.Unit.Origin != tc.wantOrigin {
				t.Errorf("origin = %q, want %q", res.Unit.Origin, tc.wantOrigin)
			}
			// A unit the file stated is explicit, never a guess: that is what makes
			// it acceptable under --rt-unit=strict.
			if res.Unit.Confidence != "explicit" {
				t.Errorf("confidence = %q, want explicit", res.Unit.Confidence)
			}
			if !warned(res.Warnings, tc.wantWarn) {
				t.Errorf("missing warning %s in %+v", tc.wantWarn, res.Warnings)
			}
		})
	}
}

// tofSample reproduces what sampleRT hands ResolveRTUnit for a real LECO
// GC-TOF export: times start after the solvent delay, are stored rounded to the
// instrument clock (550, 550.05, 550.1, ...), and are sampled as three runs of
// 256 from the start, middle and end. Differences between such large values
// fall a hair under the nominal period (0.049999999999954525 on the file that
// exposed this), which is what pushed 20 Hz past a 0.05 s floor.
func tofSample(scans int, start, period float64, decimals int) []float64 {
	scale := math.Pow(10, float64(decimals))
	at := func(i int) float64 { return math.Round((start+float64(i)*period)*scale) / scale }
	var out []float64
	for _, first := range []int{0, scans / 2, scans - 256} {
		for i := first; i < first+256; i++ {
			out = append(out, at(i))
		}
	}
	return out
}

// accumulate builds evenly spaced acquisition times from zero.
func accumulate(n int, period float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = float64(i) * period
	}
	return out
}

// TestResolveRTUnitTOFRatesAreNotMinutes pins the regression found on real
// LECO GC-TOF exports: unitless scan_acquisition_time at 20 spectra/s was
// inferred as MINUTES, and at 17 and 10 spectra/s it must not be either.
// Every one of these is a plausible seconds clock, so auto mode must refuse
// rather than pick minutes.
func TestResolveRTUnitTOFRatesAreNotMinutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scans    int
		start    float64
		period   float64
		decimals int
	}{
		// Shapes measured on production files (scan count, first time, period).
		{"20Hz", 56660, 550, 0.05, 2},
		{"17Hz", 14452, 350, 0.0588, 4},
		{"10Hz", 495, 10.106, 0.1, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := tofSample(tc.scans, tc.start, tc.period, tc.decimals)
			res, err := ResolveRTUnit(RTPolicyAuto, &VarRef{Name: "scan_acquisition_time"}, "", nil, sample)
			if err == nil {
				t.Fatalf("resolved %s (%s); want refusal as ambiguous", res.Unit.Name, res.Unit.Origin)
			}
			if !errors.Is(err, msdata.ErrUnitUndetermined) {
				t.Fatalf("err = %v, want ErrUnitUndetermined", err)
			}
		})
	}
}

func TestResolveRTUnitSpecificityAndConflict(t *testing.T) {
	sample := ambiguousSample(24, 0.1, 0.05)

	t.Run("variable unit wins over a disagreeing global", func(t *testing.T) {
		rt := &VarRef{Name: "scan_acquisition_time", Units: "minute"}
		res, err := ResolveRTUnit(RTPolicyAuto, rt, "", map[string]string{"units": "Seconds"}, sample)
		if err != nil {
			t.Fatal(err)
		}
		if res.Unit.Name != "minute" || res.Unit.Scale != 60 {
			t.Errorf("unit = %s/%g, want minute/60 (the more specific attribute)", res.Unit.Name, res.Unit.Scale)
		}
		if res.Unit.Origin != "var:scan_acquisition_time.units" {
			t.Errorf("origin = %q", res.Unit.Origin)
		}
		if !warned(res.Warnings, msdata.CodeANDIRTUnitConflict) {
			t.Errorf("expected %s: the file states two different clocks", msdata.CodeANDIRTUnitConflict)
		}
	})

	t.Run("agreeing units report no conflict", func(t *testing.T) {
		rt := &VarRef{Name: "scan_acquisition_time", Units: "second"}
		res, err := ResolveRTUnit(RTPolicyAuto, rt, "", map[string]string{"units": "Seconds"}, sample)
		if err != nil {
			t.Fatal(err)
		}
		if warned(res.Warnings, msdata.CodeANDIRTUnitConflict) {
			t.Error("seconds and Seconds agree; no conflict should be reported")
		}
	})

	t.Run("unparseable variable unit falls through to the global", func(t *testing.T) {
		rt := &VarRef{Name: "scan_acquisition_time", Units: "beats"}
		res, err := ResolveRTUnit(RTPolicyAuto, rt, "", map[string]string{"units": "Seconds"}, sample)
		if err != nil {
			t.Fatalf("a stated global unit should settle an unparseable variable unit: %v", err)
		}
		if res.Unit.Origin != "global:units" {
			t.Errorf("origin = %q, want global:units", res.Unit.Origin)
		}
		if !warned(res.Warnings, msdata.CodeANDIUnknownRTUnit) {
			t.Errorf("expected %s for the unreadable variable attribute", msdata.CodeANDIUnknownRTUnit)
		}
	})

	t.Run("point-level unit still outranks the global", func(t *testing.T) {
		rt := &VarRef{Name: "scan_acquisition_time"}
		res, err := ResolveRTUnit(RTPolicyAuto, rt, "minute", map[string]string{"units": "Seconds"}, sample)
		if err != nil {
			t.Fatal(err)
		}
		if res.Unit.Origin != "var:time_values.units" {
			t.Errorf("origin = %q, want var:time_values.units", res.Unit.Origin)
		}
		if res.Unit.Name != "minute" {
			t.Errorf("unit = %s, want minute", res.Unit.Name)
		}
		if !warned(res.Warnings, msdata.CodeANDIRTUnitConflict) {
			t.Errorf("expected %s", msdata.CodeANDIRTUnitConflict)
		}
	})

	t.Run("operator override still wins over everything", func(t *testing.T) {
		rt := &VarRef{Name: "scan_acquisition_time", Units: "minute"}
		res, err := ResolveRTUnit(RTPolicyMinutes, rt, "", map[string]string{"units": "Seconds"}, sample)
		if err != nil {
			t.Fatal(err)
		}
		if res.Unit.Origin != "policy:operator-minutes" {
			t.Errorf("origin = %q, want policy:operator-minutes", res.Unit.Origin)
		}
	})
}

// TestReaderFileGlobalTimeUnit reproduces the reported file shape end to end: a
// scan_acquisition_time with no attributes at all, no time_values, and a scan
// spacing that fits both clocks, decided only by the ASTM general-data `units`
// global. Before that global was consulted the file refused to convert.
func TestReaderFileGlobalTimeUnit(t *testing.T) {
	dir := t.TempDir()
	base := testutil.ANDIOptions{
		ScanCount: 40, PointsPerScan: []int{200},
		RTUnit: "", RTValueScale: 0.2, // first RT 0.05, spacing 0.1: undecidable
	}

	t.Run("seconds", func(t *testing.T) {
		opt := base
		opt.GlobalAttributes = map[string]string{"dataset_name": "global-units-s", "units": "Seconds"}
		r := openFixture(t, dir, "global-units-s", opt, nil)
		if u := r.RTUnit(); u.Name != "second" || u.Origin != "global:units" || u.Confidence != "explicit" {
			t.Errorf("rtUnit = %+v, want second/global:units/explicit", u)
		}
		if !warned(r.Diagnostics().Warnings, msdata.CodeANDIUsedGlobalTimeUnit) {
			t.Errorf("missing %s in %v", msdata.CodeANDIUsedGlobalTimeUnit, r.Diagnostics().Warnings)
		}
		res := sweep(t, r, expandPoints(opt), syntheticWant(false))
		if math.Abs(res.firstRT-0.05) > 1e-9 {
			t.Errorf("first RT = %v, want 0.05 (values are already seconds)", res.firstRT)
		}
	})

	t.Run("minutes are rescaled", func(t *testing.T) {
		opt := base
		opt.GlobalAttributes = map[string]string{"dataset_name": "global-units-m", "units": "Minutes"}
		r := openFixture(t, dir, "global-units-m", opt, nil)
		if u := r.RTUnit(); u.Name != "minute" || u.Scale != 60 {
			t.Errorf("rtUnit = %+v, want minute/60", u)
		}
		res := sweep(t, r, expandPoints(opt), syntheticWant(false))
		if math.Abs(res.firstRT-0.05*60) > 1e-9 {
			t.Errorf("first RT = %v, want %v", res.firstRT, 0.05*60)
		}
	})

	t.Run("strict accepts the stated global unit", func(t *testing.T) {
		opt := base
		opt.GlobalAttributes = map[string]string{"dataset_name": "global-units-strict", "units": "Seconds"}
		r := openFixture(t, dir, "global-units-strict", opt, func(o *Options) { o.RTUnit = RTPolicyStrict })
		if r.RTUnit().Origin != "global:units" {
			t.Errorf("origin = %q, want global:units", r.RTUnit().Origin)
		}
	})

	t.Run("a global unit that is not a time unit still refuses", func(t *testing.T) {
		opt := base
		opt.GlobalAttributes = map[string]string{"dataset_name": "global-units-mz", "units": "M/Z"}
		spec, err := testutil.ANDI(opt)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "global-units-mz.cdf")
		if err := testutil.WriteNetCDF(path, spec); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path, DefaultOptions()); !errors.Is(err, msdata.ErrUnitUndetermined) {
			t.Fatalf("err = %v, want ErrUnitUndetermined (an m/z unit says nothing about the clock)", err)
		}
	})
}

// TestResolveRTUnitStillDecidesClearCases keeps the magnitude test useful:
// spacing that is only plausible under one hypothesis is still decided.
func TestResolveRTUnitStillDecidesClearCases(t *testing.T) {
	// 30 unit spacing: 30 s is plausible, 30 min per scan is not.
	res, err := ResolveRTUnit(RTPolicyAuto, &VarRef{Name: "scan_acquisition_time"}, "", nil, accumulate(100, 30))
	if err != nil || res.Unit.Name != "second" {
		t.Fatalf("got %+v, %v; want seconds", res, err)
	}
	// 0.0005 unit spacing: 0.5 ms per scan is below any real rate, 30 ms is not.
	res, err = ResolveRTUnit(RTPolicyAuto, &VarRef{Name: "scan_acquisition_time"}, "", nil, accumulate(100, 0.0005))
	if err != nil || res.Unit.Name != "minute" {
		t.Fatalf("got %+v, %v; want minutes", res, err)
	}
}
