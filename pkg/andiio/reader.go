package andiio

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/netcdfio"
)

// Options controls reader behaviour. Every knob that can change scientific
// output is explicit and appears in the report.
type Options struct {
	// RTUnit decides how scan_acquisition_time's unit is resolved.
	RTUnit RTUnitPolicy
	// Plan bounds scan-plan derivation.
	Plan PlanOptions
	// IncludeTimeValues reads the point-level time_values array when present
	// (it is almost always fill values, so it is off by default).
	IncludeTimeValues bool
	// HeaderLimits bounds NetCDF header parsing.
	HeaderLimits netcdfio.Limits
}

// DefaultOptions returns the production defaults.
func DefaultOptions() Options {
	return Options{RTUnit: RTPolicyAuto, Plan: DefaultPlanOptions(), HeaderLimits: netcdfio.DefaultLimits()}
}

// Reader streams an ANDI/MS file as normalized spectra.
type Reader struct {
	path   string
	f      *netcdfio.File
	layout *Layout
	plan   *ScanPlan
	run    *msdata.Run
	diag   *msdata.Diagnostics
	peaks  *peakReader
	opts   Options
	rtUnit RTUnit
	src    msdata.SourceFile

	defMSLevel  int
	msLevelOrig string
	defPol      msdata.Polarity
	polOrigin   string
	defCent     msdata.CentroidState
	centOrig    string

	// aux is the per-scan metadata block currently held in memory.
	aux      *auxBlock
	nextScan int
	stats    msdata.ReaderStats

	mzBuf  []float64
	intBuf []float64
	spec   msdata.Spectrum
}

// auxBlock holds a contiguous block of per-scan arrays. Blocks are the unit of
// memory bounding: the reader only ever holds one block plus one spectrum.
type auxBlock struct {
	base  int
	count int

	rt         []float64
	hasRT      bool
	scanNumber []int64
	tic        []float64
	massMin    []float64
	massMax    []float64
	scanDur    []float64
	interScan  []float64
	resolution []float64
	msLevel    []int64
	polarity   []float64
	baseMZ     []float64
	baseInt    []float64
	preMZ      []float64
	preInt     []float64
	preZ       []int64
	colEnergy  []float64
}

// auxBlockScans is the number of scans whose per-scan arrays are held at once.
// 8192 scans * 13 float64 arrays is ~850 KiB — negligible, and it keeps the
// number of syscalls proportional to file size rather than scan count.
const auxBlockScans = 8192

// Open opens an ANDI/MS netCDF file.
func Open(path string, opts Options) (*Reader, error) {
	// Fill defaults field by field. Replacing the struct wholesale would silently
	// discard an explicit --rt-unit whenever the caller left HeaderLimits unset.
	if opts.HeaderLimits.MaxVariables == 0 {
		opts.HeaderLimits = netcdfio.DefaultLimits()
	}
	if opts.Plan.MaxScanPoints == 0 {
		opts.Plan = DefaultPlanOptions()
	}
	if opts.RTUnit == "" {
		opts.RTUnit = RTPolicyAuto
	}
	d := &msdata.Diagnostics{}
	enc, magic, derr := netcdfio.Detect(path)
	if derr != nil {
		d.Fail(msdata.CodeNCOpenFailed, 0, "%v", derr)
		return nil, msdata.WrapError(msdata.CodeNCOpenFailed, derr, "cannot inspect %s", path)
	}
	if enc == netcdfio.EncodingHDF5 {
		d.Fail(msdata.CodeNCUnsupportedEncoding, 0, "%s is an HDF5-backed NetCDF-4 file", filepath.Base(path))
		return nil, msdata.WrapError(msdata.CodeNCUnsupportedEncoding,
			netcdfio.ErrUnsupportedEncoding,
			"%s is HDF5-backed NetCDF-4; cdf2ms reads classic CDF-1/2/5 only (see docs/compatibility.md)",
			filepath.Base(path))
	}
	f, err := netcdfio.OpenWithLimits(path, opts.HeaderLimits)
	if err != nil {
		code := msdata.CodeNCHeaderCorrupt
		if errors.Is(err, netcdfio.ErrShortFile) {
			code = msdata.CodeSourceTruncated
		}
		d.Fail(code, 0, "%v", err)
		return nil, msdata.WrapError(code, err, "cannot open %s", path)
	}
	size := f.FileSize()
	src := msdata.SourceFile{
		Path:     path,
		Name:     filepath.Base(path),
		Size:     size,
		Encoding: enc.String(),
		Magic:    magic,
	}
	r := &Reader{path: path, f: f, opts: opts, diag: d, src: src}

	layout, lerr := DiscoverLayout(f, d)
	if lerr != nil {
		f.Close()
		if msdata.CodeOf(lerr) != "" {
			d.Fail(msdata.CodeANDIRequiredVarMissing, 0, "%v", lerr)
		} else {
			d.Fail(msdata.CodeANDIRequiredVarMissing, 0, "%v", lerr)
		}
		return nil, lerr
	}
	r.layout = layout
	r.peaks = newPeakReader(f, layout)

	if f.Truncated {
		d.WarnDetail(msdata.CodeSourceTruncated, 0, f.TruncDetail,
			"%s: %s", filepath.Base(path), f.TruncDetail)
	}

	if err := r.resolveRTUnit(); err != nil {
		f.Close()
		return nil, err
	}
	if err := r.buildPlan(); err != nil {
		f.Close()
		return nil, err
	}
	r.deriveScanConstants()
	r.run = BuildRunMetadata(f, layout, r.rtUnit, r.plan, src, d)
	r.applyDerivedRunFields()
	return r, nil
}

