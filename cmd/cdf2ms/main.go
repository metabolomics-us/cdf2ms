// Command cdf2ms converts ANDI/MS (ASTM E1205/E1947) NetCDF corpora into mzML
// and mzXML without loading a whole file into memory.
//
// Usage:
//
//	cdf2ms inspect  FILE...              describe the NetCDF container
//	cdf2ms audit    FILE...              report ANDI conformance and convertibility
//	cdf2ms convert  [flags] PATH...      convert files or directories
//	cdf2ms fixtures DIR                  write synthetic ANDI files for testing
//	cdf2ms version                       print build information
//
// Exit codes are stable so scripts can rely on them:
//
//	0  success (warnings are reported but do not fail the run)
//	1  usage or flag error
//	2  at least one file failed to convert or is not convertible
//	3  interrupted by a signal before the run finished
//	4  --fail-on-warning tripped
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"
)

// version is overridden at build time with
// -ldflags "-X main.version=v1.2.3".
var version = "dev"

const (
	exitOK          = 0
	exitUsage       = 1
	exitFailed      = 2
	exitInterrupted = 3
	exitWarned      = 4
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage())
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Print(usage())
		return exitOK
	case "inspect":
		return cmdInspect(ctx, rest)
	case "audit":
		return cmdAudit(ctx, rest)
	case "convert":
		return cmdConvert(ctx, rest)
	case "fixtures":
		return cmdFixtures(ctx, rest)
	case "report":
		return cmdReport(rest)
	case "version":
		return cmdVersion(rest)
	}
	fmt.Fprintf(os.Stderr, "cdf2ms: unknown command %q\n\n%s", cmd, usage())
	return exitUsage
}

func usage() string {
	return `cdf2ms - ANDI/MS NetCDF to mzML/mzXML converter (pure Go, no cgo)

Usage:
  cdf2ms inspect [-json] [-globals] FILE...
  cdf2ms audit   [-json] [-deep] [-rt-unit auto|strict|seconds|minutes] FILE...
  cdf2ms convert [-format mzML,mzXML] [-out-dir DIR] [-overwrite] [-workers N]
                 [-rt-unit auto|strict|seconds|minutes] [-precision auto|f32|f64]
                 [-compress] [-compression-level N] [-recursive]
                 [-max-source-bytes SIZE] [-verify] [-verify-tolerance X]
                 [-report-json PATH] [-json] PATH...
  cdf2ms fixtures DIR [-files N] [-scans N] [-points N] [-variant NAME]
  cdf2ms report summarize FILE.jsonl   aggregate a conversion journal
  cdf2ms version

Run 'cdf2ms COMMAND -h' for the flags of a single command.
`
}

// newFlagSet builds a flag set whose usage output goes to stderr and that fails
// loudly on unknown flags instead of silently ignoring operator intent.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// jsonOut prints v as indented JSON.
func jsonOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdVersion(args []string) int {
	fs := newFlagSet("version")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	info := buildInfo()
	if *jsonFlag {
		if err := jsonOut(os.Stdout, info); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms:", err)
			return exitUsage
		}
		return exitOK
	}
	fmt.Printf("cdf2ms %s\n", info.Version)
	fmt.Printf("  module:   %s\n", info.Module)
	fmt.Printf("  revision: %s\n", orNA(info.Revision))
	fmt.Printf("  built:    %s\n", orNA(info.BuildDate))
	fmt.Printf("  go:       %s (%s/%s)\n", info.GoVersion, runtime.GOOS, runtime.GOARCH)
	fmt.Printf("  cgo:      disabled (pure Go; the NetCDF reader is built in)\n")
	fmt.Printf("  output:   mzML 1.1.0, mzXML 3.2\n")
	fmt.Printf("  psi-ms:   CV %s\n", psiMSVersion())
	return exitOK
}

type versionInfo struct {
	Version   string `json:"version"`
	Module    string `json:"module"`
	Revision  string `json:"revision,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	PureGo    bool   `json:"pureGo"`
	MzML      string `json:"mzml"`
	MzXML     string `json:"mzxml"`
	PSIMS     string `json:"psiMs"`
}

func buildInfo() versionInfo {
	info := versionInfo{
		Version:   version,
		Module:    "github.com/metabolomics-us/cdf2ms",
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		PureGo:    true,
		MzML:      "1.1.0",
		MzXML:     "3.2",
		PSIMS:     psiMSVersion(),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Path != "" {
			info.Module = bi.Main.Path
		}
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" && version == "dev" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Revision = s.Value
			case "vcs.time":
				info.BuildDate = s.Value
			}
		}
	}
	return info
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}

// parseSize accepts "512MB", "2GiB", "1.5 GB" and bare byte counts, because
// operators type all three and silently misreading a size limit is worse than an
// error message.
func parseSize(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(t, "kb"), strings.HasSuffix(t, "k"):
		mult = 1 << 10
	case strings.HasSuffix(t, "mb"), strings.HasSuffix(t, "m"):
		mult = 1 << 20
	case strings.HasSuffix(t, "gb"), strings.HasSuffix(t, "g"):
		mult = 1 << 30
	case strings.HasSuffix(t, "tb"), strings.HasSuffix(t, "t"):
		mult = 1 << 40
	case strings.HasSuffix(t, "b"):
		t = strings.TrimSuffix(t, "b")
	}
	t = strings.TrimRight(t, " ")
	var f float64
	if _, err := fmt.Sscanf(t, "%g", &f); err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if f < 0 {
		return 0, fmt.Errorf("invalid size %q: negative", s)
	}
	return int64(f * float64(mult)), nil
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

// sortedKeys returns map keys in lexical order for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// deadline String helpers shared by the commands.
func formatDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
