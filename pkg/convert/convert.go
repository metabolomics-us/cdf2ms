// Package convert turns ANDI/MS NetCDF files into mzML and mzXML documents,
// one streaming pass per source no matter how many output formats are asked for.
//
// The engine is deliberately explicit about refusal: a source it cannot convert
// honestly is reported with a stable code and a human explanation, never coerced
// into an output whose numbers or metadata would be invented.
package convert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/metabolomics-us/cdf2ms/pkg/andiio"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/mzml"
	"github.com/metabolomics-us/cdf2ms/pkg/mzxml"
	"github.com/metabolomics-us/cdf2ms/pkg/validate"
)

// Format is an output format.
type Format string

const (
	FormatMzML  Format = "mzML"
	FormatMzXML Format = "mzXML"
)

// Extension is the file suffix used for an output format.
func (f Format) Extension() string {
	switch f {
	case FormatMzML:
		return ".mzML"
	case FormatMzXML:
		return ".mzXML"
	}
	return "." + string(f)
}

// ParseFormat accepts the spellings an operator types (mzml, mzML, mzxml,
// mzXML3.2 ...) and rejects anything else rather than guessing.
func ParseFormat(s string) (Format, error) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.TrimSuffix(k, "1.1")
	k = strings.TrimSuffix(k, "3.2")
	switch k {
	case "mzml", "psi-ms":
		return FormatMzML, nil
	case "mzxml":
		return FormatMzXML, nil
	}
	return "", fmt.Errorf("unknown output format %q (want mzml or mzxml)", s)
}

// Status is the per-file outcome.
type Status string

const (
	StatusConverted Status = "converted"
	StatusSkipped   Status = "skipped"
	StatusFailed    Status = "failed"
)

// Options controls a conversion run.
type Options struct {
	// Formats is required; a source is read once and written to each format.
	Formats []Format

	// OutDir receives the outputs. Empty means beside each source file.
	OutDir string

	// Overwrite permits replacing an existing output. Without it a collision is
	// reported instead of destroying data.
	Overwrite bool

	// Workers bounds concurrent file conversions. <=0 means GOMAXPROCS-based.
	Workers int

	// Recursive descends into directories named on the command line.
	Recursive bool

	// Precision is "auto", "f32" or "f64".
	Precision string

	// Compress zlib-compresses binary arrays.
	Compress bool
	// CompressionLevel is a compress/zlib level; 0 keeps the default.
	CompressionLevel int

	// RTUnit is "auto", "strict", "seconds" or "minutes".
	RTUnit string

	// MaxSourceBytes refuses sources larger than this (0 = no limit).
	MaxSourceBytes int64

	// ReopenAndVerify re-reads every produced document and compares it against
	// the source before the file is counted as converted.
	ReopenAndVerify bool
	// VerifyTolerance is the absolute tolerance for read-back comparison.
	VerifyTolerance float64

	// SoftwareVersion is written into each document's provenance.
	SoftwareVersion string

	// Extensions are the source suffixes accepted during directory discovery
	// (default .cdf and .CDF variants).
	Extensions []string

	// Now overrides timestamps so runs are reproducible in tests.
	Now func() time.Time

	// OnEvent receives progress notifications. It is called from the result
	// collector goroutine of Run, so implementations need no locking.
	OnEvent func(Event)

	// OnResult receives each finished file result as it completes (completion
	// order, not discovery order), so a long corpus can be journalled live. It is
	// called from the same collector goroutine as OnEvent and needs no locking.
	OnResult func(FileResult)

	// FailFast stops scheduling new files once one has failed. Files already in
	// flight are allowed to finish and their results are still reported. The
	// default is deliberately the opposite: one bad file must not abandon a corpus.
	FailFast bool

	// TempDir holds each writer's scratch file before the finished document is
	// renamed into place. Empty keeps the scratch file beside the output, where
	// the rename is atomic.
	TempDir string

	// HashSource computes the SHA-1 and SHA-256 of each source in one pass and
	// records them in the report and in the documents. nil means "only when a
	// requested format needs them": mzXML requires a source SHA-1, so hashing is
	// free there, while an mzML-only run would pay an extra pass over the data.
	HashSource *bool

	// HashOutput digests every produced document while it is written (no extra
	// pass) so reports and resume state can key on the output digest.
	HashOutput bool

	// ResumeFrom names a JSONL report from an earlier run. Sources whose recorded
	// outputs still exist, still match in size, and (when recorded) still match in
	// digest are skipped rather than re-converted.
	ResumeFrom string
}