// resolveRTUnit samples acquisition times and resolves the unit.
func (r *Reader) resolveRTUnit() error {
	if r.layout.RT == nil {
		r.diag.WarnDetail(msdata.CodeANDIMissingRetentionTime, 0, "scan_acquisition_time absent",
			"%s declares no scan_acquisition_time variable; spectra will carry no retention time",
			r.src.Name)
		r.rtUnit = RTUnit{Name: "second", Scale: 1, Origin: "none:no-retention-time", Confidence: "explicit"}
		return nil
	}
	sample, err := r.sampleRT()
	if err != nil {
		return err
	}
	tvUnits := ""
	if r.layout.TimeValues != nil {
		tvUnits = r.layout.TimeValues.Units
	}
	res, err := ResolveRTUnit(r.opts.RTUnit, r.layout.RT, tvUnits, globalTimeUnits(r.f), sample)
	if err != nil {
		// A units attribute we cannot read is a different problem from a file that
		// states no usable unit at all: the first is malformed, the second merely
		// undecidable, and the operator's next step differs.
		code := msdata.CodeANDIUnknownRTUnit
		if errors.Is(err, msdata.ErrUnitUndetermined) {
			code = msdata.CodeANDIAmbiguousUnits
		}
		r.diag.Fail(code, 0, "%v", err)
		return msdata.WrapError(code, err, "%s", r.src.Name)
	}
	for _, w := range res.Warnings {
		r.diag.WarnDetail(w.Code, 0, w.Detail, "%s", w.Message)
	}
	r.rtUnit = res.Unit
	return nil
}

// globalTimeUnits lifts the file-global attributes that can state a time unit
// into plain strings, so unit resolution stays a pure function of text and does
// not need the container.
func globalTimeUnits(f *netcdfio.File) map[string]string {
	out := make(map[string]string, len(GlobalTimeUnitKeys))
	for _, k := range GlobalTimeUnitKeys {
		if a, ok := f.Globals[k]; ok && a.IsChar {
			out[k] = a.Str
		}
	}
	return out
}

// sampleRT reads a bounded, strided sample of acquisition times for the
// magnitude test (never the whole array).
func (r *Reader) sampleRT() ([]float64, error) {
	v := r.layout.RT.Var
	ext := v.Extent()
	if ext <= 0 {
		return nil, nil
	}
	const sampleN = 256
	var out []float64
	readRun := func(start, n int64) error {
		if n <= 0 {
			return nil
		}
		vals, err := r.f.ReadFloats(v, start, n, nil)
		if err != nil {
			r.diag.WarnDetail(msdata.CodeNCReadFailed, 0, "var:"+v.Name,
				"cannot read scan_acquisition_time sample: %v", err)
			return nil
		}
		applyScale(vals, r.layout.RT)
		out = append(out, vals...)
		return nil
	}
	if ext <= sampleN*2 {
		if err := readRun(0, ext); err != nil {
			return nil, err
		}
		return out, nil
	}
	if err := readRun(0, sampleN); err != nil {
		return nil, err
	}
	if err := readRun(ext/2, sampleN); err != nil {
		return nil, err
	}
	if err := readRun(ext-sampleN, sampleN); err != nil {
		return nil, err
	}
	return out, nil
}

