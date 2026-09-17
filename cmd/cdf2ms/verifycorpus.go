package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/metabolomics-us/cdf2ms/pkg/convert"
)

func boolPtr(b bool) *bool { return &b }

// cmdVerifyCorpus implements "cdf2ms verify-corpus PATH...": convert into scratch
// storage, independently re-open and verify every produced document, summarize,
// and optionally delete the successful outputs. It is the batch form of
// "--verify" for corpus-wide regression gates.
func cmdVerifyCorpus(ctx context.Context, args []string) int {
	fs := newFlagSet("verify-corpus")
	var formats formatFlag
	fs.Var(&formats, "format", "output format, repeatable or comma-separated: mzML, mzXML (default both)")
	tempDir := fs.String("temp-dir", "", "scratch directory for outputs (required)")
	jobs := fs.Int("jobs", 0, "concurrent file conversions (0 = one per CPU)")
	recursive := fs.Bool("recursive", false, "descend into directories")
	rtUnit := fs.String("rt-unit", "auto", "retention-time unit policy: auto|strict|seconds|minutes")
	tolerance := fs.Float64("tolerance", 0, "absolute tolerance for verification (0 = bit-exact)")
	reportJSONL := fs.String("report-jsonl", "", "write one JSON record per file to this path")
	keep := fs.Bool("keep", true, "keep the verified outputs (default)")
	deleteOut := fs.Bool("delete", false, "delete successful outputs after verification (the gate leaves no artifacts)")
	jsonFlag := fs.Bool("json", false, "emit the summary as JSON")
	quiet := fs.Bool("quiet", false, "suppress progress output")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *tempDir == "" {
		fmt.Fprintln(os.Stderr, "cdf2ms verify-corpus: -temp-dir is required")
		return exitUsage
	}
	// Only reject when the operator explicitly passed both flags; -keep merely
	// defaults to true, so -delete alone must not trip the mutual exclusion.
	keepSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "keep" {
			keepSet = true
		}
	})
	if *deleteOut && keepSet && *keep {
		fmt.Fprintln(os.Stderr, "cdf2ms verify-corpus: -delete and -keep are mutually exclusive")
		return exitUsage
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms verify-corpus: at least one file or directory is required")
		return exitUsage
	}
	if len(formats.formats) == 0 {
		formats.formats = []convert.Format{convert.FormatMzML, convert.FormatMzXML}
	}

	opts := convert.Options{
		Formats:         formats.formats,
		OutDir:          *tempDir,
		Overwrite:       true,
		Workers:         *jobs,
		Recursive:       *recursive,
		RTUnit:          *rtUnit,
		ReopenAndVerify: true,
		VerifyTolerance: *tolerance,
		SoftwareVersion: version,
		TempDir:         *tempDir,
		Extensions:      []string{".cdf", ".CDF"},
		ResumeFrom:      *reportJSONL,
		HashSource:      boolPtr(true),
	}
	var jl *jsonlWriter
	if *reportJSONL != "" {
		w, err := newJSONLWriter(*reportJSONL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms verify-corpus:", err)
			return exitUsage
		}
		jl = w
		defer jl.Close()
		opts.OnResult = func(r convert.FileResult) { jl.Write(reportRecord(r)) }
	}
	if !*quiet && !*jsonFlag {
		opts.OnEvent = func(e convert.Event) {
			switch e.Kind {
			case "done":
				fmt.Printf("++ %s\n", e.Source)
			case "fail":
				fmt.Printf("!! %s (%s)\n", e.Source, e.Detail)
			case "skip":
				fmt.Printf("~~ %s (%s)\n", e.Source, e.Detail)
			}
		}
	}

	rep, err := convert.Run(ctx, paths, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cdf2ms verify-corpus:", err)
		return exitUsage
	}

	if *deleteOut {
		for _, r := range rep.Results {
			if r.Status != convert.StatusConverted {
				continue
			}
			for _, o := range r.Outputs {
				_ = os.Remove(o.Path)
			}
		}
	}

	if *jsonFlag {
		if err := jsonOut(os.Stdout, rep); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms verify-corpus:", err)
			return exitUsage
		}
	} else if !*quiet {
		fmt.Println()
		fmt.Print(rep.Text())
	}

	if ctx.Err() != nil {
		return exitInterrupted
	}
	if rep.Failed > 0 {
		return exitFailed
	}
	return exitOK
}