// Event is a progress notification.
type Event struct {
	Kind   string // "start" | "done" | "skip" | "fail"
	Source string
	Detail string
}

// FileResult is the outcome for one source file.
type FileResult struct {
	Source       string `json:"source"`
	SourceSize   int64  `json:"sourceSize,omitempty"`
	SourceSHA1   string `json:"sourceSha1,omitempty"`
	SourceSHA256 string `json:"sourceSha256,omitempty"`
	// SchemaFingerprint and VendorFingerprint identify the ANDI layout and the
	// instrument that produced it, which is how a corpus is grouped by exporter.
	SchemaFingerprint string                `json:"schemaFingerprint,omitempty"`
	VendorFingerprint string                `json:"vendorFingerprint,omitempty"`
	Worker            int                   `json:"worker,omitempty"`
	Status            Status                `json:"status"`
	Code              msdata.Code           `json:"code,omitempty"`
	Detail            string                `json:"detail,omitempty"`
	Spectra           int64                 `json:"spectra"`
	Points            int64                 `json:"points"`
	SkippedPts        int64                 `json:"droppedPoints,omitempty"`
	RTUnit            string                `json:"rtUnit,omitempty"`
	RTUnitOrigin      string                `json:"rtUnitOrigin,omitempty"`
	Outputs           []msdata.WriteSummary `json:"outputs,omitempty"`
	Warnings          []msdata.Diagnostic   `json:"warnings,omitempty"`
	ReadBytes         int64                 `json:"readBytes,omitempty"`
	ElapsedMS         int64                 `json:"elapsedMs"`
}

// Report aggregates a whole run.
type Report struct {
	Tool          string              `json:"tool"`
	StartedAt     time.Time           `json:"startedAt"`
	FinishedAt    time.Time           `json:"finishedAt"`
	DurationMS    int64               `json:"durationMs"`
	Formats       []Format            `json:"formats"`
	Files         int                 `json:"files"`
	Converted     int                 `json:"converted"`
	Skipped       int                 `json:"skipped"`
	Failed        int                 `json:"failed"`
	Spectra       int64               `json:"spectra"`
	Points        int64               `json:"points"`
	BytesOut      int64               `json:"bytesOut"`
	WarningCounts map[msdata.Code]int `json:"warningCounts,omitempty"`
	Results       []FileResult        `json:"results"`
}

