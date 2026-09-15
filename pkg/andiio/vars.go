// Package andiio reads ANDI/MS (ASTM E1205 / E1947) netCDF files and presents
// them as the format-neutral msdata model.
//
// Design rules, in priority order:
//
//  1. Never invent scientific facts. Fields the source does not assert stay
//     unset; fields derived by policy record their derivation origin and emit
//     a warning so the choice is visible in reports.
//  2. Never silently change numbers. Packed data (scale_factor/add_offset) is
//     unpacked exactly as the netCDF convention prescribes; fill values and
//     non-finite numbers are counted and reported, not quietly dropped.
//  3. Bounded memory. Index arrays are read in blocks and peak arrays are read
//     through a record-window de-interleaver, so peak RAM use is independent of
//     file size.
package andiio

import (
	"fmt"
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/netcdfio"
)

// VarRef is a resolved ANDI variable plus the unpacking parameters that apply
// to it.
type VarRef struct {
	Var *netcdfio.Var
	// Name is the actual variable name found in the file.
	Name string
	// Aliased is true when the variable was found under a non-canonical name.
	Aliased bool

	// Scale/Add implement the netCDF packing convention:
	//   physical = stored * Scale + Add
	Scale float64
	Add   float64
	// Packed reports whether scale_factor or add_offset was present.
	Packed bool

	// FillValue is the declared _FillValue (when present).
	FillValue    float64
	HasFill      bool
	MissingValue float64
	HasMissing   bool

	// Units is the verbatim units attribute, when present.
	Units string
}

// Unpack maps a stored value to its physical value.
func (r *VarRef) Unpack(v float64) float64 {
	if r == nil || !r.Packed {
		return v
	}
	return v*r.Scale + r.Add
}

// IsFill reports whether v is the variable's fill/missing sentinel. The netCDF
// default fill values are recognised in addition to an explicit _FillValue,
// because ANDI exporters routinely leave arrays filled with 9.96921e+36.
func (r *VarRef) IsFill(v float64) bool {
	if r.HasFill && v == r.FillValue {
		return true
	}
	if r.HasMissing && v == r.MissingValue {
		return true
	}
	return netcdfio.IsFillFloat(r.Var.Type, v)
}

// canonical variable names, most-specific first
var (
	canonicalMass        = []string{"mass_values", "mass_value", "mz_values", "mass"}
	canonicalIntensity   = []string{"intensity_values", "intensity_value", "intensity", "abundance_values"}
	canonicalTimeValues  = []string{"time_values", "time_value", "point_time_values"}
	canonicalScanIndex   = []string{"scan_index", "scanindex", "scan_offset", "spectrum_index"}
	canonicalPointCount  = []string{"point_count", "pointcount", "peak_count", "n_peaks", "num_points"}
	canonicalRT          = []string{"scan_acquisition_time", "scan_acquisition_times", "retention_time", "acquisition_time", "scan_time"}
	canonicalScanNumber  = []string{"actual_scan_number", "scan_number_values", "scan_no", "spectrum_number"}
	canonicalTIC         = []string{"total_intensity", "total_ion_current", "tic", "base_peak_area"}
	canonicalMassMin     = []string{"mass_range_min", "min_mass", "low_mz"}
	canonicalMassMax     = []string{"mass_range_max", "max_mass", "high_mz"}
	canonicalTimeMin     = []string{"time_range_min"}
	canonicalTimeMax     = []string{"time_range_max"}
	canonicalScanDur     = []string{"scan_duration", "scan_time_duration"}
	canonicalInterScan   = []string{"inter_scan_time"}
	canonicalResolution  = []string{"resolution", "mass_resolution"}
	canonicalMSLevel     = []string{"ms_level", "mslevel", "analysis_level"}
	canonicalPolarity    = []string{"scan_polarity", "polarity", "ion_polarity", "ms_polarity"}
	canonicalBasePeakMZ  = []string{"base_peak_mz", "base_peak_mass", "basepeaktmz"}
	canonicalBasePeakInt = []string{"base_peak_intensity", "base_peak_height", "basepeakintensity"}
	canonicalPrecursorMZ = []string{"precursor_mz", "precursor_m/z", "precursor_ref", "precursor_mass", "isolated_mz"}
	canonicalPrecursorIn = []string{"precursor_intensity", "precursor_abs_intensity"}
	canonicalPrecursorZ  = []string{"precursor_charge", "charge", "charge_state"}
	canonicalCollisionE  = []string{"collision_energy", "collision_energy_ev", "fragmentation_energy"}
	canonicalTotalIons   = []string{"total_ions", "complete_scan", "total_current"}
)

// Layout is the resolved variable map of one ANDI/MS file.
type Layout struct {
	Mass        *VarRef
	Intensity   *VarRef
	TimeValues  *VarRef
	ScanIndex   *VarRef
	PointCount  *VarRef
	RT          *VarRef
	ScanNumber  *VarRef
	TIC         *VarRef
	MassMin     *VarRef
	MassMax     *VarRef
	TimeMin     *VarRef
	TimeMax     *VarRef
	ScanDur     *VarRef
	InterScan   *VarRef
	Resolution  *VarRef
	MSLevel     *VarRef
	Polarity    *VarRef
	BasePeakMZ  *VarRef
	BasePeakInt *VarRef
	PrecursorMZ *VarRef
	PrecursorIn *VarRef
	PrecursorZ  *VarRef
	CollisionE  *VarRef

	// Instrument text variables (char arrays).
	InstrumentVars map[string]*VarRef

	// Unmapped holds ANDI variables that exist but that cdf2ms does not map to a
	// spectrum field (a_d_sampling_rate, error_log, flag_count, ...). They are
	// preserved as run metadata rather than discarded.
	Unmapped []*VarRef
}

