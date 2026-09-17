package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// reportRecordOut is the subset of a JSONL record that summarize reads. It is
// deliberately lenient about which fields exist so a partial or hand-edited
// journal still summarizes rather than crashing.
type reportRecordOut struct {
	Source     string `json:"source"`
	Status     string `json:"status"`
	ErrorCode  string `json:"error_code"`
	ScanCount  int64  `json:"scan_count"`
	PointCount int64  `json:"point_count"`
	ReadBytes  int64  `json:"read_bytes"`
	WriteBytes int64  `json:"write_bytes"`
	DurationMS int64  `json:"duration_ms"`
	Warnings   []struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"warnings"`
	Outputs []struct {
		Format string `json:"format"`
		Path   string `json:"path"`
		Bytes  int64  `json:"bytes"`
	} `json:"outputs"`
}

// cmdReport implements "cdf2ms report summarize FILE.jsonl": aggregate a
// streaming conversion journal into a compact summary without needing the
// source files to still exist.
func cmdReport(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms report: subcommand required (summarize)")
		return exitUsage
	}
	switch args[0] {
	case "summarize":
		return cmdReportSummarize(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "cdf2ms report: unknown subcommand %q (want summarize)\n", args[0])
		return exitUsage
	}
}

func cmdReportSummarize(args []string) int {
	fs := newFlagSet("report summarize")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms report summarize: at least one JSONL file is required")
		return exitUsage
	}

	agg := summaryAgg{}
	for _, p := range paths {
		fh, err := os.Open(p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms report summarize:", err)
			return exitUsage
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 || line[0] == '#' {
				continue
			}
			var rec reportRecordOut
			if err := json.Unmarshal(line, &rec); err != nil {
				continue
			}
			agg.add(rec)
		}
		fh.Close()
		if err := sc.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms report summarize:", p, err)
			return exitUsage
		}
	}

	if *jsonFlag {
		if err := jsonOut(os.Stdout, agg); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms report summarize:", err)
			return exitUsage
		}
		return exitOK
	}
	agg.print(os.Stdout)
	return exitOK
}

type summaryAgg struct {
	Files    int   `json:"files"`
	Success  int   `json:"success"`
	Warning  int   `json:"warning"`
	Failed   int   `json:"failed"`
	Skipped  int   `json:"skipped"`
	Spectra  int64 `json:"spectra"`
	Points   int64 `json:"points"`
	ReadMB   int64 `json:"readMB"`
	WriteMB  int64 `json:"writeMB"`
	Duration int64 `json:"durationMs"`

	// perCode counts warning codes; perErr counts error codes.
	perCode map[msdata.Code]int `json:"-"`
	perErr  map[string]int      `json:"-"`
	// failures carries the most frequent failure detail snippets for humans.
	failures map[string]int `json:"-"`
}

func (a *summaryAgg) add(rec reportRecordOut) {
	a.Files++
	switch rec.Status {
	case "success":
		a.Success++
	case "warning":
		a.Warning++
	case "failed":
		a.Failed++
	case "converted":
		a.Success++
	default:
		a.Skipped++
	}
	a.Spectra += rec.ScanCount
	a.Points += rec.PointCount
	a.ReadMB += rec.ReadBytes / (1 << 20)
	a.WriteMB += rec.WriteBytes / (1 << 20)
	a.Duration += rec.DurationMS
	for _, w := range rec.Warnings {
		if a.perCode == nil {
			a.perCode = map[msdata.Code]int{}
		}
		a.perCode[msdata.Code(w.Code)]++
	}
	if rec.ErrorCode != "" {
		if a.perErr == nil {
			a.perErr = map[string]int{}
		}
		a.perErr[rec.ErrorCode]++
	}
}

func (a *summaryAgg) print(w *os.File) {
	fmt.Fprintf(w, "records:      %d\n", a.Files)
	fmt.Fprintf(w, "  success:    %d\n", a.Success)
	fmt.Fprintf(w, "  warning:    %d\n", a.Warning)
	fmt.Fprintf(w, "  failed:     %d\n", a.Failed)
	fmt.Fprintf(w, "  skipped:    %d\n", a.Skipped)
	fmt.Fprintf(w, "spectra:      %d\n", a.Spectra)
	fmt.Fprintf(w, "points:       %d\n", a.Points)
	fmt.Fprintf(w, "read:         %d MiB\n", a.ReadMB)
	fmt.Fprintf(w, "written:      %d MiB\n", a.WriteMB)
	fmt.Fprintf(w, "wall time:    %d ms\n", a.Duration)
	if len(a.perErr) > 0 {
		fmt.Fprintln(w, "error codes:")
		for _, c := range sortedStrKeys(a.perErr) {
			fmt.Fprintf(w, "  %-32s %d\n", c, a.perErr[c])
		}
	}
	if len(a.perCode) > 0 {
		codes := make([]msdata.Code, 0, len(a.perCode))
		for c := range a.perCode {
			codes = append(codes, c)
		}
		sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
		fmt.Fprintln(w, "warning codes:")
		for _, c := range codes {
			fmt.Fprintf(w, "  %-36s %d\n", c, a.perCode[c])
		}
	}
}

func sortedStrKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