// JSON renders the report machine-readably.
func (r *Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

// Text renders the report for a terminal.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cdf2ms %s  %s -> %s  (%.1fs)\n", r.Tool,
		r.StartedAt.UTC().Format(time.RFC3339), r.FinishedAt.UTC().Format(time.RFC3339),
		float64(r.DurationMS)/1000.0)
	fmt.Fprintf(&b, "targets: %s\n", formatList(r.Formats))
	fmt.Fprintf(&b, "files: %d  converted: %d  skipped: %d  failed: %d\n",
		r.Files, r.Converted, r.Skipped, r.Failed)
	fmt.Fprintf(&b, "spectra: %d  points: %d  bytes written: %s\n",
		r.Spectra, r.Points, humanBytes(r.BytesOut))
	for _, res := range r.Results {
		switch res.Status {
		case StatusConverted:
			outs := make([]string, 0, len(res.Outputs))
			for _, o := range res.Outputs {
				outs = append(outs, filepath.Base(o.Path)+" ("+o.Format+", "+humanBytes(o.Bytes)+")")
			}
			fmt.Fprintf(&b, "  ok   %s  %d spectra  %d points  rt=%s[%s]  %s  %.0fms\n",
				res.Source, res.Spectra, res.Points, res.RTUnit, res.RTUnitOrigin,
				strings.Join(outs, ", "), float64(res.ElapsedMS))
			for _, w := range res.Warnings {
				fmt.Fprintf(&b, "       warn %s: %s\n", w.Code, w.Message)
			}
		case StatusSkipped:
			fmt.Fprintf(&b, "  skip %s  %s: %s\n", res.Source, res.Code, res.Detail)
		case StatusFailed:
			fmt.Fprintf(&b, "  FAIL %s  %s: %s\n", res.Source, res.Code, res.Detail)
		}
	}
	if len(r.WarningCounts) > 0 {
		codes := make([]msdata.Code, 0, len(r.WarningCounts))
		for c := range r.WarningCounts {
			codes = append(codes, c)
		}
		sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
		b.WriteString("warning totals:\n")
		for _, c := range codes {
			fmt.Fprintf(&b, "  %-36s %d\n", c, r.WarningCounts[c])
		}
	}
	return b.String()
}

