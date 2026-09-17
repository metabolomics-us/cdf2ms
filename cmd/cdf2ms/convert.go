package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// formatFlag accepts both repeated -format flags and comma-separated lists,
// because "-format mzML -format mzXML" and "-format mzML,mzXML" are both
// natural things to type.
type formatFlag struct {
	formats []convert.Format
}

func (f *formatFlag) String() string {
	if f == nil || len(f.formats) == 0 {
		return ""
	}
	names := make([]string, len(f.formats))
	for i, fm := range f.formats {
		names[i] = string(fm)
	}
	return strings.Join(names, ",")
}

func (f *formatFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		p, err := convert.ParseFormat(part)
		if err != nil {
			return err
		}
		seen := false
		for _, have := range f.formats {
			if have == p {
				seen = true
				break
			}
		}
		if !seen {
			f.formats = append(f.formats, p)
		}
	}
	return nil
}

func cmdConvert(ctx context.Context, args []string) int {
	fs := newFlagSet("convert")
	var formats formatFlag
	fs.Var(&formats, "format", "output format, repeatable or comma-separated: mzML, mzXML (default both)")
	outDir := fs.String("out-dir", "", "write outputs here instead of beside each source")
	overwrite := fs.Bool("overwrite", false, "replace existing outputs (default: report a collision and skip)")
	jobs := fs.Int("jobs", 0, "concurrent file conversions (alias for -workers)")
	workers := fs.Int("workers", 0, "concurrent file conversions (0 = one per CPU)")
	failFast := fs.Bool("fail-fast", false, "stop scheduling new files after the first failure")
	tempDir := fs.String("temp-dir", "", "write scratch files here; atomic rename into place afterwards")
	resume := fs.String("resume", "", "skip files already recorded as converted in this JSONL report")
	skipExisting := fs.Bool("skip-existing", false, "skip sources whose output already exists")
	rtUnit := fs.String("rt-unit", "auto", "retention-time unit policy: auto|strict|seconds|minutes")
	precision := fs.String("precision", "auto", "binary array precision: auto|f32|f64")
	compress := fs.Bool("compress", false, "zlib-compress binary arrays")
	level := fs.Int("compression-level", 0, "zlib level 1..9 (0 = default)")
	recursive := fs.Bool("recursive", false, "descend into directories")
	maxBytes := fs.String("max-source-bytes", "", "skip sources larger than this size (e.g. 4GB)")
	verify := fs.Bool("verify", false, "re-open each output and compare every number against the source")
	tolerance := fs.Float64("verify-tolerance", 0, "absolute tolerance for --verify comparisons")
	reportJSON := fs.String("report-json", "", "write the aggregate machine-readable report to this path")
	reportJSONL := fs.String("report-jsonl", "", "write one JSON record per source file to this path (streaming)")
	hashSource := fs.Bool("hash-source", false, "hash each source (SHA-1+SHA-256) and record it in the report")
	hashOutputs := fs.Bool("hash-outputs", false, "digest each output as it is written (enables resume-by-digest)")
	logFormat := fs.String("log-format", "text", "log format: text|json")
	logLevel := fs.String("log-level", "info", "log verbosity: debug|info|warn|error")
	logFile := fs.String("log-file", "", "write structured logs here (default: stderr)")
	jsonOutFlag := fs.Bool("json", false, "print the aggregate report as JSON instead of a table")
	quiet := fs.Bool("quiet", false, "suppress progress output")
	failWarn := fs.Bool("fail-on-warning", false, "exit 4 when files convert but report warnings")
	exts := fs.String("ext", ".cdf", "comma-separated source extensions for directory scans")
	fileList := fs.String("file-list", "", "read the list of source files from this file (one path per line)")
	shardIndex := fs.Int("shard-index", -1, "convert only files whose hash falls in this shard (requires -shard-count)")
	shardCount := fs.Int("shard-count", 0, "number of shards for deterministic hash sharding")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	paths := fs.Args()
	if *fileList != "" {
		if len(paths) != 0 {
			fmt.Fprintln(os.Stderr, "cdf2ms convert: -file-list cannot be combined with positional paths")
			return exitUsage
		}
		list, err := readFileList(*fileList)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert:", err)
			return exitUsage
		}
		paths = list
	}
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms convert: at least one file or directory is required")
		return exitUsage
	}
	if (*shardIndex < 0) != (*shardCount == 0) {
		fmt.Fprintln(os.Stderr, "cdf2ms convert: -shard-index and -shard-count must be given together")
		return exitUsage
	}
	if len(formats.formats) == 0 {
		formats.formats = []convert.Format{convert.FormatMzML, convert.FormatMzXML}
	}
	var limit int64
	if *maxBytes != "" {
		n, err := parseSize(*maxBytes)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert:", err)
			return exitUsage
		}
		limit = n
	}
	if *jobs != 0 && *workers != 0 && *jobs != *workers {
		fmt.Fprintln(os.Stderr, "cdf2ms convert: -jobs and -workers disagree")
		return exitUsage
	}
	if *jobs != 0 {
		*workers = *jobs
	}

	// Logging: a leveled structured sink. text goes to stderr (or -log-file),
	// json emits one object per line with file/worker/stage/code/elapsed.
	log, lerr := newLogger(*logFormat, *logLevel, *logFile)
	if lerr != nil {
		fmt.Fprintln(os.Stderr, "cdf2ms convert:", lerr)
		return exitUsage
	}
	if !*quiet && *logFile == "" {
		log.sink = os.Stderr
	}

	opts := convert.Options{
		Formats:          formats.formats,
		OutDir:           *outDir,
		Overwrite:        *overwrite || *skipExisting,
		Workers:          *workers,
		Recursive:        *recursive,
		Precision:        *precision,
		Compress:         *compress,
		CompressionLevel: *level,
		RTUnit:           *rtUnit,
		MaxSourceBytes:   limit,
		ReopenAndVerify:  *verify,
		VerifyTolerance:  *tolerance,
		SoftwareVersion:  version,
		Extensions:       splitList(*exts),
		FailFast:         *failFast,
		TempDir:          *tempDir,
		ResumeFrom:       *resume,
	}
	if *hashSource || *hashOutputs {
		t := *hashSource
		opts.HashSource = &t
		opts.HashOutput = *hashOutputs
	}

	// Streaming JSONL journal of every finished file, in spec §21 field shape.
	var jl *jsonlWriter
	if *reportJSONL != "" {
		w, err := newJSONLWriter(*reportJSONL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert:", err)
			return exitUsage
		}
		jl = w
		defer jl.Close()
		// Hashing the source is needed for the provenance fields in the record.
		if opts.HashSource == nil {
			need := true
			opts.HashSource = &need
		}
	}

	// skip-existing maps onto the engine's collision policy: with Overwrite set,
	// existing outputs are replaced, which is the opposite of what -skip-existing
	// means, so we clear Overwrite when the operator asked to skip.
	if *skipExisting {
		opts.Overwrite = false
	}

	opts.OnResult = func(r convert.FileResult) {
		if jl != nil {
			jl.Write(reportRecord(r))
		}
		logResult(log, r)
	}
	if !*quiet && !*jsonOutFlag {
		opts.OnEvent = func(e convert.Event) {
			switch e.Kind {
			case "start":
				fmt.Printf("-- %s\n", e.Source)
			case "done":
				fmt.Printf("++ %s\n", e.Source)
			case "skip":
				fmt.Printf("~~ %s (%s)\n", e.Source, e.Detail)
			case "fail":
				fmt.Printf("!! %s (%s)\n", e.Source, e.Detail)
			}
		}
	}
	// Deterministic hash sharding: expand the paths to concrete files, then keep
	// only those whose content-independent hash falls in the requested shard.
	// Hashing the absolute path (not the discovery order) makes sharding stable
	// across runs and machines, which is what Slurm array jobs need.
	if *shardIndex >= 0 {
		files, _, derr := convert.Discover(paths, opts)
		if derr != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert:", derr)
			return exitUsage
		}
		selected := make([]string, 0, len(files))
		for _, f := range files {
			if inShard(f, *shardIndex, *shardCount) {
				selected = append(selected, f)
			}
		}
		paths = selected
	}

	rep, err := convert.Run(ctx, paths, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cdf2ms convert:", err)
		return exitUsage
	}
	if *reportJSON != "" {
		b, jerr := rep.JSON()
		if jerr != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert: writing report:", jerr)
			return exitUsage
		}
		if dir := filepath.Dir(*reportJSON); dir != "" && dir != "." {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				fmt.Fprintln(os.Stderr, "cdf2ms convert:", mkErr)
				return exitUsage
			}
		}
		if werr := os.WriteFile(*reportJSON, append(b, '\n'), 0o644); werr != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert: writing report:", werr)
			return exitUsage
		}
	}
	if *jsonOutFlag {
		if err := jsonOut(os.Stdout, rep); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms convert:", err)
			return exitUsage
		}
	} else {
		if !*quiet {
			fmt.Println()
			fmt.Print(rep.Text())
		}
	}

	if ctx.Err() != nil {
		return exitInterrupted
	}
	if rep.Failed > 0 {
		return exitFailed
	}
	if *failWarn && len(rep.WarningCounts) > 0 {
		return exitWarned
	}
	return exitOK
}

