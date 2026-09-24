package andiio

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/netcdfio"
	"time"
)

// globalMap maps ANDI global attributes onto Instrument fields. The value is the
// Instrument struct member. Every global is also preserved verbatim in
// Run.Metadata, so nothing is lost when a mapping does not apply.
var globalMap = map[string]string{
	"test_ionization_mode":      "ionization",
	"test_ionization_polarity":  "polarity",
	"test_detector_type":        "detector",
	"test_ms_inlet":             "inlet",
	"test_separation_type":      "separation",
	"test_scan_function":        "scan_function",
	"test_scan_direction":       "scan_direction",
	"test_scan_law":             "scan_law",
	"test_resolution_type":      "resolution",
	"test_ms_data_type":         "data_type",
	"test_ms_centroiding":       "centroiding",
	"experiment_title":          "experiment_title",
	"experiment_type":           "experiment_type",
	"operator_name":             "operator",
	"operator":                  "operator",
	"dataset_origin":            "origin",
	"instrument_name":           "name",
	"instrument_id":             "id",
	"instrument_mfr":            "manufacturer",
	"instrument_model":          "model",
	"instrument_serial_no":      "serial",
	"instrument_sw_version":     "software",
	"instrument_fw_version":     "firmware",
	"instrument_os_version":     "os",
	"instrument_app_version":    "app",
	"instrument_comments":       "comments",
	"ms_acquisition_software":   "acquisition_software",
	"ms_analysis_software":      "analysis_software",
	"ms_calibration_software":   "calibration_software",
	"ms_mass_units":             "mass_units",
	"ms_scan_accuracy":          "scan_accuracy",
	"ms_scan_accuracy_units":    "scan_accuracy_units",
	"ms_detector_type":          "detector",
	"ms_ionization_mode":        "ionization",
	"ms_inlet_type":             "inlet",
	"ms_data_file_format":       "data_format",
	"dataset_completeness":      "dataset_completeness",
	"raw_data_mass_format":      "raw_mass_format",
	"raw_data_intensity_format": "raw_intensity_format",
	"raw_data_time_format":      "raw_time_format",
}

// extraFields carries ANDI metadata that has no Instrument slot but is still
// scientifically relevant and must survive into the output as user params.
var extraFields = []string{
	"sample_name", "sample_inject_volume", "sample_solvent", "sample_state",
	"collection_date", "collection_start_time", "collection_end_time",
	"experiment_date_time_stamp", "netcdf_file_date_time_stamp",
	"data_file_name", "original_instrument_file", "chromatography_data_file",
	"ms_template_revision", "netcdf_revision", "ms_scan_format", "ms_scan_type",
	"ms_range_min", "ms_range_max", "ms_centroid_mass_window", "ms_scan_min",
	"ms_scan_max", "ms_mass_units", "number_of_times_calibrated",
	"number_of_times_processed", "spin_deck_number", "chromatographer",
	"administrative_comments", "languages", "external_file_ref_0",
	"dataset_completeness", "raw_data_mass_format", "raw_data_intensity_format",
	"raw_data_time_format",
}

