package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/metabolomics-us/cdf2ms/pkg/testutil"
)

// fixtureVariant is one synthetic ANDI/MS file shape. These exist so a CI job, a
// bug report, or a new contributor can reproduce an awkward layout without
// shipping a proprietary vendor file, and so the reader's behaviour on each shape
// is asserted somewhere.
type fixtureVariant struct {
	Name        string
	Description string
	Expectation string
	Options     testutil.ANDIOptions
}

func fixtureVariants() []fixtureVariant {
	return []fixtureVariant{
		{
			Name:        "plain",
			Description: "canonical GC/MS export: units declared in seconds, index and point counts present",
			Expectation: "converts unattended",
			Options: testutil.ANDIOptions{
				ScanCount: 60, PointsPerScan: []int{240}, RTUnit: "second",
				RTValueScale: 1, IncludeInstrumentVars: true, IncludeTimeValues: true,
				GlobalAttributes: map[string]string{"dataset_name": "plain", "ms_template_revision": "1.0.1"},
			},
		},
		{
			Name:        "minutes",
			Description: "scan_acquisition_time declared in minutes",
			Expectation: "converts; retention times are rescaled to seconds",
			Options: testutil.ANDIOptions{
				ScanCount: 40, PointsPerScan: []int{200}, RTUnit: "minute", RTValueScale: 1,
				IncludeInstrumentVars: true,
				GlobalAttributes:      map[string]string{"dataset_name": "minutes"},
			},
		},
		{
			Name:        "unitless-ambiguous",
			Description: "no units attribute anywhere and a scan spacing that fits both seconds and minutes",
			Expectation: "refused until --rt-unit states the clock",
			Options: testutil.ANDIOptions{
				ScanCount: 40, PointsPerScan: []int{200}, RTUnit: "", RTValueScale: 0.2,
				GlobalAttributes: map[string]string{"dataset_name": "unitless-ambiguous"},
			},
		},
		{
			Name:        "cdf5",
			Description: "CDF-5 container with 64-bit widths in the header",
			Expectation: "converts",
			Options: testutil.ANDIOptions{
				Version: 5, ScanCount: 30, PointsPerScan: []int{180}, RTUnit: "second",
				IncludeInstrumentVars: true,
				GlobalAttributes:      map[string]string{"dataset_name": "cdf5"},
			},
		},
		{
			Name:        "cdf2",
			Description: "CDF-2 (64-bit offsets) container",
			Expectation: "converts",
			Options: testutil.ANDIOptions{
				Version: 2, ScanCount: 30, PointsPerScan: []int{180}, RTUnit: "second",
				IncludeInstrumentVars: true,
				GlobalAttributes:      map[string]string{"dataset_name": "cdf2"},
			},
		},
		{
			Name:        "packed",
			Description: "integer payloads with scale_factor and add_offset (netCDF packing convention)",
			Expectation: "converts; physical values are unpacked and the scaling is reported",
			Options: testutil.ANDIOptions{
				ScanCount: 25, PointsPerScan: []int{160}, RTUnit: "second", Packing: true,
				IncludeInstrumentVars: true,
				GlobalAttributes:      map[string]string{"dataset_name": "packed"},
			},
		},
		{
			Name:        "no-scan-index",
			Description: "scan_index absent; spans must be derived from point_count",
			Expectation: "converts",
			Options: testutil.ANDIOptions{
				ScanCount: 25, PointsPerScan: []int{140, 150, 160}, RTUnit: "second",
				OmitScanIndex: true, GlobalAttributes: map[string]string{"dataset_name": "no-scan-index"},
			},
		},
		{
			Name:        "no-point-count",
			Description: "point_count absent; counts must be derived from scan_index deltas",
			Expectation: "converts",
			Options: testutil.ANDIOptions{
				ScanCount: 25, PointsPerScan: []int{140}, RTUnit: "second",
				OmitPointCount: true, GlobalAttributes: map[string]string{"dataset_name": "no-point-count"},
			},
		},
		{
			Name:        "one-based-index",
			Description: "Fortran-style scan_index starting at 1",
			Expectation: "converts with the base recorded",
			Options: testutil.ANDIOptions{
				ScanCount: 20, PointsPerScan: []int{120}, RTUnit: "second", ScanIndexBase: 1,
				GlobalAttributes: map[string]string{"dataset_name": "one-based-index"},
			},
		},
		{
			Name:        "zero-point-scans",
			Description: "scans with no points at all",
			Expectation: "converts; empty spectra are nulled and reported",
			Options: testutil.ANDIOptions{
				ScanCount: 12, PointsPerScan: []int{0}, ZeroScans: true, RTUnit: "second",
				GlobalAttributes: map[string]string{"dataset_name": "zero-point-scans"},
			},
		},
		{
			Name:        "mixed-msn",
			Description: "per-scan ms_level switching between MS1 and MS2",
			Expectation: "converts; MS2 scans carry their precursor data",
			Options: testutil.ANDIOptions{
				ScanCount: 36, PointsPerScan: []int{150}, RTUnit: "second", MSLevelVar: true,
				GlobalAttributes: map[string]string{"dataset_name": "mixed-msn"},
			},
		},
		{
			Name:        "nonutf8-metadata",
			Description: "invalid UTF-8 in a global attribute (typical of legacy Windows exports)",
			Expectation: "converts; the value is sanitised and the substitution reported",
			Options: testutil.ANDIOptions{
				ScanCount: 15, PointsPerScan: []int{100}, RTUnit: "second", NonUTF8Title: true,
				GlobalAttributes: map[string]string{"dataset_name": "nonutf8-metadata"},
			},
		},
		{
			Name:        "irregular-points",
			Description: "wide, uneven peak counts including an empty scan in the middle",
			Expectation: "converts",
			Options: testutil.ANDIOptions{
				ScanCount: 24, PointsPerScan: []int{17, 4096, 3, 0, 88, 1200}, RTUnit: "second",
				IncludeTimeValues: true,
				GlobalAttributes:  map[string]string{"dataset_name": "irregular-points"},
			},
		},
	}
}

