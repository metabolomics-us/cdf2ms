package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
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
	workers := fs.Int("workers", 0, "concurrent file conversions (0 = one per CPU)")
	rtUnit := fs.String("rt-unit", "auto", "retention-time unit policy: auto|strict|seconds|minutes")
	precision := fs.String("precision", "auto", "binary array precision: auto|f32|f64")
	compress := fs.Bool("compress", false, "zlib-compress binary arrays")
	level := fs.Int("compression-level", 0, "zlib level 1..9 (0 = default)")
	recursive := fs.Bool("recursive", false, "descend into directories")
	maxBytes := fs.String("max-source-bytes", "", "skip sources larger than this size (e.g. 4GB)")
	verify := fs.Bool("verify", false, "re-open each output and compare every number against the source")
	tolerance := fs.Float64("verify-tolerance", 0, "absolute tolerance for --verify comparisons")
	reportJSON := fs.String("report-json", "", "write the machine-readable report to this path")
	jsonOutFlag := fs.Bool("json", false, "print the report as JSON instead of a table")
	quiet := fs.Bool("quiet", false, "suppress progress output")
	failWarn := fs.Bool("fail-on-warning", false, "exit 4 when files convert but report warnings")
	exts := fs.String("ext", ".cdf", "comma-separated source extensions for directory scans")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms convert: at least one file or directory is required")
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

	opts := convert.Options{
		Formats:          formats.formats,
		OutDir:           *outDir,
		Overwrite:        *overwrite,
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