// BuildRunMetadata assembles the msdata.Run for an opened ANDI file.
func BuildRunMetadata(f *netcdfio.File, l *Layout, rtUnit RTUnit, plan *ScanPlan,
	src msdata.SourceFile, d *msdata.Diagnostics) *msdata.Run {

	run := &msdata.Run{
		ID:                      RunID(src.Path),
		Source:                  src,
		Metadata:                msdata.Metadata{},
		RetentionTimeUnit:       "second",
		RetentionTimeSourceUnit: rtUnit.Name,
		RetentionTimeUnitOrigin: rtUnit.Origin,
	}

	apply := func(field, value, origin string) {
		value = sanitizeText(value)
		if value == "" {
			return
		}
		switch field {
		case "ionization":
			setOnce(&run.Instrument.IonizationMode, value)
		case "polarity":
			setOnce(&run.Instrument.IonizationPol, value)
		case "detector":
			setOnce(&run.Instrument.DetectorType, value)
		case "inlet":
			setOnce(&run.Instrument.MSInlet, value)
		case "separation":
			setOnce(&run.Instrument.SeparationType, value)
		case "scan_function":
			setOnce(&run.Instrument.ScanFunction, value)
		case "scan_direction":
			setOnce(&run.Instrument.ScanDirection, value)
		case "scan_law":
			setOnce(&run.Instrument.ScanLaw, value)
		case "resolution":
			setOnce(&run.Instrument.ResolutionType, value)
		case "data_type":
			setOnce(&run.Instrument.ExperimentType, value)
		case "centroiding":
			setOnce(&run.Instrument.ScanLaw, "")
			run.Metadata["centroiding"] = msdata.MetadataValue{Kind: msdata.KindString, Str: value, Origin: origin}
		case "experiment_title":
			setOnce(&run.Instrument.ExperimentTitle, value)
		case "experiment_type":
			setOnce(&run.Instrument.ExperimentType, value)
		case "operator":
			setOnce(&run.Instrument.Operator, value)
		case "origin":
			setOnce(&run.Instrument.DataSetOrigin, value)
		case "name":
			setOnce(&run.Instrument.Name, value)
		case "id":
			setOnce(&run.Instrument.ID, value)
		case "manufacturer":
			setOnce(&run.Instrument.Manufacturer, value)
		case "model":
			setOnce(&run.Instrument.Model, value)
		case "serial":
			setOnce(&run.Instrument.SerialNumber, value)
		case "software":
			setOnce(&run.Instrument.SoftwareVer, value)
		case "firmware":
			setOnce(&run.Instrument.FirmwareVer, value)
		case "os":
			setOnce(&run.Instrument.OSVersion, value)
		case "app":
			setOnce(&run.Instrument.AppVersion, value)
		case "comments":
			setOnce(&run.Instrument.Comments, value)
		case "acquisition_software":
			setOnce(&run.Instrument.SoftwareVer, value)
		default:
			// unmapped: fall through to metadata below
		}
	}

	// global attributes
	for _, k := range sortedAttrKeys(f.Globals) {
		a := f.Globals[k]
		raw := attrRawText(a)
		if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0xFFFD) {
			d.WarnDetail(msdata.CodeANDIMetadataNotUTF8, 0, "global:"+k,
				"global attribute %q is not valid UTF-8; it was sanitised with replacement characters", k)
		}
		clean := sanitizeText(raw)
		if clean == "" {
			continue
		}
		origin := "global:" + k
		if field, ok := globalMap[normalizeKey(k)]; ok {
			apply(field, clean, origin)
		}
		run.Metadata[origin] = textOrNumeric(clean)
	}

	// instrument char variables (ANDI stores them as [instrument_number][32] char)
	for _, k := range sortedKeysStr(l.InstrumentVars) {
		ref := l.InstrumentVars[k]
		v, err := f.ReadString(ref.Var)
		if err != nil {
			d.WarnDetail(msdata.CodeNCReadFailed, 0, "var:"+ref.Name,
				"could not read instrument variable %s: %v", ref.Name, err)
			continue
		}
		clean := sanitizeText(v)
		if clean == "" {
			continue
		}
		origin := "var:" + ref.Name
		if field, ok := globalMap[normalizeKey(ref.Name)]; ok {
			apply(field, clean, origin)
		}
		run.Metadata[origin] = msdata.MetadataValue{Kind: msdata.KindString, Str: clean, Origin: origin}
	}

	if a, ok := f.Globals["experiment_date_time_stamp"]; ok {
		if raw := sanitizeText(attrRawText(a)); raw != "" {
			if ts, ok := ParseANDIDateTime(raw); ok {
				run.AcquisitionStart = ts
				run.AcquisitionStartOrigin = "global:experiment_date_time_stamp"
			} else {
				d.WarnDetail(msdata.CodeANDIAcquisitionTimeUnusable, 0, "global:experiment_date_time_stamp="+raw,
					"experiment_date_time_stamp %q is not a timestamp with a UTC offset; the run start time is omitted", raw)
			}
		}
	}

	// preserved extras that live only in metadata
	for _, name := range extraFields {
		if a, ok := f.Globals[name]; ok {
			clean := sanitizeText(attrRawText(a))
			if clean != "" {
				run.Metadata["global:"+name] = textOrNumeric(clean)
			}
		}
	}

	// unmapped variables are summarised rather than dropped, so an auditor can
	// see what the source carried that cdf2ms does not model.
	for _, ref := range l.Unmapped {
		run.Metadata["var:"+ref.Name] = msdata.MetadataValue{
			Kind:   msdata.KindString,
			Str:    fmt.Sprintf("%s %v extent=%d", ref.Var.Type, ref.Var.Dims, ref.Var.Extent()),
			Origin: "var:" + ref.Name,
		}
	}

	if plan != nil {
		run.ScanCount = plan.Scans
		run.PointCount = plan.TotalPoints
	}
	run.SchemaFingerprint = SchemaFingerprint(f)
	run.VendorFingerprint = VendorFingerprint(f, run)
	return run
}

