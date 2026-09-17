package testutil

import "fmt"

// ANDIOptions parameterises a synthetic ANDI/MS file so tests can reproduce
// every layout family observed in real corpora without shipping binaries.
type ANDIOptions struct {
	Version int // CDF version: 1, 2 or 5

	ScanCount int
	// PointsPerScan cycles through this list across scans.
	PointsPerScan []int

	// RTUnit is written on scan_acquisition_time; "" means no units attribute.
	RTUnit string
	// PointTimeUnit is written on time_values; "" means no units attribute.
	PointTimeUnit string
	// RTIsMinutes controls the numeric magnitude only (values stay consistent
	// with RTUnit).
	RTValueScale float64

	// ScanIndexBase is 0 or 1 (some exporters are Fortran-style 1-based).
	ScanIndexBase int
	// OmitPointCount drops point_count so the reader must derive counts.
	OmitPointCount bool
	// OmitScanIndex drops scan_index (reader must fall back to point_count).
	OmitScanIndex bool
	// OmitTotalIntensity drops total_intensity.
	OmitTotalIntensity bool
	// MassType / IntensityType default to float32; int16 exercises packing.
	MassType      NCType
	IntensityType NCType
	// Packing writes scale_factor/add_offset and packed integer payloads.
	Packing bool

	// GlobalAttributes are merged into the file globals (ANDI style).
	GlobalAttributes map[string]string
	// IncludeInstrumentVars writes the instrument_* char variables.
	IncludeInstrumentVars bool
	// IncludeTimeValues writes the time_values point variable.
	IncludeTimeValues bool
	// ZeroScans makes every scan have zero points.
	ZeroScans bool
	// TruncateBytes writes only the first N bytes of the data section.
	TruncateBytes int
	// ScanNumberVar writes actual_scan_number with these values (len == scans).
	ScanNumbers []int64
	// MSLevelVar writes a per-scan ms_level variable when non-zero.
	MSLevelVar bool
	// NonUTF8Title injects invalid UTF-8 bytes into experiment_title.
	NonUTF8Title bool
	// Filler adds variable/global metadata noise typical of vendors.
	ExtraGlobals map[string]string
}