// buildPlan reads the scan index arrays and derives the scan plan.
func (r *Reader) buildPlan() error {
	l := r.layout
	declaredScans := 0
	if n, ok := r.f.DimSize("scan_number"); ok {
		declaredScans = int(n)
	}
	peakExtent := l.Mass.Var.Extent()

	readIndex := func(ref *VarRef) ([]int64, error) {
		if ref == nil {
			return nil, nil
		}
		n := ref.Var.Extent()
		if declaredScans > 0 && n > int64(declaredScans) {
			n = int64(declaredScans)
		}
		const chunk int64 = 1 << 16
		out := make([]int64, 0, n)
		for off := int64(0); off < n; off += chunk {
			m := chunk
			if off+m > n {
				m = n - off
			}
			v, err := r.f.ReadInts(ref.Var, off, m)
			if err != nil {
				return nil, msdata.WrapError(msdata.CodeNCReadFailed, err,
					"reading %s at index %d", ref.Name, off)
			}
			out = append(out, v...)
			r.stats.IndexArrays += m
		}
		return out, nil
	}

	scanIndex, err := readIndex(l.ScanIndex)
	if err != nil {
		return err
	}
	pointCount, err := readIndex(l.PointCount)
	if err != nil {
		return err
	}
	plan, err := DerivePlan(scanIndex, pointCount, l.ScanIndex != nil, l.PointCount != nil,
		peakExtent, declaredScans, r.opts.Plan, r.diag)
	if err != nil {
		r.diag.Fail(msdata.CodeANDIInvalidScanIndex, 0, "%v", err)
		return err
	}
	r.plan = plan
	if l.Intensity.Var.Extent() != peakExtent {
		r.diag.WarnDetail(msdata.CodeANDIMassIntensityLenMis, 0,
			fmt.Sprintf("mass_values=%d intensity_values=%d", peakExtent, l.Intensity.Var.Extent()),
			"mass_values holds %d values but intensity_values holds %d; the shorter length wins",
			peakExtent, l.Intensity.Var.Extent())
		if l.Intensity.Var.Extent() < peakExtent {
			r.plan.TotalPoints = l.Intensity.Var.Extent()
		}
	}
	return nil
}

// deriveScanConstants resolves run-level MS level, polarity and centroid state.
func (r *Reader) deriveScanConstants() {
	l := r.layout
	r.defMSLevel = 0
	switch {
	case l.MSLevel != nil:
		r.msLevelOrig = "var:" + l.MSLevel.Name
	case l.PrecursorMZ != nil:
		// A precursor array exists: scans with a positive precursor are MS2.
		r.msLevelOrig = "derived:precursor_presence"
		r.defMSLevel = 1
		r.diag.WarnDetail(msdata.CodeANDIMSLevelUnknown, 0, "ms_level absent, precursor array present",
			"ms_level is absent; scans carrying a precursor m/z are labelled MS2, the rest MS1")
	default:
		r.defMSLevel = 1
		r.msLevelOrig = "derived:no_precursor_variables"
		r.diag.WarnDetail(msdata.CodeANDIMSLevelUnknown, 0, "ms_level and precursor arrays absent",
			"ms_level is absent and no precursor variables exist; every scan is labelled MS1 "+
				"(single-quadrupole acquisition). Recorded in output provenance.")
	}

	r.defPol = msdata.PolarityUnknown
	if l.Polarity != nil {
		r.polOrigin = "var:" + l.Polarity.Name
	} else if a, ok := r.f.Globals["test_ionization_polarity"]; ok {
		r.defPol, r.polOrigin = DerivePolarity(a, 0, false)
		if r.defPol.Known() {
			r.diag.WarnDetail(msdata.CodeANDIPolarityUnknown, 0, r.polOrigin,
				"per-scan polarity is absent; using run-level test_ionization_polarity=%q for every scan",
				strings.TrimSpace(a.Str))
		}
	}
	if !r.defPol.Known() && l.Polarity == nil {
		r.diag.WarnDetail(msdata.CodeANDIPolarityUnknown, 0, "no polarity evidence",
			"no polarity information exists in the source; polarity is omitted rather than assumed")
	}

	cvals := make([]msdata.MetadataValue, 0, 4)
	for _, name := range []string{"test_ms_data_type", "experiment_type", "ms_scan_type", "test_ms_centroiding", "ms_data_type"} {
		if a, ok := r.f.Globals[name]; ok {
			cvals = append(cvals, msdata.MetadataValue{
				Kind: msdata.KindString, Str: sanitizeText(attrRawText(a)), Origin: "global:" + name})
		}
	}
	r.defCent, r.centOrig = DeriveCentroid(cvals...)
	if !r.defCent.Known() {
		r.diag.WarnDetail(msdata.CodeANDICentroidUnknown, 0, "no centroid evidence",
			"centroid/profile state is not stated by the source; it is omitted from the output "+
				"instead of being inferred from peak spacing")
	}
}

