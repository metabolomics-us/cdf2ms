package andiio

import (
	"errors"
	"math"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

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
			res, err := ResolveRTUnit(RTPolicyAuto, &VarRef{Name: "scan_acquisition_time"}, "", sample)
			if err == nil {
				t.Fatalf("resolved %s (%s); want refusal as ambiguous", res.Unit.Name, res.Unit.Origin)
			}
			if !errors.Is(err, msdata.ErrUnitUndetermined) {
				t.Fatalf("err = %v, want ErrUnitUndetermined", err)
			}
		})
	}
}

// TestResolveRTUnitStillDecidesClearCases keeps the magnitude test useful:
// spacing that is only plausible under one hypothesis is still decided.
func TestResolveRTUnitStillDecidesClearCases(t *testing.T) {
	// 30 unit spacing: 30 s is plausible, 30 min per scan is not.
	res, err := ResolveRTUnit(RTPolicyAuto, &VarRef{Name: "scan_acquisition_time"}, "", accumulate(100, 30))
	if err != nil || res.Unit.Name != "second" {
		t.Fatalf("got %+v, %v; want seconds", res, err)
	}
	// 0.0005 unit spacing: 0.5 ms per scan is below any real rate, 30 ms is not.
	res, err = ResolveRTUnit(RTPolicyAuto, &VarRef{Name: "scan_acquisition_time"}, "", accumulate(100, 0.0005))
	if err != nil || res.Unit.Name != "minute" {
		t.Fatalf("got %+v, %v; want minutes", res, err)
	}
}