// setOnce writes the first non-empty value only: ANDI lists several aliases and
// the first one found wins, so mapping order stays deterministic.
func setOnce(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

// RunID derives a stable, filesystem-safe identifier from a source path.
func RunID(path string) string {
	base := filepath.Base(path)
	if dot := strings.LastIndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	var b strings.Builder
	for i := 0; i < len(base); i++ {
		c := base[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

// SchemaFingerprint groups files with structurally identical layouts: same
// variable names, types, dimensions and attribute keys. Two files with the same
// fingerprint can be expected to convert identically, which is what makes a
// sampled compatibility check meaningful.
func SchemaFingerprint(f *netcdfio.File) string {
	h := sha256.New()
	fmt.Fprintf(h, "enc=%s\n", f.Encoding())
	for _, d := range f.Dimensions {
		fmt.Fprintf(h, "dim %s %v\n", d.Name, d.Unlimited)
	}
	names := f.VarNames()
	for _, n := range names {
		v, err := f.Var(n)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "var %s %s %v rec=%v", v.Name, v.Type, v.Dims, v.IsRecord)
		for _, ak := range sortedAttrKeys(v.Attrs) {
			fmt.Fprintf(h, " @%s:%s", ak, v.Attrs[ak].Kind())
		}
		h.Write([]byte("\n"))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// VendorFingerprint groups files by exporter/instrument identity so a corpus can
// be sampled per vendor rather than per file.
func VendorFingerprint(f *netcdfio.File, run *msdata.Run) string {
	h := sha256.New()
	field := func(name string) string {
		if a, ok := f.Globals[name]; ok {
			return sanitizeText(attrRawText(a))
		}
		return ""
	}
	fmt.Fprintf(h, "inst=%s|%s|%s\n", run.Instrument.Name, run.Instrument.Manufacturer, run.Instrument.Model)
	fmt.Fprintf(h, "sw=%s|%s|%s\n", field("ms_acquisition_software"), field("ms_analysis_software"), field("instrument_sw_version"))
	fmt.Fprintf(h, "tpl=%s|%s\n", field("ms_template_revision"), field("netcdf_revision"))
	fmt.Fprintf(h, "exp=%s\n", field("experiment_type"))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// DerivePolarity maps ANDI polarity evidence to a msdata.Polarity.
//
// ANDI expresses polarity in several places with several vocabularies. Only an
// explicit statement is honoured; absence yields PolarityUnknown, which writers
// omit rather than assert.
func DerivePolarity(runGlobal netcdfio.Attribute, scanPolarityValue float64, hasScanPolarity bool) (msdata.Polarity, string) {
	if hasScanPolarity {
		// ANDI scan_polarity: 1 == positive, 0 == negative in the template, but
		// some vendors invert it. Only the template convention is honoured.
		if scanPolarityValue == 1 {
			return msdata.PolarityPositive, "var:scan_polarity=1"
		}
		if scanPolarityValue == 0 {
			return msdata.PolarityNegative, "var:scan_polarity=0"
		}
		return msdata.PolarityUnknown, ""
	}
	if runGlobal.IsChar {
		k := normalizeUnit(runGlobal.Str)
		switch {
		case strings.Contains(k, "positive"), k == "pos", k == "p":
			return msdata.PolarityPositive, "global:test_ionization_polarity"
		case strings.Contains(k, "negative"), k == "neg", k == "n":
			return msdata.PolarityNegative, "global:test_ionization_polarity"
		}
	}
	return msdata.PolarityUnknown, ""
}

// DeriveCentroid maps ANDI text evidence to a centroid state.
func DeriveCentroid(values ...msdata.MetadataValue) (msdata.CentroidState, string) {
	for _, v := range values {
		if v.Kind != msdata.KindString {
			continue
		}
		k := normalizeUnit(v.Str)
		switch {
		case strings.Contains(k, "centroid"), strings.Contains(k, "centroied"), strings.Contains(k, "discrete"):
			return msdata.CentroidCentroided, v.Origin
		case strings.Contains(k, "profile"), strings.Contains(k, "continuous"), strings.Contains(k, "scanprofile"):
			return msdata.CentroidProfile, v.Origin
		}
	}
	return msdata.CentroidUnknown, ""
}

// sanitizeText trims trailing NUL/space padding from NetCDF char data and
// replaces invalid UTF-8 rather than dropping the value outright.
func sanitizeText(s string) string {
	s = strings.TrimRight(s, "\x00 ")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if utf8.ValidString(s) {
		return s
	}
	b := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			b = append(b, '\uFFFD')
			i++
			continue
		}
		b = append(b, r)
		i += size
	}
	return strings.TrimSpace(string(b))
}

func attrRawText(a netcdfio.Attribute) string {
	if a.IsChar {
		return a.Str
	}
	return a.Value()
}

// textOrNumeric stores a value as numeric when it parses cleanly, so reports and
// mzML user params keep their type instead of degrading to strings.
func textOrNumeric(s string) msdata.MetadataValue {
	if v, ok := parseFloatStrict(s); ok {
		if v == float64(int64(v)) && absFloat(v) < 1e15 {
			return msdata.MetadataValue{Kind: msdata.KindInt64, I: int64(v), Str: s}
		}
		return msdata.MetadataValue{Kind: msdata.KindFloat64, F: v, Str: s}
	}
	return msdata.MetadataValue{Kind: msdata.KindString, Str: s}
}

// parseFloatStrict accepts only canonical numeric text (no hex floats, no
// inf/nan words, no trailing junk), so metadata that merely looks numeric stays
// a string instead of being silently coerced.
func parseFloatStrict(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i++
	}
	digits, dot, exp := 0, false, false
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == '.' && !dot && !exp:
			dot = true
		case (c == 'e' || c == 'E') && !exp && digits > 0:
			exp = true
			if i+1 < len(s) && (s[i+1] == '+' || s[i+1] == '-') {
				i++
			}
		default:
			return 0, false
		}
	}
	if digits == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func sortedAttrKeys(m map[string]netcdfio.Attribute) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortedKeysStr[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
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

// andiDateTimeLayouts are the timestamp shapes accepted for the run start. The
// ANDI/MS template (ASTM E1947) writes YYYYMMDDhhmmss followed by a signed UTC
// offset, e.g. "20170320235239-0800"; every production file surveyed uses it.
// RFC 3339 is accepted for exporters that write ISO timestamps. Layouts without
// an offset are deliberately absent.
var andiDateTimeLayouts = []string{
	"20060102150405-0700",
	"20060102150405Z0700",
	time.RFC3339Nano,
}

// ParseANDIDateTime parses an ANDI date-time stamp into UTC. It reports false
// for anything without an explicit UTC offset.
func ParseANDIDateTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range andiDateTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