// ANDI builds a FileSpec approximating an ANDI/MS (ASTM E1205/E1947) export.
//
// Peak values are deterministic functions of (scan, point) so validation can
// recompute expected values without storing them.
func ANDI(opt ANDIOptions) (FileSpec, error) {
	if opt.Version == 0 {
		opt.Version = 1
	}
	if opt.ScanCount <= 0 {
		return FileSpec{}, fmt.Errorf("testutil: ScanCount must be positive")
	}
	if len(opt.PointsPerScan) == 0 {
		opt.PointsPerScan = []int{5}
	}
	if opt.ScanIndexBase == 0 && opt.OmitScanIndex {
		opt.ScanIndexBase = 0
	}
	points := make([]int, opt.ScanCount)
	total := 0
	for i := 0; i < opt.ScanCount; i++ {
		if opt.ZeroScans {
			points[i] = 0
		} else {
			points[i] = opt.PointsPerScan[i%len(opt.PointsPerScan)]
		}
		total += points[i]
	}

	massType := opt.MassType
	if massType == 0 {
		massType = NCFloat
	}
	intenType := opt.IntensityType
	if intenType == 0 {
		intenType = NCFloat
	}
	if opt.Packing {
		// The netCDF packing convention stores integers; scale_factor/add_offset
		// map them back to physical values.
		massType = NCShort
		intenType = NCInt
	}

	dims := []Dim{
		{Name: "point_number", Unlimited: true},
		{Name: "scan_number", Size: int64(opt.ScanCount)},
		{Name: "range", Size: 2},
	}
	if opt.IncludeInstrumentVars {
		dims = append(dims, Dim{Name: "instrument_number", Size: 1}, Dim{Name: "_32_byte_string", Size: 32})
	}

	spec := FileSpec{
		Version: opt.Version,
		Global:  map[string]Attr{},
		Dims:    dims,
		Vars:    nil,
		NumRecs: int64(total),
	}
	for k, v := range opt.GlobalAttributes {
		spec.Global[k] = StringAttr(v)
	}
	for k, v := range opt.ExtraGlobals {
		spec.Global[k] = StringAttr(v)
	}
	if _, ok := spec.Global["ms_template_revision"]; !ok {
		spec.Global["ms_template_revision"] = StringAttr("1.0.1")
	}
	if _, ok := spec.Global["netcdf_revision"]; !ok {
		spec.Global["netcdf_revision"] = StringAttr("4.0.0")
	}
	if opt.NonUTF8Title {
		// 0xC3 0x28 is invalid UTF-8 (typical of GBK-encoded operator names).
		spec.Global["experiment_title"] = StringAttr("run \xc3\x28 name")
	}

	// scan_acquisition_time
	rt := make([]float64, opt.ScanCount)
	scale := opt.RTValueScale
	if scale == 0 {
		scale = 1
	}
	for i := 0; i < opt.ScanCount; i++ {
		rt[i] = float64(i)*0.5*scale + 0.25*scale
	}
	rtv := Var{Name: "scan_acquisition_time", Type: NCDouble, Dims: []string{"scan_number"}, Data: rt, Attr: map[string]Attr{}}
	if opt.RTUnit != "" {
		rtv.Attr["units"] = StringAttr(opt.RTUnit)
	}
	spec.Vars = append(spec.Vars, rtv)

	scanIndex := make([]int32, opt.ScanCount)
	pointCount := make([]int32, opt.ScanCount)
	acc := 0
	for i := 0; i < opt.ScanCount; i++ {
		scanIndex[i] = int32(acc + opt.ScanIndexBase)
		pointCount[i] = int32(points[i])
		acc += points[i]
	}
	if !opt.OmitScanIndex {
		spec.Vars = append(spec.Vars, Var{Name: "scan_index", Type: NCInt, Dims: []string{"scan_number"}, Data: scanIndex})
	}
	if !opt.OmitPointCount {
		spec.Vars = append(spec.Vars, Var{Name: "point_count", Type: NCInt, Dims: []string{"scan_number"}, Data: pointCount})
	}

	if opt.ScanNumbers != nil {
		sn := make([]int32, len(opt.ScanNumbers))
		for i, v := range opt.ScanNumbers {
			sn[i] = int32(v)
		}
		spec.Vars = append(spec.Vars, Var{Name: "actual_scan_number", Type: NCInt, Dims: []string{"scan_number"}, Data: sn})
	}
	if opt.MSLevelVar {
		lv := make([]int32, opt.ScanCount)
		for i := range lv {
			lv[i] = 1
			if i%3 == 2 {
				lv[i] = 2
			}
		}
		spec.Vars = append(spec.Vars, Var{Name: "ms_level", Type: NCInt, Dims: []string{"scan_number"}, Data: lv})
	}

	tic := make([]float64, opt.ScanCount)
	for i := 0; i < opt.ScanCount; i++ {
		sum := 0.0
		for p := 0; p < points[i]; p++ {
			sum += IntensityAt(i, p)
		}
		tic[i] = sum
	}
	if !opt.OmitTotalIntensity {
		tv := Var{Name: "total_intensity", Type: NCDouble, Dims: []string{"scan_number"}, Data: tic,
			Attr: map[string]Attr{"units": StringAttr("Arbitrary Intensity Units")}}
		spec.Vars = append(spec.Vars, tv)
	}

	mn := make([]float64, opt.ScanCount)
	mx := make([]float64, opt.ScanCount)
	for i := 0; i < opt.ScanCount; i++ {
		if points[i] == 0 {
			continue
		}
		mn[i] = MassAt(i, 0)
		mx[i] = MassAt(i, points[i]-1)
	}
	spec.Vars = append(spec.Vars,
		Var{Name: "mass_range_min", Type: NCDouble, Dims: []string{"scan_number"}, Data: mn},
		Var{Name: "mass_range_max", Type: NCDouble, Dims: []string{"scan_number"}, Data: mx},
	)

	// point variables
	massPacked := make([]float64, total)
	intenPacked := make([]float64, total)
	timePacked := make([]float64, total)
	idx := 0
	for i := 0; i < opt.ScanCount; i++ {
		for p := 0; p < points[i]; p++ {
			massPacked[idx] = MassAt(i, p)
			intenPacked[idx] = IntensityAt(i, p)
			timePacked[idx] = float64(i)*0.5 + float64(p)*0.001
			idx++
		}
	}

	massVar := Var{Name: "mass_values", Type: massType, Dims: []string{"point_number"},
		Attr: map[string]Attr{"units": StringAttr("M/Z")}}
	intenVar := Var{Name: "intensity_values", Type: intenType, Dims: []string{"point_number"},
		Attr: map[string]Attr{"units": StringAttr("Arbitrary Intensity Units")}}
	if opt.Packing {
		massVar.Attr["scale_factor"] = FloatAttr(0.1)
		massVar.Attr["add_offset"] = FloatAttr(0.0)
		intenVar.Attr["scale_factor"] = FloatAttr(1.0)
		intenVar.Attr["add_offset"] = FloatAttr(0.0)
		ms := make([]int16, total)
		for i, v := range massPacked {
			ms[i] = int16(v * 10)
		}
		massVar.Data = ms
		it := make([]int32, total)
		for i, v := range intenPacked {
			it[i] = int32(v)
		}
		intenVar.Data = it
	} else {
		switch massType {
		case NCFloat:
			m := make([]float32, total)
			for i, v := range massPacked {
				m[i] = float32(v)
			}
			massVar.Data = m
		case NCDouble:
			massVar.Data = massPacked
		case NCShort:
			m := make([]int16, total)
			for i, v := range massPacked {
				m[i] = int16(v)
			}
			massVar.Data = m
		default:
			return FileSpec{}, fmt.Errorf("testutil: unsupported mass type %d", massType)
		}
		switch intenType {
		case NCFloat:
			v32 := make([]float32, total)
			for i, v := range intenPacked {
				v32[i] = float32(v)
			}
			intenVar.Data = v32
		case NCDouble:
			intenVar.Data = intenPacked
		case NCInt:
			v := make([]int32, total)
			for i, x := range intenPacked {
				v[i] = int32(x)
			}
			intenVar.Data = v
		default:
			return FileSpec{}, fmt.Errorf("testutil: unsupported intensity type %d", intenType)
		}
	}
	spec.Vars = append(spec.Vars, massVar, intenVar)

	if opt.IncludeTimeValues {
		tv := Var{Name: "time_values", Type: NCFloat, Dims: []string{"point_number"}, Data: func() []float32 {
			out := make([]float32, total)
			for i, v := range timePacked {
				out[i] = float32(v)
			}
			return out
		}(), Attr: map[string]Attr{}}
		if opt.PointTimeUnit != "" {
			tv.Attr["units"] = StringAttr(opt.PointTimeUnit)
		}
		spec.Vars = append(spec.Vars, tv)
	}

	if opt.IncludeInstrumentVars {
		str := func(s string) []uint8 {
			b := make([]uint8, 32)
			copy(b, s)
			return b
		}
		for _, n := range []struct{ name, val string }{
			{"instrument_name", "Synthetic QMS"},
			{"instrument_id", "SYN-001"},
			{"instrument_mfr", "Synthetic Instruments Inc."},
			{"instrument_model", "SIM-QQQ-9000"},
			{"instrument_serial_no", "SN0001"},
			{"instrument_sw_version", "1.0"},
			{"instrument_fw_version", ""},
			{"instrument_os_version", ""},
			{"instrument_app_version", ""},
			{"instrument_comments", ""},
		} {
			spec.Vars = append(spec.Vars, Var{Name: n.name, Type: NCChar,
				Dims: []string{"instrument_number", "_32_byte_string"}, Data: str(n.val)})
		}
	}
	return spec, nil
}

