// Package msdata holds the normalized mass-spectrometry domain model used by
// cdf2ms. It is deliberately free of any target-format (mzML/mzXML) or
// source-format (NetCDF/ANDI) concepts: a Run/Spectrum pair is produced by
// source readers and consumed by target writers.
//
// Scientific integrity rule: every field that asserts a scientific fact which
// may be absent from the source is represented as an optional value (pointer,
// or an explicit Unknown enum member). Readers must leave such fields unset
// rather than substituting a typical value.
package msdata

import (
	"fmt"
	"sort"
	"strings"
)

// Polarity describes the polarity of a scan. The zero value is Unknown: it is
// never serialized as an assertion.
type Polarity int

const (
	PolarityUnknown Polarity = iota
	PolarityPositive
	PolarityNegative
)

// String implements fmt.Stringer.
func (p Polarity) String() string {
	switch p {
	case PolarityPositive:
		return "positive"
	case PolarityNegative:
		return "negative"
	default:
		return "unknown"
	}
}

// Known reports whether the polarity was actually observed in the source.
func (p Polarity) Known() bool { return p != PolarityUnknown }

// CentroidState describes whether a spectrum is centroided or profile mode.
type CentroidState int

const (
	CentroidUnknown CentroidState = iota
	CentroidCentroided
	CentroidProfile
)

// String implements fmt.Stringer.
func (c CentroidState) String() string {
	switch c {
	case CentroidCentroided:
		return "centroid"
	case CentroidProfile:
		return "profile"
	default:
		return "unknown"
	}
}

// Known reports whether the centroid state was actually observed in the source.
func (c CentroidState) Known() bool { return c != CentroidUnknown }

// ValueKind classifies the underlying scalar type of a MetadataValue so that
// writers can emit faithful representations without string round-tripping.
type ValueKind int

const (
	KindString ValueKind = iota
	KindFloat64
	KindInt64
	KindBool
)

// MetadataValue is a single preserved source metadata entry.
//
// Source records where the value came from (variable, attribute, global
// attribute) so that provenance and debugging can explain every emitted field.
type MetadataValue struct {
	Kind ValueKind
	Str  string
	F    float64
	I    int64
	B    bool

	// Origin is a short machine-readable description of where the value was
	// read from, e.g. "global:test_ionization_mode" or "var:ms_level".
	Origin string
}

// String renders the value independent of kind (used by reports and CLI text).
func (v MetadataValue) String() string {
	switch v.Kind {
	case KindFloat64:
		return FormatFloat(v.F)
	case KindInt64:
		return fmt.Sprintf("%d", v.I)
	case KindBool:
		if v.B {
			return "true"
		}
		return "false"
	default:
		return v.Str
	}
}

