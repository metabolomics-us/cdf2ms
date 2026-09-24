package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/metabolomics-us/cdf2ms/pkg/andiio"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

type auditRole struct {
	Role     string `json:"role"`
	Variable string `json:"variable,omitempty"`
	Units    string `json:"units,omitempty"`
	Aliased  bool   `json:"aliased,omitempty"`
	Packed   bool   `json:"packed,omitempty"`
	Required bool   `json:"required,omitempty"`
}

type auditDiag struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Scan    int    `json:"scan,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type auditDeep struct {
	Spectra         int64            `json:"spectra"`
	Points          int64            `json:"points"`
	EmptySpectra    int64            `json:"emptySpectra"`
	MinRetentionSec float64          `json:"minRetentionSeconds"`
	MaxRetentionSec float64          `json:"maxRetentionSeconds"`
	NonMonotonicRT  int64            `json:"nonMonotonicRetentionSteps"`
	MSLevelCounts   map[int]int64    `json:"msLevelCounts,omitempty"`
	PolarityCounts  map[string]int64 `json:"polarityCounts,omitempty"`
	PeaksPerScanMin int64            `json:"peaksPerScanMin"`
	PeaksPerScanMax int64            `json:"peaksPerScanMax"`
}

type auditReport struct {
	Path          string         `json:"path"`
	Verdict       string         `json:"verdict"`
	Container     string         `json:"container,omitempty"`
	Size          int64          `json:"sizeBytes,omitempty"`
	Spectra       int            `json:"declaredSpectra"`
	Points        int64          `json:"declaredPoints"`
	PlanSource    string         `json:"planSource,omitempty"`
	IndexBase     int64          `json:"scanIndexBase,omitempty"`
	EmptyScans    int            `json:"emptyScans"`
	ClippedScans  int            `json:"clippedScans"`
	MinScanPoints int64          `json:"minScanPoints"`
	MaxScanPoints int64          `json:"maxScanPoints"`
	MedianPoints  string         `json:"medianScanPoints,omitempty"`
	RTUnit        string         `json:"retentionTimeUnit,omitempty"`
	RTUnitOrigin  string         `json:"retentionTimeUnitOrigin,omitempty"`
	RTConfidence  string         `json:"retentionTimeConfidence,omitempty"`
	Instrument    string         `json:"instrument,omitempty"`
	RunID         string         `json:"runId,omitempty"`
	Fingerprint   string         `json:"schemaFingerprint,omitempty"`
	Vendor        string         `json:"vendorFingerprint,omitempty"`
	MetadataKeys  int            `json:"preservedMetadataValues"`
	UnmappedVars  []string       `json:"unmappedSourceVariables,omitempty"`
	Roles         []auditRole    `json:"roles,omitempty"`
	Warnings      []auditDiag    `json:"warnings,omitempty"`
	WarningCounts map[string]int `json:"warningCounts,omitempty"`
	Fatal         *auditDiag     `json:"fatal,omitempty"`
	Deep          *auditDeep     `json:"deep,omitempty"`
	ElapsedMS     int64          `json:"elapsedMs"`
	Err           string         `json:"error,omitempty"`
}

const (
	verdictConvertible = "convertible"
	verdictWarned      = "convertible-with-warnings"
	verdictNo          = "not-convertible"
)

func cmdAudit(ctx context.Context, args []string) int {
	fs := newFlagSet("audit")
	jsonFlag := fs.Bool("json", false, "emit JSON instead of a table")
	deep := fs.Bool("deep", false, "also stream every scan to measure retention-time behaviour and peak counts")
	rtUnit := fs.String("rt-unit", "auto", "retention-time unit policy: auto|strict|seconds|minutes")
	failWarn := fs.Bool("fail-on-warning", false, "exit 4 when a file converts but reports warnings")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms audit: at least one file is required")
		return exitUsage
	}
	policy, err := andiio.ParseRTUnitPolicy(*rtUnit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cdf2ms audit:", err)
		return exitUsage
	}

	reports := make([]auditReport, 0, len(paths))
	failed := 0
	warned := 0
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		rep := auditOne(ctx, p, policy, *deep)
		reports = append(reports, rep)
		switch rep.Verdict {
		case verdictNo:
			failed++
		case verdictWarned:
			warned++
		}
	}
	if *jsonFlag {
		if err := jsonOut(os.Stdout, reports); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms audit:", err)
			return exitUsage
		}
	} else {
		for _, rep := range reports {
			printAudit(os.Stdout, rep)
		}
		fmt.Printf("audited %d file(s): %d convertible, %d with warnings, %d not convertible\n",
			len(reports), len(reports)-failed-warned, warned, failed)
	}
	if failed > 0 {
		return exitFailed
	}
	if *failWarn && warned > 0 {
		return exitWarned
	}
	return exitOK
}

// auditClock times each audit; a variable so tests can advance it.
var auditClock = time.Now

// auditOne audits one source. rep is a named result so the deferred timing
// lands on the value returned rather than on a copy taken before it ran.
func auditOne(ctx context.Context, path string, policy andiio.RTUnitPolicy, deep bool) (rep auditReport) {
	started := auditClock()
	rep = auditReport{Path: path, Verdict: verdictConvertible}
	defer func() { rep.ElapsedMS = auditClock().Sub(started).Milliseconds() }()
	r, err := andiio.Open(path, andiio.Options{RTUnit: policy})
	if err != nil {
		rep.Verdict = verdictNo
		rep.Err = err.Error()
		code := msdata.CodeOf(err)
		if code == "" {
			code = msdata.CodeOpenFailed
		}
		rep.Fatal = &auditDiag{Code: string(code), Message: err.Error()}
		return rep
	}
	defer r.Close()

	src := r.SourceFile()
	rep.Container = src.Encoding
	rep.Size = src.Size

	run, err := r.Metadata(ctx)
	if err != nil {
		rep.Verdict = verdictNo
		rep.Fatal = &auditDiag{Code: string(msdata.CodeOf(err)), Message: err.Error()}
		return rep
	}
	rep.RunID = run.ID
	rep.Spectra = run.ScanCount
	rep.Points = run.PointCount
	rep.Fingerprint = run.SchemaFingerprint
	rep.Vendor = run.VendorFingerprint
	rep.Instrument = instrumentSummary(run.Instrument)
	rep.MetadataKeys = len(run.Metadata)

	rt := r.RTUnit()
	rep.RTUnit = rt.Name
	rep.RTUnitOrigin = rt.Origin
	rep.RTConfidence = rt.Confidence

	if plan := r.Plan(); plan != nil {
		rep.PlanSource = plan.Source
		rep.IndexBase = plan.IndexBase
		rep.EmptyScans = plan.EmptyScans
		rep.ClippedScans = plan.ClippedScans
		rep.MinScanPoints, rep.MaxScanPoints, rep.MedianPoints = planStats(plan)
	}
	rep.Roles = roleTable(r.Layout())
	if lay := r.Layout(); lay != nil {
		for _, v := range lay.Unmapped {
			rep.UnmappedVars = append(rep.UnmappedVars, v.Name)
		}
	}

	if diags := r.Diagnostics(); diags != nil {
		for _, w := range diags.Warnings {
			rep.Warnings = append(rep.Warnings, auditDiag{Code: string(w.Code), Message: w.Message,
				Scan: w.Scan, Detail: w.Detail})
			if rep.WarningCounts == nil {
				rep.WarningCounts = map[string]int{}
			}
			rep.WarningCounts[string(w.Code)]++
		}
		if diags.Fatal != nil {
			rep.Fatal = &auditDiag{Code: string(diags.Fatal.Code), Message: diags.Fatal.Message,
				Scan: diags.Fatal.Scan, Detail: diags.Fatal.Detail}
		}
	}

	if deep {
		d, derr := deepAudit(ctx, r)
		if derr != nil {
			rep.Err = derr.Error()
			code := msdata.CodeOf(derr)
			if rep.Fatal == nil {
				rep.Fatal = &auditDiag{Code: string(code), Message: derr.Error()}
			}
		}
		rep.Deep = d
	}

	if rep.Fatal != nil {
		rep.Verdict = verdictNo
	} else if len(rep.Warnings) > 0 {
		rep.Verdict = verdictWarned
	}
	return rep
}

func deepAudit(ctx context.Context, r *andiio.Reader) (*auditDeep, error) {
	d := &auditDeep{PeaksPerScanMin: -1}
	var prevRT float64
	havePrev := false
	for {
		sp, err := r.Next(ctx)
		if errors.Is(err, msdata.ErrEndOfData) {
			break
		}
		if err != nil {
			return d, err
		}
		d.Spectra++
		n := len(sp.MZ)
		d.Points += int64(n)
		if n == 0 {
			d.EmptySpectra++
		}
		if d.PeaksPerScanMin < 0 || int64(n) < d.PeaksPerScanMin {
			d.PeaksPerScanMin = int64(n)
		}
		if int64(n) > d.PeaksPerScanMax {
			d.PeaksPerScanMax = int64(n)
		}
		if sp.RetentionTimeSet {
			if !havePrev {
				d.MinRetentionSec = sp.RetentionTime
				d.MaxRetentionSec = sp.RetentionTime
				havePrev = true
			} else {
				if sp.RetentionTime < d.MinRetentionSec {
					d.MinRetentionSec = sp.RetentionTime
				}
				if sp.RetentionTime > d.MaxRetentionSec {
					d.MaxRetentionSec = sp.RetentionTime
				}
				if sp.RetentionTime <= prevRT {
					d.NonMonotonicRT++
				}
			}
			prevRT = sp.RetentionTime
		}
		if sp.MSLevel != nil {
			if d.MSLevelCounts == nil {
				d.MSLevelCounts = map[int]int64{}
			}
			d.MSLevelCounts[*sp.MSLevel]++
		}
		if sp.Polarity != msdata.PolarityUnknown {
			if d.PolarityCounts == nil {
				d.PolarityCounts = map[string]int64{}
			}
			d.PolarityCounts[sp.Polarity.String()]++
		}
	}
	if d.PeaksPerScanMin < 0 {
		d.PeaksPerScanMin = 0
	}
	return d, nil
}

// planStats summarises the scan plan. The median comes from a bounded sample so
// auditing a file with millions of scans stays cheap; min and max are exact.
func planStats(plan *andiio.ScanPlan) (min, max int64, median string) {
	if plan == nil || len(plan.Count) == 0 {
		return 0, 0, ""
	}
	min = int64(plan.Count[0])
	max = min
	const sample = 4096
	sampled := make([]int64, 0, smallest(len(plan.Count), sample))
	stride := (len(plan.Count) + sample - 1) / sample
	for i, c := range plan.Count {
		if int64(c) < min {
			min = int64(c)
		}
		if int64(c) > max {
			max = int64(c)
		}
		if i%stride == 0 {
			sampled = append(sampled, int64(c))
		}
	}
	sort.Slice(sampled, func(i, j int) bool { return sampled[i] < sampled[j] })
	n := len(sampled)
	if n == 0 {
		return min, max, ""
	}
	if n%2 == 1 {
		return min, max, fmt.Sprintf("%d", sampled[n/2])
	}
	med := (sampled[n/2-1] + sampled[n/2]) / 2
	return min, max, fmt.Sprintf("%g", float64(med))
}

func smallest(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// roleTable renders which source variable satisfies each spectrum role, including
// roles the source does not provide: "absent" is a fact about the file, and the
// only honest way to say that a field will be omitted downstream.
func roleTable(lay *andiio.Layout) []auditRole {
	if lay == nil {
		return nil
	}
	roles := []struct {
		name     string
		ref      *andiio.VarRef
		required bool
	}{
		{"m/z values", lay.Mass, true},
		{"intensity values", lay.Intensity, true},
		{"retention time", lay.RT, true},
		{"scan index", lay.ScanIndex, false},
		{"point count", lay.PointCount, false},
		{"actual scan number", lay.ScanNumber, false},
		{"total ion current", lay.TIC, false},
		{"mass range min", lay.MassMin, false},
		{"mass range max", lay.MassMax, false},
		{"acquisition time min", lay.TimeMin, false},
		{"acquisition time max", lay.TimeMax, false},
		{"scan duration", lay.ScanDur, false},
		{"inter-scan delay", lay.InterScan, false},
		{"resolution", lay.Resolution, false},
		{"ms level", lay.MSLevel, false},
		{"polarity", lay.Polarity, false},
		{"base peak m/z", lay.BasePeakMZ, false},
		{"base peak intensity", lay.BasePeakInt, false},
		{"precursor m/z", lay.PrecursorMZ, false},
		{"precursor intensity", lay.PrecursorIn, false},
		{"precursor charge", lay.PrecursorZ, false},
		{"collision energy", lay.CollisionE, false},
		{"point time values", lay.TimeValues, false},
	}
	out := make([]auditRole, 0, len(roles))
	for _, r := range roles {
		ar := auditRole{Role: r.name, Required: r.required}
		if r.ref != nil {
			ar.Variable = r.ref.Name
			ar.Units = r.ref.Units
			ar.Aliased = r.ref.Aliased
			ar.Packed = r.ref.Packed
		}
		out = append(out, ar)
	}
	return out
}

func instrumentSummary(in msdata.Instrument) string {
	parts := make([]string, 0, 4)
	for _, s := range []string{in.Name, in.Manufacturer, in.Model, in.SerialNumber} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if in.IonizationMode != "" {
		parts = append(parts, in.IonizationMode)
	}
	return strings.Join(parts, " / ")
}

func printAudit(w *os.File, rep auditReport) {
	fmt.Fprintf(w, "%s\n", rep.Path)
	fmt.Fprintf(w, "  verdict: %s  (%s)\n", rep.Verdict, formatDur(msDuration(rep.ElapsedMS)))
	if rep.Container != "" {
		fmt.Fprintf(w, "  container: %s  size: %s\n", rep.Container, humanBytes(rep.Size))
	}
	if rep.RunID != "" {
		fmt.Fprintf(w, "  run: %s  spectra: %d  points: %d\n", rep.RunID, rep.Spectra, rep.Points)
	}
	if rep.PlanSource != "" {
		fmt.Fprintf(w, "  scan plan: %s (index base %d, %d empty, %d clipped; peaks/scan %d..%d%s)\n",
			rep.PlanSource, rep.IndexBase, rep.EmptyScans, rep.ClippedScans,
			rep.MinScanPoints, rep.MaxScanPoints, medianNote(rep.MedianPoints))
	}
	if rep.RTUnit != "" {
		fmt.Fprintf(w, "  retention time: %s [%s, %s]\n", rep.RTUnit, rep.RTUnitOrigin, rep.RTConfidence)
	}
	if rep.Instrument != "" {
		fmt.Fprintf(w, "  instrument: %s\n", rep.Instrument)
	}
	if rep.Fingerprint != "" {
		fmt.Fprintf(w, "  fingerprints: schema=%s vendor=%s\n", rep.Fingerprint, rep.Vendor)
	}
	if roles := compactRoles(rep.Roles); roles != "" {
		fmt.Fprintf(w, "  variables: %s\n", roles)
		if missing := missingRequired(rep.Roles); len(missing) > 0 {
			fmt.Fprintf(w, "  MISSING REQUIRED: %s\n", strings.Join(missing, ", "))
		}
		if aliased := aliasedRoles(rep.Roles); len(aliased) > 0 {
			fmt.Fprintf(w, "  aliases used: %s\n", strings.Join(aliased, ", "))
		}
	}
	if rep.Deep != nil {
		d := rep.Deep
		fmt.Fprintf(w, "  deep scan: %d spectra, %d points, %d empty; RT %.3f..%.3f s, %d non-monotonic step(s)\n",
			d.Spectra, d.Points, d.EmptySpectra, d.MinRetentionSec, d.MaxRetentionSec, d.NonMonotonicRT)
		fmt.Fprintf(w, "  peaks/scan: %d..%d\n", d.PeaksPerScanMin, d.PeaksPerScanMax)
		if len(d.MSLevelCounts) > 0 {
			keys := make([]int, 0, len(d.MSLevelCounts))
			for k := range d.MSLevelCounts {
				keys = append(keys, k)
			}
			sort.Ints(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("MS%d=%d", k, d.MSLevelCounts[k]))
			}
			fmt.Fprintf(w, "  ms levels: %s\n", strings.Join(parts, " "))
		}
		if len(d.PolarityCounts) > 0 {
			fmt.Fprintf(w, "  polarity: %s\n", mapParts(d.PolarityCounts))
		}
	}
	if len(rep.UnmappedVars) > 0 {
		fmt.Fprintf(w, "  unmapped source variables kept as metadata: %s\n",
			strings.Join(rep.UnmappedVars, " "))
	}
	if rep.Fatal != nil {
		fmt.Fprintf(w, "  FATAL [%s] %s\n", rep.Fatal.Code, rep.Fatal.Message)
		if rep.Fatal.Detail != "" {
			fmt.Fprintf(w, "        %s\n", rep.Fatal.Detail)
		}
	}
	if len(rep.Warnings) > 0 {
		fmt.Fprintf(w, "  warnings:\n")
		for _, d := range rep.Warnings {
			fmt.Fprintf(w, "    %-32s %s\n", d.Code, d.Message)
		}
	}
	if rep.Err != "" && rep.Err != errString(rep.Fatal) {
		fmt.Fprintf(w, "  error: %s\n", rep.Err)
	}
}

func errString(d *auditDiag) string {
	if d == nil {
		return ""
	}
	return d.Message
}

func medianNote(m string) string {
	if m == "" {
		return ""
	}
	return ", median " + m + " (sampled)"
}

func mapParts(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func compactRoles(roles []auditRole) string {
	parts := make([]string, 0, len(roles))
	for _, r := range roles {
		if r.Variable == "" {
			continue
		}
		parts = append(parts, r.Role+"="+r.Variable)
	}
	return strings.Join(parts, " ")
}

func missingRequired(roles []auditRole) []string {
	var out []string
	for _, r := range roles {
		if r.Required && r.Variable == "" {
			out = append(out, r.Role)
		}
	}
	return out
}

func aliasedRoles(roles []auditRole) []string {
	var out []string
	for _, r := range roles {
		if r.Aliased {
			out = append(out, r.Role+"="+r.Variable)
		}
	}
	return out
}

func msDuration(ms int64) time.Duration {
	return time.Duration(ms) * time.Millisecond
}