func cmdFixtures(ctx context.Context, args []string) int {
	fs := newFlagSet("fixtures")
	count := fs.Int("files", 1, "number of files to write per selected variant")
	scans := fs.Int("scans", 0, "override the scan count of every variant")
	points := fs.Int("points", 0, "override the peak count of every variant")
	only := fs.String("variant", "", "comma-separated variant names (default: all)")
	manifest := fs.String("manifest", "", "also write a JSON manifest to this path")
	jsonOutFlag := fs.Bool("json", false, "print the manifest as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "cdf2ms fixtures: exactly one output directory is required")
		return exitUsage
	}
	dir := fs.Arg(0)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cdf2ms fixtures:", err)
		return exitUsage
	}
	want := map[string]bool{}
	for _, name := range splitList(*only) {
		want[name] = true
	}

	type entry struct {
		File        string `json:"file"`
		Variant     string `json:"variant"`
		Description string `json:"description"`
		Expectation string `json:"expectation"`
		Scans       int    `json:"scans"`
		Peaks       string `json:"peaksPerScan"`
		Bytes       int64  `json:"bytes"`
		Err         string `json:"error,omitempty"`
	}
	var items []entry
	bad := 0
	for _, v := range fixtureVariants() {
		if len(want) > 0 && !want[v.Name] {
			continue
		}
		opt := v.Options
		if *scans > 0 {
			opt.ScanCount = *scans
			if opt.ScanNumbers != nil {
				opt.ScanNumbers = nil
			}
		}
		if *points > 0 {
			opt.PointsPerScan = []int{*points}
		}
		if opt.ScanCount <= 0 {
			opt.ScanCount = 10
		}
		for i := 0; i < *count; i++ {
			if ctx.Err() != nil {
				return exitInterrupted
			}
			name := fmt.Sprintf("%s-%02d.cdf", v.Name, i+1)
			path := filepath.Join(dir, name)
			spec, err := testutil.ANDI(opt)
			if err != nil {
				fmt.Fprintln(os.Stderr, "cdf2ms fixtures:", err)
				bad++
				continue
			}
			if err := testutil.WriteNetCDF(path, spec); err != nil {
				items = append(items, entry{File: path, Variant: v.Name, Err: err.Error()})
				bad++
				continue
			}
			st, _ := os.Stat(path)
			items = append(items, entry{
				File: path, Variant: v.Name, Description: v.Description, Expectation: v.Expectation,
				Scans: opt.ScanCount, Peaks: peaksNote(opt.PointsPerScan),
				Bytes: sizeOf(st),
			})
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms fixtures: no variant matched --variant")
		return exitUsage
	}
	if *manifest != "" {
		if err := writeManifest(*manifest, items); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms fixtures:", err)
			return exitUsage
		}
	}
	if *jsonOutFlag {
		if err := jsonOut(os.Stdout, items); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms fixtures:", err)
			return exitUsage
		}
	} else {
		for _, it := range items {
			if it.Err != "" {
				fmt.Printf("!! %-44s %s\n", it.File, it.Err)
				continue
			}
			fmt.Printf("ok %-44s %8s  scans=%d peaks=%s\n", it.File, humanBytes(it.Bytes), it.Scans, it.Peaks)
			fmt.Printf("     %s\n     expects: %s\n", it.Description, it.Expectation)
		}
		fmt.Printf("wrote %d fixture file(s) to %s\n", len(items)-bad, dir)
	}
	if bad > 0 {
		return exitFailed
	}
	return exitOK
}

func peaksNote(points []int) string {
	if len(points) == 0 {
		return "0"
	}
	if len(points) == 1 {
		return fmt.Sprintf("%d", points[0])
	}
	sorted := append([]int(nil), points...)
	sort.Ints(sorted)
	return fmt.Sprintf("%d..%d", sorted[0], sorted[len(sorted)-1])
}

func sizeOf(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

func writeManifest(path string, items any) error {
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