// MassAt is the deterministic m/z of peak p in scan i.
func MassAt(scan, point int) float64 {
	return 50.0 + float64(point)*0.7 + float64(scan%17)*0.01
}

// IntensityAt is the deterministic intensity of peak p in scan i.
func IntensityAt(scan, point int) float64 {
	return float64((scan*7919+point*104729)%100000) * 1.5
}

// WriteANDI builds a synthetic ANDI/MS file and writes it to path.
//
// The default options are deliberately a plain, unambiguous GC/MS export: unit
// declared in seconds, index and point counts present, instrument block included.
// Callers override individual fields to reproduce the awkward variants found in
// vendor corpora.
func WriteANDI(path string, opt ANDIOptions) error {
	if opt.ScanCount == 0 {
		opt.ScanCount = 1
	}
	if len(opt.PointsPerScan) == 0 {
		opt.PointsPerScan = []int{5}
	}
	if opt.RTUnit == "" {
		opt.RTUnit = "second"
	}
	if opt.RTValueScale == 0 {
		opt.RTValueScale = 1
	}
	if opt.GlobalAttributes == nil {
		opt.GlobalAttributes = map[string]string{}
	}
	if _, ok := opt.GlobalAttributes["dataset_name"]; !ok {
		name := path
		for i := len(name) - 1; i >= 0; i-- {
			if name[i] == '/' {
				name = name[i+1:]
				break
			}
		}
		opt.GlobalAttributes["dataset_name"] = name
	}
	opt.IncludeInstrumentVars = true
	spec, err := ANDI(opt)
	if err != nil {
		return err
	}
	return WriteNetCDF(path, spec)
}
