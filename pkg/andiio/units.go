package andiio

import (
	"fmt"
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// RTUnitPolicy selects how the retention-time unit of scan_acquisition_time is
// resolved.
type RTUnitPolicy string

const (
	// RTPolicyAuto uses, in order: the variable's own units attribute, the
	// point-level time_values units attribute, then a decisive magnitude test.
	// If the magnitude test is not decisive it fails rather than guessing.
	RTPolicyAuto RTUnitPolicy = "auto"
	// RTPolicyStrict requires an explicit units attribute; anything else fails.
	RTPolicyStrict RTUnitPolicy = "strict"
	// RTPolicySeconds forces seconds.
	RTPolicySeconds RTUnitPolicy = "seconds"
	// RTPolicyMinutes forces minutes.
	RTPolicyMinutes RTUnitPolicy = "minutes"
)

// ParseRTUnitPolicy validates operator input for --rt-unit.
func ParseRTUnitPolicy(s string) (RTUnitPolicy, error) {
	switch RTUnitPolicy(strings.ToLower(strings.TrimSpace(s))) {
	case "", RTPolicyAuto:
		return RTPolicyAuto, nil
	case RTPolicyStrict:
		return RTPolicyStrict, nil
	case RTPolicySeconds, "second", "s", "sec", "secs":
		return RTPolicySeconds, nil
	case RTPolicyMinutes, "minute", "min", "mins":
		return RTPolicyMinutes, nil
	}
	return "", fmt.Errorf("invalid --rt-unit %q (want auto|strict|seconds|minutes)", s)
}

// RTUnit is a resolved retention-time unit.
type RTUnit struct {
	// Name is the canonical unit name ("second" or "minute").
	Name string
	// Scale converts a source value to seconds.
	Scale float64
	// Origin documents the decision, e.g. "var:scan_acquisition_time.units".
	Origin string
	// Confidence is "explicit" when the source stated the unit, "policy" when a
	// rule or operator override decided, and "inference" when the magnitude test
	// decided (always accompanied by a warning).
	Confidence string
}

// unitTable maps the vocabulary actually seen in ANDI/MS exports. Keys are
// normalized (lowercase, no spaces/underscores). The value is the seconds per
// unit. Entries marked reject are units that cannot be a retention time.
var unitTable = map[string]float64{
	"s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1, "sec.": 1, "secondtime": 1,
	"m": 60, "min": 60, "mins": 60, "minute": 60, "minutes": 60,
	"h": 3600, "hr": 3600, "hrs": 3600, "hour": 3600, "hours": 3600,
	"ms": 0.001, "msec": 0.001, "millisecond": 0.001, "milliseconds": 0.001,
}

// ambiguousUnitStrings are units that name a time-like quantity but not a scale
// we may assume (e.g. scan-index-like values).
var ambiguousUnitStrings = map[string]bool{
	"scans":          true,
	"scannumber":     true,
	"fractionalscan": true,
	"points":         true,
	"index":          true,
	"slicenumber":    true,
}

// classifyUnit maps a units string to a scale. ok=false means "not recognised".
func classifyUnit(raw string) (scale float64, name string, ok bool, rejectMsg string) {
	k := normalizeUnit(raw)
	if k == "" {
		return 0, "", false, ""
	}
	if ambiguousUnitStrings[k] {
		return 0, "", false, fmt.Sprintf("units %q is not a time unit", raw)
	}
	if s, found := unitTable[k]; found {
		if s == 1 {
			return 1, "second", true, ""
		}
		if s == 60 {
			return 60, "minute", true, ""
		}
		return s, "second", true, ""
	}
	return 0, "", false, fmt.Sprintf("unrecognised units %q", raw)
}

func normalizeUnit(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case ' ', '_', '-', '.', '/', '(', ')', '[', ']':
			// drop separators: "Secs" == "secs" == "sec s"
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RTResolution is the outcome of resolving a file's retention-time unit.
type RTResolution struct {
	Unit      RTUnit
	Warnings  []msdata.Diagnostic
	Ambiguous bool
	Detail    string
}

// MagnitudeEvidence summarises the decisive magnitude test so reports can show
// why a unit was chosen.
type MagnitudeEvidence struct {
	ScanCount   int
	MedianDelta float64
	MinDelta    float64
	MaxDelta    float64
	Range       float64
	// ImpliedDeltaSeconds holds the median scan spacing under each hypothesis.
	ImpliedDeltaSeconds float64
	ImpliedDeltaMinutes float64
}

// plausibleScanSpacing bounds a single acquisition in seconds. Real quadrupole
// acquisitions run 0.05 s to 60 s per scan; anything far outside that band under
// both hypotheses means the unit cannot be decided from magnitude.
const (
	minPlausibleScanSec = 0.05
	maxPlausibleScanSec = 60.0
)

// ResolveRTUnit decides the retention-time unit for a file.
//
// rtVar is the scan-level acquisition time variable; tvUnits is the units
// attribute of the point-level time_values variable (may be empty).
// sample holds a strided sample of acquisition-time values in source units.
func ResolveRTUnit(policy RTUnitPolicy, rtVar *VarRef, tvUnits string, sample []float64) (*RTResolution, error) {
	res := &RTResolution{}

	// Operator override wins and is recorded as such.
	switch policy {
	case RTPolicySeconds:
		res.Unit = RTUnit{Name: "second", Scale: 1, Origin: "policy:operator-seconds", Confidence: "policy"}
		res.Warnings = append(res.Warnings, msdata.Diagnostic{
			Code: msdata.CodeANDIAmbiguousUnits,
			Message: "retention-time unit forced to seconds by operator; " +
				"scan_acquisition_time values are taken at face value",
		})
		return res, nil
	case RTPolicyMinutes:
		res.Unit = RTUnit{Name: "minute", Scale: 60, Origin: "policy:operator-minutes", Confidence: "policy"}
		res.Warnings = append(res.Warnings, msdata.Diagnostic{
			Code: msdata.CodeANDIAmbiguousUnits,
			Message: "retention-time unit forced to minutes by operator; " +
				"scan_acquisition_time values are multiplied by 60",
		})
		return res, nil
	}

	// 1. the variable's own units attribute
	if rtVar != nil && rtVar.Units != "" {
		if scale, name, ok, msg := classifyUnit(rtVar.Units); ok {
			res.Unit = RTUnit{Name: name, Scale: scale,
				Origin: "var:" + rtVar.Name + ".units", Confidence: "explicit"}
			if scale != 1 && scale != 60 {
				res.Warnings = append(res.Warnings, msdata.Diagnostic{
					Code:    msdata.CodeANDIAmbiguousUnits,
					Message: fmt.Sprintf("retention-time units %q interpreted as %g seconds per unit", rtVar.Units, scale),
				})
			}
			return res, nil
		} else if msg != "" {
			if policy == RTPolicyStrict {
				return nil, fmt.Errorf("%w: scan_acquisition_time has %s", msdata.ErrUnitUndetermined, msg)
			}
			res.Warnings = append(res.Warnings, msdata.Diagnostic{
				Code: msdata.CodeANDIUnknownRTUnit,
				Message: fmt.Sprintf("scan_acquisition_time.units is %q and could not be interpreted; "+
					"falling back to the inference ladder", rtVar.Units),
			})
		}
	}

	// 2. the point-level time_values units attribute (same clock family in the
	// ANDI template, and the only stated time unit in many real exports)
	if tvUnits != "" {
		if scale, name, ok, _ := classifyUnit(tvUnits); ok {
			res.Unit = RTUnit{Name: name, Scale: scale, Origin: "var:time_values.units", Confidence: "explicit"}
			res.Warnings = append(res.Warnings, msdata.Diagnostic{
				Code: msdata.CodeANDIUsedPointTimeUnit,
				Message: fmt.Sprintf("scan_acquisition_time has no units attribute; inherited %q from "+
					"time_values.units (%s)", tvUnits, name),
			})
			return res, nil
		}
	}

	if policy == RTPolicyStrict {
		return nil, fmt.Errorf("%w: scan_acquisition_time carries no units attribute and "+
			"--rt-unit=strict forbids inference", msdata.ErrUnitUndetermined)
	}

	// 3. decisive magnitude test
	ev, ok := decideByMagnitude(sample)
	res.Detail = ev.text()
	if ok {
		if ev.ImpliedDeltaSeconds >= minPlausibleScanSec && ev.ImpliedDeltaSeconds <= maxPlausibleScanSec {
			res.Unit = RTUnit{Name: "second", Scale: 1, Origin: "inference:scan-spacing", Confidence: "inferred"}
			res.Warnings = append(res.Warnings, msdata.Diagnostic{
				Code: msdata.CodeANDIUnitInferredFromMag,
				Message: fmt.Sprintf("scan_acquisition_time has no units attribute; inferred SECONDS because the "+
					"median scan spacing is %.4g s (minutes would imply %.4g s/scan, implausible). %s",
					ev.ImpliedDeltaSeconds, ev.ImpliedDeltaMinutes, ev.text()),
			})
			return res, nil
		}
		if ev.ImpliedDeltaMinutes >= minPlausibleScanSec && ev.ImpliedDeltaMinutes <= maxPlausibleScanSec {
			res.Unit = RTUnit{Name: "minute", Scale: 60, Origin: "inference:scan-spacing", Confidence: "inferred"}
			res.Warnings = append(res.Warnings, msdata.Diagnostic{
				Code: msdata.CodeANDIUnitInferredFromMag,
				Message: fmt.Sprintf("scan_acquisition_time has no units attribute; inferred MINUTES because the "+
					"median scan spacing would be %.4g s (seconds would imply %.4g s/scan, implausible). %s",
					ev.ImpliedDeltaMinutes, ev.ImpliedDeltaSeconds, ev.text()),
			})
			return res, nil
		}
	}

	res.Ambiguous = true
	return nil, fmt.Errorf("%w: scan_acquisition_time has no usable units attribute and the values are "+
		"not decisive (%s). Re-run with --rt-unit=seconds or --rt-unit=minutes once you have confirmed "+
		"the acquisition clock for this instrument.", msdata.ErrUnitUndetermined, ev.text())
}

// decideByMagnitude computes the median inter-scan delta and the implied spacing
// under the seconds and minutes hypotheses.
func decideByMagnitude(sample []float64) (MagnitudeEvidence, bool) {
	ev := MagnitudeEvidence{}
	if len(sample) < 3 {
		return ev, false
	}
	deltas := make([]float64, 0, len(sample))
	minv, maxv := sample[0], sample[0]
	for i, v := range sample {
		if v < minv {
			minv = v
		}
		if v > maxv {
			maxv = v
		}
		if i > 0 {
			d := v - sample[i-1]
			if d > 0 {
				deltas = append(deltas, d)
			}
		}
	}
	if len(deltas) == 0 {
		return ev, false
	}
	median := quickMedian(deltas)
	ev.ScanCount = len(sample)
	ev.MedianDelta = median
	ev.Range = maxv - minv
	ev.ImpliedDeltaSeconds = median
	ev.ImpliedDeltaMinutes = median * 60
	// Decisive means exactly one hypothesis lands inside the plausible band.
	inSec := median >= minPlausibleScanSec && median <= maxPlausibleScanSec
	inMin := median*60 >= minPlausibleScanSec && median*60 <= maxPlausibleScanSec
	return ev, inSec != inMin
}

func (ev MagnitudeEvidence) text() string {
	if ev.ScanCount == 0 {
		return "insufficient monotonic acquisition-time samples"
	}
	return fmt.Sprintf("samples=%d median_delta=%.6g total_range=%.6g implied_s=%.6g implied_min=%.6g",
		ev.ScanCount, ev.MedianDelta, ev.Range, ev.ImpliedDeltaSeconds, ev.ImpliedDeltaMinutes)
}

// quickMedian returns the median without mutating the caller's slice.
func quickMedian(v []float64) float64 {
	n := len(v)
	if n == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	// Simple insertion-free approach: partial selection would be faster, but n
	// is bounded by the sample size (<= 512), so sorting is negligible.
	sortFloats(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2]) / 2
}

func sortFloats(s []float64) {
	// insertion sort: only ever used on small samples
	for i := 1; i < len(s); i++ {
		x := s[i]
		j := i - 1
		for j >= 0 && s[j] > x {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = x
	}
}