func formatList(fs []Format) string {
	names := make([]string, len(fs))
	for i, f := range fs {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Discover expands the command-line paths into source files. Files named
// explicitly are always considered; directory entries are matched by extension.
func Discover(paths []string, opts Options) (files []string, skipped []FileResult, err error) {
	exts := opts.Extensions
	if len(exts) == 0 {
		exts = []string{".cdf"}
	}
	lower := make([]string, len(exts))
	for i, e := range exts {
		lower[i] = strings.ToLower(e)
	}
	seen := map[string]bool{}
	add := func(p string) {
		abs, aerr := filepath.Abs(p)
		if aerr != nil {
			abs = p
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		files = append(files, p)
	}
	for _, p := range paths {
		st, serr := os.Stat(p)
		if serr != nil {
			skipped = append(skipped, FileResult{
				Source: p, Status: StatusSkipped, Code: msdata.CodeFileNotFound,
				Detail: serr.Error(),
			})
			continue
		}
		if !st.IsDir() {
			add(p)
			continue
		}
		werr := filepath.WalkDir(p, func(path string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil // unreadable corners of a tree are reported, not fatal
			}
			if d.IsDir() {
				if path != p && !opts.Recursive {
					return filepath.SkipDir
				}
				return nil
			}
			if matchesAnyExt(strings.ToLower(d.Name()), lower) {
				add(path)
			}
			return nil
		})
		if werr != nil {
			return nil, skipped, werr
		}
	}
	sort.Strings(files)
	return files, skipped, nil
}

func matchesAnyExt(name string, exts []string) bool {
	for _, e := range exts {
		if strings.HasSuffix(name, e) {
			return true
		}
	}
	return false
}

// Run converts every discovered source and returns the aggregate report. A
// non-nil error means the run itself could not proceed; per-file problems are
// carried in the report.
func Run(ctx context.Context, paths []string, opts Options) (*Report, error) {
	if len(opts.Formats) == 0 {
		return nil, errors.New("convert: at least one output format is required")
	}
	if _, err := parsePrecision(opts.Precision); err != nil {
		return nil, err
	}
	if _, err := andiio.ParseRTUnitPolicy(opts.RTUnit); err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	files, skipped, err := Discover(paths, opts)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		Tool:          versionOr(opts.SoftwareVersion),
		StartedAt:     now(),
		Formats:       append([]Format(nil), opts.Formats...),
		WarningCounts: map[msdata.Code]int{},
	}
	for _, s := range skipped {
		rep.Results = append(rep.Results, s)
	}
	if len(files) == 0 {
		rep.FinishedAt = now()
		rep.DurationMS = rep.FinishedAt.Sub(rep.StartedAt).Milliseconds()
		rep.Skipped = len(rep.Results)
		return rep, nil
	}

	workers := opts.Workers
	if workers <= 0 {
		workers = defaultWorkers()
	}
	if workers > len(files) {
		workers = len(files)
	}

	type slot struct {
		path   string
		worker int
	}
	in := make(chan slot)
	out := make(chan FileResult, len(files))

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for s := range in {
				out <- convertOne(ctx, s.path, id, opts)
			}
		}(w)
	}

	// resumeState maps a source path to its recorded outputs from an earlier run.
	resumeState := loadResume(opts.ResumeFrom)

	// failFast is set once a failure is collected so the feeder stops admitting
	// new work; the pipeline drains and every in-flight result is still reported.
	var failFast atomic.Bool

	go func() {
		defer close(in)
		for i, f := range files {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if failFast.Load() {
				// Not a failure of this file: it never started.
				out <- FileResult{Source: f, Status: StatusSkipped, Code: msdata.CodeFailFast,
					Detail: "not started: an earlier file failed and --fail-fast was set"}
				continue
			}
			if rec, ok := resumeState[f]; ok {
				if targets, t := resumable(rec, f, opts); t {
					out <- FileResult{Source: f, Status: StatusSkipped, Code: msdata.CodeSkipResumed,
						Detail: "already converted in an earlier run; outputs still present and matching: " +
							strings.Join(targets, ", ")}
					continue
				}
			}
			select {
			case <-ctx.Done():
				return
			case in <- slot{path: f, worker: i % workers}:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(out)
	}()

	bySource := make(map[string]FileResult, len(files))
	for res := range out {
		bySource[res.Source] = res
		if res.Status == StatusFailed {
			failFast.Store(true)
		}
		if opts.OnResult != nil {
			opts.OnResult(res)
		}
		if opts.OnEvent != nil {
			switch res.Status {
			case StatusConverted:
				opts.OnEvent(Event{Kind: "done", Source: res.Source})
			case StatusSkipped:
				opts.OnEvent(Event{Kind: "skip", Source: res.Source, Detail: string(res.Code)})
			case StatusFailed:
				opts.OnEvent(Event{Kind: "fail", Source: res.Source, Detail: string(res.Code)})
			}
		}
	}
	// Emit in discovery order so reports are stable and diffable.
	for _, f := range files {
		res, ok := bySource[f]
		if !ok {
			res = FileResult{Source: f, Status: StatusSkipped, Code: msdata.CodeCancelled,
				Detail: "not processed: the run finished or was cancelled before this file"}
		}
		rep.Results = append(rep.Results, res)
		switch res.Status {
		case StatusConverted:
			rep.Converted++
			rep.Spectra += res.Spectra
			rep.Points += res.Points
			for _, o := range res.Outputs {
				rep.BytesOut += o.Bytes
			}
		case StatusSkipped:
			rep.Skipped++
		case StatusFailed:
			rep.Failed++
		}
		for _, w := range res.Warnings {
			rep.WarningCounts[w.Code]++
		}
	}
	rep.Files = len(files) + len(skipped)
	if ctx.Err() != nil {
		rep.Results = append(rep.Results, FileResult{
			Source: "<run>", Status: StatusSkipped, Code: msdata.CodeCancelled,
			Detail: ctx.Err().Error(),
		})
		rep.Skipped++
	}
	rep.FinishedAt = now()
	rep.DurationMS = rep.FinishedAt.Sub(rep.StartedAt).Milliseconds()
	return rep, nil
}

// nowFn returns the timestamp a writer should stamp, honouring a reproducible
// clock so tests and release builds are byte-stable.
func nowFn(now func() time.Time) time.Time {
	if now == nil {
		return time.Now().UTC()
	}
	return now().UTC()
}

func versionOr(v string) string {
	if v == "" {
		return "dev"
	}
	return v
}

func defaultWorkers() int {
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return n
}

// convertOne streams one source into every requested format.
func convertOne(ctx context.Context, path string, worker int, opts Options) FileResult {
	start := time.Now()
	res := FileResult{Source: path, Worker: worker}
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()

	policy, err := andiio.ParseRTUnitPolicy(opts.RTUnit)
	if err != nil {
		res.Status, res.Code, res.Detail = StatusFailed, msdata.CodeInvalidOption, err.Error()
		return res
	}
	readerOpts := andiio.Options{RTUnit: policy}

	st, statErr := os.Stat(path)
	if statErr != nil {
		res.Status, res.Code, res.Detail = StatusSkipped, msdata.CodeFileNotFound, statErr.Error()
		return res
	}
	res.SourceSize = st.Size()
	if opts.MaxSourceBytes > 0 && st.Size() > opts.MaxSourceBytes {
		res.Status, res.Code = StatusSkipped, msdata.CodeSourceTooLarge
		res.Detail = fmt.Sprintf("%s is %s, above the %s limit",
			filepath.Base(path), humanBytes(st.Size()), humanBytes(opts.MaxSourceBytes))
		return res
	}

	// Decide once whether the source must be hashed. mzXML needs a source SHA-1
	// (its parentFile/@fileSha1 is schema-required), so hashing is free whenever
	// mzXML is among the formats; otherwise it is an explicit extra pass.
	needHash := opts.HashSource != nil && *opts.HashSource
	if opts.HashSource == nil {
		for _, f := range opts.Formats {
			if f == FormatMzXML {
				needHash = true
				break
			}
		}
	}
	// The digests are computed before the reader is opened and are handed to the
	// writers so none of them re-reads the file for its own checksum.
	if needHash {
		if s1, s256, err := andiio.SourceChecksums(path); err == nil {
			res.SourceSHA1 = s1
			res.SourceSHA256 = s256
		}
	}

	// Resolve every output path before the source is opened: a collision should
	// cost nothing, and must not leave a half-written document behind.
	targets := make([]string, len(opts.Formats))
	for i, f := range opts.Formats {
		targets[i] = OutputPath(path, f, opts)
		if !opts.Overwrite {
			if _, err := os.Stat(targets[i]); err == nil {
				res.Status, res.Code = StatusSkipped, msdata.CodeCollision
				res.Detail = "output exists and --overwrite was not given: " + targets[i]
				return res
			}
		}
	}

	rd, err := andiio.Open(path, readerOpts)
	if err != nil {
		res.Status = StatusFailed
		res.Code = msdata.CodeOf(err)
		if res.Code == "" {
			res.Code = msdata.CodeOpenFailed
		}
		res.Detail = err.Error()
		// An unconvertible source is a skip with a reason, not a crash: a corpus
		// run must keep going through the next file.
		if errors.Is(err, msdata.ErrSourceUnsupported) {
			res.Status = StatusSkipped
		}
		return res
	}
	defer rd.Close()

	run, err := rd.Metadata(ctx)
	if err != nil {
		res.Status, res.Code, res.Detail = StatusFailed, msdata.CodeOf(err), err.Error()
		return res
	}
	res.RTUnit = run.RetentionTimeUnit
	res.RTUnitOrigin = run.RetentionTimeUnitOrigin

	// Hand the digests (if computed) to every writer so mzXML does not re-read the
	// source for its required parentFile/@fileSha1.
	if res.SourceSHA1 != "" || res.SourceSHA256 != "" {
		src := run.Source
		if res.SourceSHA1 != "" {
			src.SHA1 = res.SourceSHA1
		}
		if res.SourceSHA256 != "" {
			src.SHA256 = res.SourceSHA256
		}
		run.Source = src
	}

	writers := make([]msdata.SpectrumWriter, 0, len(opts.Formats))
	defer func() {
		// Anything still open at return time is aborted so no orphan temp file
		// survives an early exit.
		for _, w := range writers {
			w.Abort()
		}
	}()
	for i, f := range opts.Formats {
		w, cerr := createWriter(f, targets[i], run, run.Source, opts)
		if cerr != nil {
			res.Status, res.Code, res.Detail = StatusFailed, msdata.CodeOf(cerr), cerr.Error()
			return res
		}
		writers = append(writers, w)
	}

	for {
		sp, err := rd.Next(ctx)
		if errors.Is(err, msdata.ErrEndOfData) {
			break
		}
		if err != nil {
			res.Status, res.Code, res.Detail = StatusFailed, msdata.CodeOf(err), err.Error()
			if res.Code == "" {
				res.Code = msdata.CodeReadFailed
			}
			return res
		}
		for _, w := range writers {
			if err := w.Write(sp); err != nil {
				res.Status, res.Code, res.Detail = StatusFailed, msdata.CodeOf(err), err.Error()
				if res.Code == "" {
					res.Code = msdata.CodeOutputWriteFailed
				}
				return res
			}
		}
	}

	for i, w := range writers {
		if err := w.Close(); err != nil {
			res.Status, res.Code, res.Detail = StatusFailed, msdata.CodeOf(err), err.Error()
			// Keep the artifact: Close already moved it to *.partial when it
			// decided the document was inconsistent.
			_ = i
			return res
		}
		sum := w.Summary()
		res.Outputs = append(res.Outputs, sum)
		res.Warnings = append(res.Warnings, sum.Warnings...)
	}
	writers = writers[:0]

	stats := rd.Stats()
	res.Spectra = stats.Spectra
	res.Points = stats.Points
	res.ReadBytes = stats.BytesRead
	res.SchemaFingerprint = run.SchemaFingerprint
	res.VendorFingerprint = run.VendorFingerprint
	if res.SourceSHA1 == "" {
		res.SourceSHA1 = run.Source.SHA1
	}
	if res.SourceSHA256 == "" {
		res.SourceSHA256 = run.Source.SHA256
	}
	if diags := rd.Diagnostics(); diags != nil {
		res.Warnings = append(res.Warnings, diags.Warnings...)
		if diags.Fatal != nil {
			res.Status = StatusFailed
			res.Code = diags.Fatal.Code
			res.Detail = diags.Fatal.Message
			return res
		}
	}

	if opts.ReopenAndVerify {
		for _, out := range res.Outputs {
			vr, err := validate.Document(ctx, out.Path, path, validate.Options{
				Tolerance: opts.VerifyTolerance,
				RTUnit:    opts.RTUnit,
			})
			if err != nil || (vr != nil && len(vr.Problems) > 0) {
				res.Status = StatusFailed
				res.Code = msdata.CodeOf(err)
				if res.Code == "" && vr != nil && len(vr.Problems) > 0 {
					res.Code = vr.Problems[0].Code
				}
				if res.Code == "" {
					res.Code = msdata.CodeNumericMismatch
				}
				res.Detail = err.Error()
				return res
			}
			msg := fmt.Sprintf("%s re-read from disk and matched against the source",
				filepath.Base(out.Path))
			res.Warnings = append(res.Warnings, msdata.Diagnostic{
				Code:    msdata.CodeVerified,
				Message: msg,
				Detail: fmt.Sprintf("%d spectra, %d peaks compared byte-for-byte",
					vr.Spectra, vr.Points),
			})
		}
	}

	res.Status = StatusConverted
	return res
}

func createWriter(f Format, path string, run *msdata.Run, src msdata.SourceFile, opts Options) (msdata.SpectrumWriter, error) {
	prec, err := parsePrecision(opts.Precision)
	if err != nil {
		return nil, err
	}
	common := struct {
		Compress         bool
		CompressionLevel int
		SoftwareVersion  string
		TempDir          string
		HashOutput       bool
		Timestamp        time.Time
	}{
		Compress: opts.Compress, CompressionLevel: opts.CompressionLevel,
		SoftwareVersion: versionOr(opts.SoftwareVersion),
		TempDir:         opts.TempDir, HashOutput: opts.HashOutput,
		Timestamp: nowFn(opts.Now),
	}
	switch f {
	case FormatMzML:
		return mzml.Create(path, run, src, mzml.Options{
			Precision:        mzml.Precision(prec),
			Compress:         common.Compress,
			CompressionLevel: common.CompressionLevel,
			SoftwareVersion:  common.SoftwareVersion,
			TempDir:          common.TempDir,
			HashOutput:       common.HashOutput,
			Timestamp:        common.Timestamp,
		})
	case FormatMzXML:
		return mzxml.Create(path, run, src, mzxml.Options{
			Precision:        mzxml.Precision(prec),
			Compress:         common.Compress,
			CompressionLevel: common.CompressionLevel,
			SoftwareVersion:  common.SoftwareVersion,
			TempDir:          common.TempDir,
			HashOutput:       common.HashOutput,
			Timestamp:        common.Timestamp,
		})
	}
	return nil, fmt.Errorf("convert: unsupported format %q", f)
}

// OutputPath is where a source's output goes, honouring OutDir.
func OutputPath(src string, f Format, opts Options) string {
	ext := f.Extension()
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)) + ext
	dir := opts.OutDir
	if dir == "" {
		dir = filepath.Dir(src)
	}
	return filepath.Join(dir, base)
}