// applyDerivedRunFields records provenance for run-level policy decisions.
func (r *Reader) applyDerivedRunFields() {
	if r.run == nil {
		return
	}
	r.run.Metadata["derived:retention_time_unit"] = msdata.MetadataValue{
		Kind: msdata.KindString, Str: r.rtUnit.Name, Origin: r.rtUnit.Origin}
	r.run.Metadata["derived:ms_level"] = msdata.MetadataValue{
		Kind: msdata.KindString, Str: fmt.Sprintf("%d", r.defMSLevel), Origin: r.msLevelOrig}
	if r.defPol.Known() {
		r.run.Metadata["derived:polarity"] = msdata.MetadataValue{
			Kind: msdata.KindString, Str: r.defPol.String(), Origin: r.polOrigin}
	}
	if r.defCent.Known() {
		r.run.Metadata["derived:centroid"] = msdata.MetadataValue{
			Kind: msdata.KindString, Str: r.defCent.String(), Origin: r.centOrig}
	}
	r.run.Metadata["derived:scan_plan"] = msdata.MetadataValue{
		Kind: msdata.KindString, Str: r.plan.SanityReport(), Origin: r.plan.Source}
}

// Metadata returns the run metadata (cached after first construction).
func (r *Reader) Metadata(ctx context.Context) (*msdata.Run, error) {
	return r.run, nil
}

// Diagnostics returns the reader's diagnostics.
func (r *Reader) Diagnostics() *msdata.Diagnostics { return r.diag }

// Stats returns reader counters.
func (r *Reader) Stats() msdata.ReaderStats { return r.stats }

// Layout exposes the resolved variable map (used by the auditor).
func (r *Reader) Layout() *Layout { return r.layout }

// Plan exposes the derived scan plan (used by the auditor).
func (r *Reader) Plan() *ScanPlan { return r.plan }

// RTUnit exposes the resolved retention-time unit.
func (r *Reader) RTUnit() RTUnit { return r.rtUnit }

// SourceFile exposes source container facts.
func (r *Reader) SourceFile() msdata.SourceFile { return r.src }