// Float coerces the value to a float64 when possible.
func (v MetadataValue) Float() (float64, bool) {
	switch v.Kind {
	case KindFloat64:
		return v.F, true
	case KindInt64:
		return float64(v.I), true
	case KindString:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v.Str), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

// Int coerces the value to an int64 when possible.
func (v MetadataValue) Int() (int64, bool) {
	switch v.Kind {
	case KindInt64:
		return v.I, true
	case KindFloat64:
		if v.F == float64(int64(v.F)) {
			return int64(v.F), true
		}
	case KindString:
		var i int64
		if _, err := fmt.Sscanf(strings.TrimSpace(v.Str), "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}

// Metadata is an ordered-safe metadata container. Iteration is always sorted by
// key so that output is deterministic.
type Metadata map[string]MetadataValue

// SortedKeys returns metadata keys in lexical order.
func (m Metadata) SortedKeys() []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// SourceFile identifies the physical input file behind a Run.
type SourceFile struct {
	Path   string
	Name   string
	Size   int64
	SHA256 string
	// SHA1 is the SHA-1 hex digest of the source file. mzXML requires it
	// (parentFile/@fileSha1, exactly 40 characters), so converters compute it
	// alongside SHA256 in one pass.
	SHA1     string
	Encoding string // e.g. "CDF-1 (classic)", "CDF-2 (64-bit offset)", "HDF5/NetCDF4"
	Magic    string // hex first-4-bytes, useful for corruption triage
}

// Instrument is the (possibly partially known) instrument description.
// Empty strings mean "unknown" and must not be emitted as facts.
type Instrument struct {
	Name         string
	ID           string
	Manufacturer string
	Model        string
	SerialNumber string
	SoftwareVer  string
	FirmwareVer  string
	OSVersion    string
	AppVersion   string
	Comments     string

	// IonizationMode and DetectorType carry verbatim source strings (e.g.
	// "Electron Impact"); the writers map them to controlled vocabulary where
	// a confident mapping exists.
	IonizationMode  string
	IonizationPol   string
	DetectorType    string
	SeparationType  string
	MSInlet         string
	ResolutionType  string
	ScanFunction    string
	ScanDirection   string
	ScanLaw         string
	ExperimentType  string
	ExperimentTitle string
	Operator        string
	DataSetOrigin   string
}

// KnownFields returns the populated instrument fields in deterministic order.
func (in Instrument) KnownFields() []string {
	pairs := []struct {
		k, v string
	}{
		{"name", in.Name},
		{"id", in.ID},
		{"manufacturer", in.Manufacturer},
		{"model", in.Model},
		{"serial", in.SerialNumber},
		{"software", in.SoftwareVer},
		{"firmware", in.FirmwareVer},
		{"os", in.OSVersion},
		{"app", in.AppVersion},
		{"comments", in.Comments},
		{"ionization", in.IonizationMode},
		{"polarity", in.IonizationPol},
		{"detector", in.DetectorType},
		{"separation", in.SeparationType},
		{"inlet", in.MSInlet},
		{"resolution", in.ResolutionType},
		{"scan_function", in.ScanFunction},
		{"scan_direction", in.ScanDirection},
		{"scan_law", in.ScanLaw},
		{"experiment_type", in.ExperimentType},
		{"experiment_title", in.ExperimentTitle},
		{"operator", in.Operator},
		{"origin", in.DataSetOrigin},
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if strings.TrimSpace(p.v) != "" {
			out = append(out, p.k+"="+p.v)
		}
	}
	return out
}

// Run is a single acquisition: instrument context plus run-level metadata.
type Run struct {
	// ID is a stable, filesystem-safe identifier derived from the source.
	ID string

	Source     SourceFile
	Instrument Instrument

	// ScanCount is the number of spectra the source declares.
	ScanCount int
	// PointCount is the total number of declared profile/centroid points.
	PointCount int64

	// RetentionTimeUnit is the resolved canonical unit actually used after
	// normalization ("second"). RetentionTimeSourceUnit records what the
	// source claimed (or the policy applied).
	RetentionTimeUnit       string
	RetentionTimeSourceUnit string
	// RetentionTimeUnitOrigin explains how the unit was resolved, e.g.
	// "var:scan_acquisition_time.units" or "policy:seconds".
	RetentionTimeUnitOrigin string

	// SchemaFingerprint groups files with equivalent structure.
	SchemaFingerprint string
	// VendorFingerprint groups files by exporter/instrument characteristics.
	VendorFingerprint string

	// Metadata carries all preserved, unmapped source metadata.
	Metadata Metadata
}

// Spectrum is a single scan normalized to canonical units.
type Spectrum struct {
	// Index is the zero-based ordinal of the spectrum within the run.
	Index int
	// ScanNumber is the source scan number when present.
	ScanNumber *int64
	// ScanNumberOrigin documents where ScanNumber came from
	// ("var:actual_scan_number" or "derived:ordinal").
	ScanNumberOrigin string

	// RetentionTime is always in seconds (canonical internal unit).
	RetentionTime    float64
	RetentionTimeSet bool

	MSLevel      *int
	Polarity     Polarity
	Centroid     CentroidState
	PrecursorMZ  *float64
	PrecursorInt *float64
	PrecursorZ   *int
	CollisionEn  *float64

	// MZ and Intensity are the peak arrays, always float64 internally.
	MZ        []float64
	Intensity []float64

	// Optional per-scan summary values, only set when present in the source.
	TIC           *float64
	BasePeakMZ    *float64
	BasePeakInt   *float64
	LowestMZ      *float64
	HighestMZ     *float64
	ScanDuration  *float64
	InterScanTime *float64
	Resolution    *float64

	// Metadata holds per-scan preserved metadata that has no mapped home.
	Metadata Metadata
}

// Points returns the number of peaks in the spectrum.
func (s *Spectrum) Points() int {
	if len(s.MZ) < len(s.Intensity) {
		return len(s.MZ)
	}
	return len(s.Intensity)
}

// DerivedStats are statistics computed from the arrays themselves. They are
// used for Level-3 validation only and are never presented as source facts.
type DerivedStats struct {
	PointCount     int
	SumIntensity   float64
	MaxIntensity   float64
	MaxIntensityMZ float64
	MinMZ          float64
	MaxMZ          float64
}

// Derive computes statistics over the spectrum arrays.
func (s *Spectrum) Derive() DerivedStats {
	var d DerivedStats
	d.PointCount = len(s.MZ)
	for i, mz := range s.MZ {
		if i == 0 || mz < d.MinMZ {
			d.MinMZ = mz
		}
		if i == 0 || mz > d.MaxMZ {
			d.MaxMZ = mz
		}
	}
	n := len(s.Intensity)
	if n > 0 {
		d.MaxIntensity = s.Intensity[0]
		d.MaxIntensityMZ = s.MZ[0]
	}
	for i, v := range s.Intensity {
		d.SumIntensity += v
		if v > d.MaxIntensity {
			d.MaxIntensity = v
			if i < len(s.MZ) {
				d.MaxIntensityMZ = s.MZ[i]
			}
		}
	}
	return d
}

// FormatFloat renders a float64 deterministically with enough digits to
// round-trip (Go's 'g' with -1 precision is shortest-representable).
func FormatFloat(v float64) string {
	return strconvFTOG(v)
}