type precision int

const (
	precisionAuto precision = iota
	precisionF32
	precisionF64
)

func parsePrecision(s string) (precision, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return precisionAuto, nil
	case "f32", "32", "float32":
		return precisionF32, nil
	case "f64", "64", "float64":
		return precisionF64, nil
	}
	return 0, fmt.Errorf("convert: unknown precision %q (want auto, f32 or f64)", s)
}

// resumeRecord is the subset of a prior run's per-file report that resume needs.
type resumeRecord struct {
	Source  string `json:"source"`
	Status  string `json:"status"`
	Outputs []struct {
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256,omitempty"`
	} `json:"outputs,omitempty"`
}

// loadResume parses a JSONL report from an earlier run into a lookup keyed by
// source path. Records whose status is neither success nor warning are ignored:
// only completed documents count as done.
func loadResume(path string) map[string]resumeRecord {
	m := map[string]resumeRecord{}
	if path == "" {
		return m
	}
	fh, err := os.Open(path)
	if err != nil {
		return m
	}
	defer fh.Close()
	dec := json.NewDecoder(fh)
	for {
		var rec resumeRecord
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec.Source == "" {
			continue
		}
		switch rec.Status {
		case "success", "warning", "converted":
			m[rec.Source] = rec
		}
	}
	return m
}

// resumable reports whether every target for a source already exists, matches
// the recorded size, and (when a digest was recorded) matches it too. A resume
// must not trust a bare filename: the document has to be the one that run made.
func resumable(rec resumeRecord, src string, opts Options) ([]string, bool) {
	if len(opts.Formats) == 0 {
		return nil, false
	}
	want := make([]string, len(opts.Formats))
	for i, f := range opts.Formats {
		want[i] = OutputPath(src, f, opts)
	}
	recorded := map[string]struct {
		bytes  int64
		sha256 string
	}{}
	for _, o := range rec.Outputs {
		if o.Path != "" {
			recorded[o.Path] = struct {
				bytes  int64
				sha256 string
			}{o.Bytes, o.SHA256}
		}
	}
	all := make([]string, 0, len(want))
	for _, target := range want {
		st, err := os.Stat(target)
		if err != nil {
			return nil, false
		}
		r, ok := recorded[target]
		if !ok {
			return nil, false
		}
		if r.bytes > 0 && st.Size() != r.bytes {
			return nil, false
		}
		if r.sha256 != "" {
			got, err := andiio.SourceChecksum(target)
			if err != nil || got != r.sha256 {
				return nil, false
			}
		}
		all = append(all, target)
	}
	return all, true
}