// reportRecord renders a FileResult in the spec §21 JSONL shape: one record per
// source with stable provenance fields. The resume loader keys on source + the
// outputs' path/bytes/sha256, so this shape is also the resume contract.
type reportRecordT map[string]any

func reportRecord(r convert.FileResult) reportRecordT {
	status := "skipped"
	switch r.Status {
	case convert.StatusConverted:
		status = "success"
	case convert.StatusFailed:
		status = "failed"
	}
	rec := reportRecordT{
		"source":             r.Source,
		"source_sha256":      r.SourceSHA256,
		"source_size":        r.SourceSize,
		"schema_fingerprint": r.SchemaFingerprint,
		"vendor_fingerprint": r.VendorFingerprint,
		"status":             status,
		"scan_count":         r.Spectra,
		"point_count":        r.Points,
		"duration_ms":        r.ElapsedMS,
		"read_bytes":         r.ReadBytes,
		"write_bytes":        totalOutBytes(r.Outputs),
		"worker":             r.Worker,
		"converter_version":  version,
		"warnings":           diagnosticsToRecords(r.Warnings),
	}
	if r.Code != "" {
		rec["error_code"] = string(r.Code)
	}
	if r.Detail != "" {
		rec["error"] = r.Detail
	}
	targets := make([]string, 0, len(r.Outputs))
	outputs := make([]map[string]any, 0, len(r.Outputs))
	for _, o := range r.Outputs {
		targets = append(targets, o.Path)
		outputs = append(outputs, map[string]any{
			"format":  o.Format,
			"path":    o.Path,
			"bytes":   o.Bytes,
			"sha256":  o.SHA256,
			"spectra": o.Spectra,
			"points":  o.Points,
		})
	}
	rec["target_format"] = strings.Join(targets, ",")
	if len(targets) > 0 {
		rec["output"] = targets[0]
	}
	rec["outputs"] = outputs
	return rec
}