// resolveVar finds the first candidate name present in the file (case- and
// separator-insensitive).
func resolveVar(f *netcdfio.File, taken map[string]bool, candidates []string) *VarRef {
	names := f.VarNames()
	lower := make(map[string]string, len(names))
	for _, n := range names {
		lower[normalizeKey(n)] = n
	}
	for _, cand := range candidates {
		if actual, ok := lower[normalizeKey(cand)]; ok {
			if taken[actual] {
				continue
			}
			v, err := f.Var(actual)
			if err != nil {
				continue
			}
			taken[actual] = true
			return varRefFrom(v)
		}
	}
	return nil
}

func normalizeKey(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b = append(b, c+32)
		case c == '-' || c == ' ' || c == '.':
			b = append(b, '_')
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

func varRefFrom(v *netcdfio.Var) *VarRef {
	r := &VarRef{Var: v, Name: v.Name}
	if a, ok := v.Attrs["scale_factor"]; ok {
		if f, ok2 := a.Float(); ok2 {
			r.Scale, r.Packed = f, true
		}
	} else {
		r.Scale = 1
	}
	if a, ok := v.Attrs["add_offset"]; ok {
		if f, ok2 := a.Float(); ok2 {
			r.Add, r.Packed = f, true
		}
	}
	if a, ok := v.Attrs["_FillValue"]; ok {
		if f, ok2 := a.Float(); ok2 {
			r.FillValue, r.HasFill = f, true
		}
	}
	if a, ok := v.Attrs["missing_value"]; ok {
		if f, ok2 := a.Float(); ok2 {
			r.MissingValue, r.HasMissing = f, true
		}
	}
	if a, ok := v.Attrs["units"]; ok && a.IsChar {
		r.Units = strings.TrimSpace(a.Str)
	}
	if r.Scale == 0 && r.Packed {
		r.Scale = 1
	}
	return r
}

// DiscoverLayout maps ANDI variables in f to their roles. It returns a
// msdata-coded error when the file cannot be an ANDI/MS spectrum file at all.
func DiscoverLayout(f *netcdfio.File, d *msdata.Diagnostics) (*Layout, error) {
	taken := map[string]bool{}
	l := &Layout{InstrumentVars: map[string]*VarRef{}}
	l.Mass = resolveVar(f, taken, canonicalMass)
	l.Intensity = resolveVar(f, taken, canonicalIntensity)
	if l.Mass == nil || l.Intensity == nil {
		return nil, fmt.Errorf("%w: %s and %s are required (found variables: %s)",
			msdata.ErrSourceUnsupported, "mass_values", "intensity_values",
			strings.Join(f.VarNames(), ","))
	}
	l.TimeValues = resolveVar(f, taken, canonicalTimeValues)
	l.ScanIndex = resolveVar(f, taken, canonicalScanIndex)
	l.PointCount = resolveVar(f, taken, canonicalPointCount)
	l.RT = resolveVar(f, taken, canonicalRT)
	l.ScanNumber = resolveVar(f, taken, canonicalScanNumber)
	l.TIC = resolveVar(f, taken, canonicalTIC)
	l.MassMin = resolveVar(f, taken, canonicalMassMin)
	l.MassMax = resolveVar(f, taken, canonicalMassMax)
	l.TimeMin = resolveVar(f, taken, canonicalTimeMin)
	l.TimeMax = resolveVar(f, taken, canonicalTimeMax)
	l.ScanDur = resolveVar(f, taken, canonicalScanDur)
	l.InterScan = resolveVar(f, taken, canonicalInterScan)
	l.Resolution = resolveVar(f, taken, canonicalResolution)
	l.MSLevel = resolveVar(f, taken, canonicalMSLevel)
	l.Polarity = resolveVar(f, taken, canonicalPolarity)
	l.BasePeakMZ = resolveVar(f, taken, canonicalBasePeakMZ)
	l.BasePeakInt = resolveVar(f, taken, canonicalBasePeakInt)
	l.PrecursorMZ = resolveVar(f, taken, canonicalPrecursorMZ)
	l.PrecursorIn = resolveVar(f, taken, canonicalPrecursorIn)
	l.PrecursorZ = resolveVar(f, taken, canonicalPrecursorZ)
	l.CollisionE = resolveVar(f, taken, canonicalCollisionE)

	for _, name := range f.VarNames() {
		if taken[name] {
			continue
		}
		v, err := f.Var(name)
		if err != nil {
			continue
		}
		norm := normalizeKey(name)
		switch {
		case strings.HasPrefix(norm, "instrument_"):
			l.InstrumentVars[norm] = varRefFrom(v)
			taken[name] = true
		}
	}
	for _, name := range f.VarNames() {
		if !taken[name] {
			if v, err := f.Var(name); err == nil {
				l.Unmapped = append(l.Unmapped, varRefFrom(v))
			}
		}
	}

	for _, r := range []*VarRef{l.Mass, l.Intensity, l.TimeValues, l.ScanIndex, l.PointCount, l.RT} {
		if r != nil && r.Packed {
			d.WarnDetail(msdata.CodeANDIPackingApplied, 0, "var:"+r.Name,
				"variable %s is packed (scale_factor=%v add_offset=%v); values are unpacked on read",
				r.Name, r.Scale, r.Add)
		}
	}
	return l, nil
}