// Close releases the file.
func (r *Reader) Close() error {
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// Next returns the next spectrum, or msdata.ErrEndOfData.
//
// The returned spectrum (and its arrays) is owned by the reader and stays valid
// only until the next call.
func (r *Reader) Next(ctx context.Context) (*msdata.Spectrum, error) {
	if r.nextScan >= r.plan.Scans {
		return nil, msdata.ErrEndOfData
	}
	if r.nextScan%512 == 0 {
		if err := ctx.Err(); err != nil {
			return nil, msdata.WrapError(msdata.CodeCancelled, err, "conversion cancelled")
		}
	}
	i := r.nextScan
	if err := r.ensureAux(i); err != nil {
		return nil, err
	}
	s := &r.spec
	*s = msdata.Spectrum{Index: i, Metadata: nil}

	start := r.plan.Start[i]
	count := int64(r.plan.Count[i])
	if lim := r.plan.TotalPoints - start; count > lim {
		count = lim
	}
	if count < 0 {
		count = 0
	}
	if count == 0 {
		r.diag.WarnDetail(msdata.CodeANDIZeroPointScan, i,
			fmt.Sprintf("scan %d has point_count 0", i),
			"scan %d has zero peaks; emitted as an empty spectrum", i)
	}

	mz, inten, err := r.peaks.Read(start, count, r.mzBuf, r.intBuf)
	if err != nil {
		if errors.Is(err, netcdfio.ErrShortFile) {
			r.diag.WarnDetail(msdata.CodeSourceUnreadablePoints, i, fmt.Sprintf("start=%d count=%d", start, count),
				"scan %d: cannot read %d peaks at offset %d (source truncated); emitted as empty",
				i, count, start)
			mz, inten = mz[:0], inten[:0]
		} else {
			r.diag.Fail(msdata.CodeNCReadFailed, i, "scan %d: %v", i, err)
			return nil, err
		}
	}
	r.mzBuf, r.intBuf = mz, inten
	mz, inten, _ = SanitizeArrays(mz, inten, r.layout.Mass, r.layout.Intensity, i, r.diag)
	s.MZ = mz
	s.Intensity = inten

	// retention time
	if r.aux.hasRT && i-r.aux.base < len(r.aux.rt) {
		v := r.aux.rt[i-r.aux.base]
		if !r.layout.RT.IsFill(v) {
			s.RetentionTime = v * r.rtUnit.Scale
			s.RetentionTimeSet = true
		} else {
			r.diag.WarnDetail(msdata.CodeANDIMissingRetentionTime, i,
				fmt.Sprintf("scan_acquisition_time[%d] is fill", i),
				"scan %d: acquisition time is a fill value; retention time omitted", i)
		}
	}

	// scan number
	if r.aux.scanNumber != nil && i-r.aux.base < len(r.aux.scanNumber) {
		n := r.aux.scanNumber[i-r.aux.base]
		s.ScanNumber = &n
		s.ScanNumberOrigin = "var:" + r.layout.ScanNumber.Name
	} else {
		derived := int64(i + 1)
		s.ScanNumber = &derived
		s.ScanNumberOrigin = "derived:ordinal"
	}

	// MS level
	switch {
	case r.aux.msLevel != nil && i-r.aux.base < len(r.aux.msLevel):
		lvl := int(r.aux.msLevel[i-r.aux.base])
		if lvl >= 1 && lvl <= 10 {
			s.MSLevel = &lvl
		} else {
			lvl := r.defMSLevel
			s.MSLevel = &lvl
			r.diag.WarnDetail(msdata.CodeANDIMSLevelUnknown, i,
				fmt.Sprintf("ms_level=%d", lvl), "scan %d: ms_level %d is out of range; used %d",
				i, int(r.aux.msLevel[i-r.aux.base]), lvl)
		}
	case r.layout.PrecursorMZ != nil && r.aux.preMZ != nil && i-r.aux.base < len(r.aux.preMZ):
		pmz := r.aux.preMZ[i-r.aux.base]
		lvl := 1
		if pmz > 0 && !r.layout.PrecursorMZ.IsFill(pmz) {
			lvl = 2
		}
		s.MSLevel = &lvl
	default:
		lvl := r.defMSLevel
		s.MSLevel = &lvl
	}

	// polarity
	if r.aux.polarity != nil && i-r.aux.base < len(r.aux.polarity) {
		p, _ := DerivePolarity(netcdfio.Attribute{}, r.aux.polarity[i-r.aux.base], true)
		s.Polarity = p
	} else {
		s.Polarity = r.defPol
	}
	s.Centroid = r.defCent

	setFloat := func(dst **float64, arr []float64, ref *VarRef) {
		if arr == nil || i-r.aux.base >= len(arr) {
			return
		}
		v := arr[i-r.aux.base]
		if ref != nil && ref.IsFill(v) {
			return
		}
		*dst = &v
	}
	setFloat(&s.TIC, r.aux.tic, r.layout.TIC)
	setFloat(&s.LowestMZ, r.aux.massMin, r.layout.MassMin)
	setFloat(&s.HighestMZ, r.aux.massMax, r.layout.MassMax)
	setFloat(&s.ScanDuration, r.aux.scanDur, r.layout.ScanDur)
	setFloat(&s.InterScanTime, r.aux.interScan, r.layout.InterScan)
	setFloat(&s.Resolution, r.aux.resolution, r.layout.Resolution)
	setFloat(&s.BasePeakMZ, r.aux.baseMZ, r.layout.BasePeakMZ)
	setFloat(&s.BasePeakInt, r.aux.baseInt, r.layout.BasePeakInt)
	setFloat(&s.PrecursorMZ, r.aux.preMZ, r.layout.PrecursorMZ)
	setFloat(&s.PrecursorInt, r.aux.preInt, r.layout.PrecursorIn)
	setFloat(&s.CollisionEn, r.aux.colEnergy, r.layout.CollisionE)
	if r.aux.preZ != nil && i-r.aux.base < len(r.aux.preZ) {
		z := int(r.aux.preZ[i-r.aux.base])
		if z != 0 {
			s.PrecursorZ = &z
		}
	}

	if r.opts.IncludeTimeValues && len(mz) > 0 {
		if tv, err := r.peaks.ReadTimeValues(start, count); err == nil && len(tv) == len(mz) {
			// Point times are not part of mzML/mzXML; they are only surfaced to
			// the validator through metadata length.
			s.Metadata = msdata.Metadata{"point_time_values_len": msdata.MetadataValue{
				Kind: msdata.KindInt64, I: int64(len(tv)), Origin: "var:time_values"}}
		}
	}

	r.nextScan++
	r.stats.Spectra++
	r.stats.Points += int64(len(s.MZ))
	return s, nil
}

// ensureAux loads the per-scan auxiliary block containing scan i.
func (r *Reader) ensureAux(i int) error {
	base := (i / auxBlockScans) * auxBlockScans
	if r.aux != nil && r.aux.base == base {
		return nil
	}
	n := r.plan.Scans - base
	if n > auxBlockScans {
		n = auxBlockScans
	}
	b := &auxBlock{base: base, count: n}
	readF := func(ref *VarRef) ([]float64, error) {
		if ref == nil {
			return nil, nil
		}
		ext := ref.Var.Extent()
		if int64(base) >= ext {
			return nil, nil
		}
		want := int64(n)
		if int64(base)+want > ext {
			want = ext - int64(base)
		}
		v, err := r.f.ReadFloats(ref.Var, int64(base), want, nil)
		if err != nil {
			return nil, msdata.WrapError(msdata.CodeNCReadFailed, err, "reading %s at scan %d", ref.Name, base)
		}
		applyScale(v, ref)
		r.stats.IndexArrays += want
		return v, nil
	}
	readI := func(ref *VarRef) ([]int64, error) {
		if ref == nil {
			return nil, nil
		}
		ext := ref.Var.Extent()
		if int64(base) >= ext {
			return nil, nil
		}
		want := int64(n)
		if int64(base)+want > ext {
			want = ext - int64(base)
		}
		v, err := r.f.ReadInts(ref.Var, int64(base), want)
		if err != nil {
			return nil, msdata.WrapError(msdata.CodeNCReadFailed, err, "reading %s at scan %d", ref.Name, base)
		}
		r.stats.IndexArrays += want
		return v, nil
	}

	var err error
	if b.rt, err = readF(r.layout.RT); err != nil {
		return err
	}
	b.hasRT = b.rt != nil
	if b.scanNumber, err = readI(r.layout.ScanNumber); err != nil {
		return err
	}
	if b.tic, err = readF(r.layout.TIC); err != nil {
		return err
	}
	if b.massMin, err = readF(r.layout.MassMin); err != nil {
		return err
	}
	if b.massMax, err = readF(r.layout.MassMax); err != nil {
		return err
	}
	if b.scanDur, err = readF(r.layout.ScanDur); err != nil {
		return err
	}
	if b.interScan, err = readF(r.layout.InterScan); err != nil {
		return err
	}
	if b.resolution, err = readF(r.layout.Resolution); err != nil {
		return err
	}
	if b.msLevel, err = readI(r.layout.MSLevel); err != nil {
		return err
	}
	if b.polarity, err = readF(r.layout.Polarity); err != nil {
		return err
	}
	if b.baseMZ, err = readF(r.layout.BasePeakMZ); err != nil {
		return err
	}
	if b.baseInt, err = readF(r.layout.BasePeakInt); err != nil {
		return err
	}
	if b.preMZ, err = readF(r.layout.PrecursorMZ); err != nil {
		return err
	}
	if b.preInt, err = readF(r.layout.PrecursorIn); err != nil {
		return err
	}
	if b.preZ, err = readI(r.layout.PrecursorZ); err != nil {
		return err
	}
	if b.colEnergy, err = readF(r.layout.CollisionE); err != nil {
		return err
	}
	r.aux = b
	return nil
}

// SourceChecksum returns the bare hex SHA-256 of a file, streamed in 1 MiB
// blocks so a multi-gigabyte corpus file costs memory nothing.
func SourceChecksum(path string) (string, error) {
	_, sum, err := SourceChecksums(path)
	return sum, err
}

// SourceChecksums returns the SHA-1 and SHA-256 of a file in a single pass.
//
// Both digests are needed by real consumers: mzXML requires a SHA-1 of the
// source, while conversion reports and resume state key on SHA-256. Hashing the
// file once instead of twice matters on 8 GB inputs.
func SourceChecksums(path string) (sha1hex, sha256hex string, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer fh.Close()
	buf := make([]byte, 1<<20)
	h1 := sha1.New()
	h2 := sha256.New()
	for {
		n, rerr := fh.Read(buf)
		if n > 0 {
			h1.Write(buf[:n])
			h2.Write(buf[:n])
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", "", rerr
		}
	}
	return hex.EncodeToString(h1.Sum(nil)), hex.EncodeToString(h2.Sum(nil)), nil
}
