package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/metabolomics-us/cdf2ms/pkg/validate"
)

// cmdValidate implements "cdf2ms validate SOURCE.CDF OUTPUT.mzML|OUTPUT.mzXML":
// re-open one produced document with an independent reader and compare every
// number against the source. This is the standalone form of --verify, for use in
// scripts and pipeline gates that want a dedicated check.
func cmdValidate(ctx context.Context, args []string) int {
	fs := newFlagSet("validate")
	jsonFlag := fs.Bool("json", false, "emit the result as JSON")
	tolerance := fs.Float64("tolerance", 0, "absolute tolerance for numeric comparison (0 = bit-exact)")
	rtUnit := fs.String("rt-unit", "auto", "retention-time unit policy used at conversion")
	skipSHA1 := fs.Bool("skip-sha1", false, "skip the mzXML <sha1> re-computation (reads the whole file)")
	skipIndex := fs.Bool("skip-index", false, "skip the mzXML byte-offset index check")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "cdf2ms validate: usage: cdf2ms validate SOURCE.CDF OUTPUT.mzML|mzXML")
		return exitUsage
	}
	src, out := fs.Arg(0), fs.Arg(1)

	res, err := validate.Document(ctx, out, src, validate.Options{
		Tolerance: *tolerance,
		RTUnit:    *rtUnit,
		SkipSHA1:  *skipSHA1,
		SkipIndex: *skipIndex,
	})
	if *jsonFlag {
		if jerr := jsonOut(os.Stdout, res); jerr != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms validate:", jerr)
			return exitUsage
		}
	} else {
		if res != nil {
			fmt.Printf("%s: %s  %d spectra, %d peaks\n", res.Path, res.Format, res.Spectra, res.Points)
			if res.IndexOK {
				fmt.Printf("  index: OK\n")
			}
			if res.SHA1OK {
				fmt.Printf("  sha1:  OK\n")
			}
		}
	}
	if err != nil {
		if errors.Is(err, validate.ErrProblems) {
			fmt.Fprintf(os.Stderr, "cdf2ms validate: %s\n", err)
			if res != nil {
				for _, p := range res.Problems {
					fmt.Fprintf(os.Stderr, "  [%s] %s\n", p.Code, p.Message)
				}
			}
			return exitFailed
		}
		fmt.Fprintln(os.Stderr, "cdf2ms validate:", err)
		return exitUsage
	}
	if res != nil && len(res.Problems) > 0 {
		fmt.Fprintf(os.Stderr, "cdf2ms validate: %d problem(s)\n", len(res.Problems))
		for _, p := range res.Problems {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", p.Code, p.Message)
		}
		return exitFailed
	}
	return exitOK
}