func diagnosticsToRecords(ds []msdata.Diagnostic) []map[string]any {
	if len(ds) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(ds))
	for _, d := range ds {
		m := map[string]any{"code": string(d.Code), "message": d.Message}
		if d.Detail != "" {
			m["detail"] = d.Detail
		}
		out = append(out, m)
	}
	return out
}

func totalOutBytes(outs []msdata.WriteSummary) int64 {
	var n int64
	for _, o := range outs {
		n += o.Bytes
	}
	return n
}

// jsonlWriter streams one JSON record per line, flushing each so a killed run
// still leaves a resumable journal.
type jsonlWriter struct {
	f *os.File
}

func newJSONLWriter(path string) (*jsonlWriter, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &jsonlWriter{f: f}, nil
}

func (w *jsonlWriter) Write(rec reportRecordT) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	w.f.Write(append(b, '\n'))
	w.f.Sync()
}

func (w *jsonlWriter) Close() error {
	if w == nil || w.f == nil {
		return nil
	}
	return w.f.Close()
}

// logger is a tiny leveled, structured logger. It exists so operators can point
// a debug stream at a long corpus without the default progress table drowning in
// per-file noise.
type logger struct {
	level int // debug=0 info=1 warn=2 error=3
	json  bool
	sink  *os.File
}

func newLogger(format, level, path string) (*logger, error) {
	levels := map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}
	lv, ok := levels[strings.ToLower(level)]
	if !ok {
		return nil, fmt.Errorf("invalid -log-level %q (want debug|info|warn|error)", level)
	}
	l := &logger{level: lv, json: strings.ToLower(format) == "json"}
	if path != "" {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, err
		}
		l.sink = f
	} else {
		l.sink = os.Stderr
	}
	return l, nil
}

func (l *logger) log(level int, msg string, kv ...any) {
	if l == nil || level < l.level {
		return
	}
	if l.json {
		m := map[string]any{"level": logLevelName(level), "msg": msg, "time": time.Now().UTC().Format(time.RFC3339)}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		b, _ := json.Marshal(m)
		fmt.Fprintln(l.sink, string(b))
		return
	}
	fmt.Fprintf(l.sink, "%s %s %s", logLevelName(level), msg, formatKV(kv))
}

func logLevelName(level int) string {
	switch level {
	case 0:
		return "debug"
	case 1:
		return "info"
	case 2:
		return "warn"
	case 3:
		return "error"
	}
	return "?"
}

func formatKV(kv []any) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, " %s=%v", kv[i], kv[i+1])
	}
	return b.String()
}

// logResult records one finished file with the spec §23 fields: identity,
// worker, stage, error code, elapsed.
func logResult(l *logger, r convert.FileResult) {
	if l == nil {
		return
	}
	stage := "converted"
	code := ""
	if r.Status == convert.StatusFailed {
		stage = "failed"
		code = string(r.Code)
	} else if r.Status == convert.StatusSkipped {
		stage = "skipped"
		code = string(r.Code)
	}
	l.log(1, stage, "file", r.Source, "worker", r.Worker, "stage", stage,
		"code", code, "elapsed_ms", r.ElapsedMS, "spectra", r.Spectra, "points", r.Points)
}

// splitList splits a comma-separated flag value, trimming whitespace and
// dropping empties.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// readFileList reads one source path per line, skipping blanks and comments, so
// an HPC scheduler can hand the tool an exact shard manifest.
func readFileList(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// inShard reports whether a file falls in shard index of count, keyed by a
// content-independent hash of its absolute path (FNV-1a 64). Hash-based sharding
// is stable regardless of discovery order or machine, unlike index-based slicing.
func inShard(path string, index, count int) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	h := fnv.New64a()
	h.Write([]byte(abs))
	return int(h.Sum64()%uint64(count)) == index
}
