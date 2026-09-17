package andiio

import (
	"fmt"
	"math"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// ScanPlan derives, for every scan, where its peak array starts and how long it
// is. ANDI/MS encodes this either explicitly (scan_index + point_count), or with
// only one of the two, and vendors ship both 0-based and 1-based scan_index.
//
// The plan is validated as it is built: contiguity, bounds, and the total-point
// identity sum(point_count) == len(mass_values) are all checked, and violations
// are reported with stable codes instead of being clamped away.
type ScanPlan struct {
	// Scans is the number of spectra the file claims.
	Scans int
	// TotalPoints is the peak-array extent (records in the point dimension).
	TotalPoints int64

	// Start and Count are parallel arrays of length Scans.
	Start []int64
	Count []int32

	// IndexBase records the offset subtracted from scan_index (0 or 1).
	IndexBase int64
	// Source records how the plan was derived, for provenance.
	Source string

	// maxHint bounds a single scan's peak count (mirrors PlanOptions).
	maxHint int64

	// ClippedScans counts scans whose declared span exceeded the file extent.
	ClippedScans int
	// EmptyScans counts zero-point scans.
	EmptyScans int
}

// PlanOptions tunes plan derivation.
type PlanOptions struct {
	// MaxScanPoints bounds a single scan's peak count. A scan claiming more is
	// treated as corrupt (a hostile or broken header) rather than allocating it.
	MaxScanPoints int64
	// MaxTotalBytes bounds the memory the plan arrays may use.
	MaxTotalBytes int64
}

// DefaultPlanOptions bounds memory at 256 MiB for plan arrays and 8M points for
// a single scan (8M points is ~128 MiB of float64, already extreme for MS).
func DefaultPlanOptions() PlanOptions {
	return PlanOptions{MaxScanPoints: 8_000_000, MaxTotalBytes: 256 << 20}
}

// DerivePlan builds the scan plan.
//
// scanIndex and pointCount may be nil. baseHint is the declared scan count (from
// the scan_number dimension) or 0 when unavailable.
func DerivePlan(scanIndex, pointCount []int64, hasIndex, hasCount bool, totalPoints int64,
	baseHint int, opts PlanOptions, d *msdata.Diagnostics) (*ScanPlan, error) {

	if opts.MaxScanPoints == 0 {
		opts = DefaultPlanOptions()
	}
	scans := baseHint
	if hasIndex && len(scanIndex) > scans {
		scans = len(scanIndex)
	}
	if hasCount && len(pointCount) > scans {
		scans = len(pointCount)
	}
	if scans <= 0 {
		return nil, fmt.Errorf("%w: cannot determine the number of scans: neither scan_index nor "+
			"point_count is present and the scan dimension is unknown", msdata.ErrSourceUnsupported)
	}
	if int64(scans)*12 > opts.MaxTotalBytes {
		return nil, fmt.Errorf("%w: %d scans would need %d bytes of index memory (limit %d)",
			msdata.ErrFileTooLarge, scans, int64(scans)*12, opts.MaxTotalBytes)
	}

	p := &ScanPlan{Scans: scans, TotalPoints: totalPoints, maxHint: opts.MaxScanPoints,
		Start: make([]int64, scans), Count: make([]int32, scans)}

	switch {
	case hasIndex && hasCount:
		p.Source = "var:scan_index+var:point_count"
		if err := p.fromIndexAndCount(scanIndex, pointCount, totalPoints, d); err != nil {
			return nil, err
		}
	case hasCount:
		p.Source = "derived:prefix-sum(point_count)"
		var acc int64
		for i := 0; i < scans; i++ {
			c := pointCount[i]
			if c < 0 {
				d.WarnDetail(msdata.CodeANDINNegativePointCount, i, fmt.Sprintf("point_count[%d]=%d", i, c),
					"scan %d has negative point_count %d; treated as empty", i, c)
				c = 0
			}
			if c > opts.MaxScanPoints {
				return nil, fmt.Errorf("%w: scan %d claims %d points (limit %d)",
					msdata.ErrCorruptSource, i, c, opts.MaxScanPoints)
			}
			p.Start[i] = acc
			p.Count[i] = int32(c)
			acc += c
		}
	case hasIndex:
		p.Source = "derived:diff(scan_index)"
		if err := p.fromIndexOnly(scanIndex, totalPoints, d); err != nil {
			return nil, err
		}
	default:
		// No index and no point count. If the peak arrays are exactly one point
		// per scan the file is a single-point "spectrum list"; anything else is
		// undecodable without guessing, which we refuse to do.
		if totalPoints == int64(scans) {
			p.Source = "derived:one-point-per-scan"
			for i := 0; i < scans; i++ {
				p.Start[i] = int64(i)
				p.Count[i] = 1
			}
			d.Warn(msdata.CodeANDIScanLayoutAssumed, 0,
				"neither scan_index nor point_count is present; assumed exactly one peak per scan "+
					"because the peak arrays hold exactly %d values", totalPoints)
			return p, nil
		}
		return nil, fmt.Errorf("%w: the file has neither scan_index nor point_count, so peaks cannot be "+
			"partitioned into spectra (%d peaks, %d scans)", msdata.ErrSourceUnsupported, totalPoints, scans)
	}

	// Total-point identity.
	var sum int64
	for i := 0; i < scans; i++ {
		sum += int64(p.Count[i])
		if p.Count[i] == 0 {
			p.EmptyScans++
		}
	}
	if sum != totalPoints {
		diff := totalPoints - sum
		abs := diff
		if abs < 0 {
			abs = -abs
		}
		if abs > totalPoints/100 && abs > 1024 {
			return nil, fmt.Errorf("%w: point_count sums to %d but the peak arrays hold %d values "+
				"(difference %d); refusing to convert a file whose scan layout does not match its data",
				msdata.ErrCorruptSource, sum, totalPoints, diff)
		}
		d.WarnDetail(msdata.CodeANDIPointCountMismatch, 0,
			fmt.Sprintf("sum(point_count)=%d peak_extent=%d delta=%d", sum, totalPoints, diff),
			"declared peak counts sum to %d but the peak arrays hold %d values (%+d); trailing scans may be "+
				"truncated in the source", sum, totalPoints, diff)
	}
	return p, nil
}

func (p *ScanPlan) fromIndexAndCount(scanIndex, pointCount []int64, totalPoints int64, d *msdata.Diagnostics) error {
	n := p.Scans
	base := scanIndex[0]
	if base != 0 && base != 1 {
		// Some exporters start scan_index at the first point of the *second*
		// scan, or carry an opaque origin. Use the observed first value as the
		// base but say so loudly.
		d.WarnDetail(msdata.CodeANDIScanIndexBase, 0, fmt.Sprintf("scan_index[0]=%d", base),
			"scan_index starts at %d (neither 0 nor 1); using it as the origin offset", base)
	} else if base == 1 {
		d.WarnDetail(msdata.CodeANDIScanIndexBase, 0, "scan_index[0]=1",
			"scan_index is 1-based; subtracting 1 to obtain zero-based peak offsets")
	}
	p.IndexBase = base

	for i := 0; i < n; i++ {
		idx := scanIndex[i] - base
		if idx < 0 {
			return fmt.Errorf("%w: scan_index[%d]=%d is below the observed base %d",
				msdata.ErrCorruptSource, i, scanIndex[i], base)
		}
		c := pointCount[i]
		if c < 0 {
			d.WarnDetail(msdata.CodeANDINNegativePointCount, i, fmt.Sprintf("point_count=%d", c),
				"scan %d has negative point_count %d; treated as empty", i, c)
			c = 0
		}
		if c > p.MaxScanPointsHint() {
			return fmt.Errorf("%w: scan %d claims %d points (limit %d)",
				msdata.ErrCorruptSource, i, c, p.MaxScanPointsHint())
		}
		if idx+c > totalPoints {
			// Trailing or truncated region: clamp, but never silently.
			avail := totalPoints - idx
			if avail < 0 {
				avail = 0
			}
			d.WarnDetail(msdata.CodeSourceTruncated, i,
				fmt.Sprintf("scan_index=%d point_count=%d peak_extent=%d", idx, c, totalPoints),
				"scan %d claims %d points at offset %d but only %d remain; clamped to %d",
				i, c, idx, avail, avail)
			p.ClippedScans++
			c = avail
		}
		if i > 0 {
			prevEnd := p.Start[i-1] + int64(p.Count[i-1])
			if idx != prevEnd {
				if idx < prevEnd {
					return fmt.Errorf("%w: scan_index[%d]=%d overlaps the previous scan (ends at %d)",
						msdata.ErrCorruptSource, i, idx, prevEnd)
				}
				d.WarnDetail(msdata.CodeANDIScanIndexNotMono, i,
					fmt.Sprintf("scan_index[%d]=%d previous_end=%d gap=%d", i, idx, prevEnd, idx-prevEnd),
					"gap of %d peaks between scan %d and scan %d; %d peaks are unreadable",
					idx-prevEnd, i-1, i, idx-prevEnd)
			}
		}
		p.Start[i] = idx
		p.Count[i] = int32(c)
	}
	return nil
}

func (p *ScanPlan) fromIndexOnly(scanIndex []int64, totalPoints int64, d *msdata.Diagnostics) error {
	base := scanIndex[0]
	p.IndexBase = base
	if base != 0 {
		d.WarnDetail(msdata.CodeANDIScanIndexBase, 0, fmt.Sprintf("scan_index[0]=%d", base),
			"point_count is absent and scan_index starts at %d; using it as the origin offset", base)
	}
	n := p.Scans
	if n > len(scanIndex) {
		n = len(scanIndex)
	}
	for i := 0; i < n-1; i++ {
		idx := scanIndex[i] - base
		next := scanIndex[i+1] - base
		c := next - idx
		if c < 0 {
			return fmt.Errorf("%w: scan_index decreases between scans %d and %d (%d -> %d)",
				msdata.ErrCorruptSource, i, i+1, scanIndex[i], scanIndex[i+1])
		}
		if c > p.MaxScanPointsHint() {
			return fmt.Errorf("%w: derived point count %d for scan %d exceeds the limit %d",
				msdata.ErrCorruptSource, c, i, p.MaxScanPointsHint())
		}
		p.Start[i] = idx
		p.Count[i] = int32(c)
	}
	last := scanIndex[n-1] - base
	c := totalPoints - last
	if c < 0 {
		return fmt.Errorf("%w: scan_index[%d]=%d lies past the end of the peak arrays (%d)",
			msdata.ErrCorruptSource, n-1, scanIndex[n-1], totalPoints)
	}
	if c > p.MaxScanPointsHint() {
		return fmt.Errorf("%w: derived point count %d for the final scan %d exceeds the limit %d",
			msdata.ErrCorruptSource, c, n-1, p.MaxScanPointsHint())
	}
	p.Start[n-1] = last
	p.Count[n-1] = int32(c)
	d.WarnDetail(msdata.CodeANDIPointCountDerived, 0, "point_count absent",
		"point_count is absent; per-scan peak counts were derived from scan_index differences")
	return nil
}

// MaxScanPointsHint returns the per-scan peak-count bound used while deriving.
func (p *ScanPlan) MaxScanPointsHint() int64 { return p.maxHint }

// EmptyFraction is the share of zero-point scans.
func (p *ScanPlan) EmptyFraction() float64 {
	if p.Scans == 0 {
		return 0
	}
	return float64(p.EmptyScans) / float64(p.Scans)
}

// SanityReport renders a one-line summary for audit output.
func (p *ScanPlan) SanityReport() string {
	mean := 0.0
	if p.Scans > 0 {
		mean = float64(p.TotalPoints) / float64(p.Scans)
	}
	maxC := int32(0)
	for _, c := range p.Count {
		if c > maxC {
			maxC = c
		}
	}
	return fmt.Sprintf("scans=%d points=%d mean_points=%.1f max_points=%d empty=%d index_base=%d source=%s",
		p.Scans, p.TotalPoints, mean, maxC, p.EmptyScans, p.IndexBase, p.Source)
}

// ValidateArrays checks the peak arrays of one scan against the plan and the
// source-declared ranges. It returns corrected values only when the correction
// is unambiguous (e.g. dropping trailing fill), and reports everything.
type ArrayReport struct {
	DroppedFill    int
	NonFinite      int
	NegativeInt    int
	Unsorted       int
	MinMZ          float64
	MaxMZ          float64
	SumIntensity   float64
	MaxIntensity   float64
	MaxIntensityMZ float64
}

// SanitizeArrays removes fill/non-finite entries and reports what it removed.
//
// mzML and mzXML cannot represent a NaN m/z, and silently keeping one would
// corrupt the output document; silently dropping it would change the spectrum.
// Both are unacceptable, so the drop is counted and surfaced as a diagnostic by
// the caller.
func SanitizeArrays(mz, inten []float64, mzRef, intenRef *VarRef, scan int, d *msdata.Diagnostics) ([]float64, []float64, ArrayReport) {
	r := ArrayReport{}
	outMZ := mz[:0]
	outIn := inten[:0]
	for i := range mz {
		z := mz[i]
		var y float64
		if i < len(inten) {
			y = inten[i]
		}
		if math.IsNaN(z) || math.IsInf(z, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
			r.NonFinite++
			continue
		}
		if (mzRef != nil && mzRef.IsFill(z)) || (intenRef != nil && intenRef.IsFill(y)) {
			r.DroppedFill++
			continue
		}
		if y < 0 {
			r.NegativeInt++
		}
		if len(outMZ) > 0 && z < outMZ[len(outMZ)-1] {
			r.Unsorted++
		}
		outMZ = append(outMZ, z)
		outIn = append(outIn, y)
		if len(outMZ) == 1 {
			r.MinMZ, r.MaxMZ = z, z
		} else {
			if z < r.MinMZ {
				r.MinMZ = z
			}
			if z > r.MaxMZ {
				r.MaxMZ = z
			}
		}
		r.SumIntensity += y
		if y > r.MaxIntensity {
			r.MaxIntensity = y
			r.MaxIntensityMZ = z
		}
	}
	if r.NonFinite+r.DroppedFill > 0 {
		d.WarnDetail(msdata.CodeANDIFFillValues, scan,
			fmt.Sprintf("dropped_fill=%d non_finite=%d", r.DroppedFill, r.NonFinite),
			"scan %d: dropped %d fill and %d non-finite points from the peak arrays",
			scan, r.DroppedFill, r.NonFinite)
	}
	if r.NegativeInt > 0 {
		d.WarnDetail(msdata.CodeANDIInvalidNumeric, scan, fmt.Sprintf("negative_intensity=%d", r.NegativeInt),
			"scan %d: %d negative intensity values preserved as-is", scan, r.NegativeInt)
	}
	if r.Unsorted > 0 {
		d.WarnDetail(msdata.CodeANDIInvalidNumeric, scan, fmt.Sprintf("out_of_order=%d", r.Unsorted),
			"scan %d: %d m/z values are not in increasing order (preserved in source order)", scan, r.Unsorted)
	}
	return outMZ, outIn, r
}
