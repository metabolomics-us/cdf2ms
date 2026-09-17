package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
	"github.com/metabolomics-us/cdf2ms/pkg/netcdfio"
)

type inspectDim struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Unlimited bool   `json:"unlimited,omitempty"`
}

type inspectVar struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Dims   []string `json:"dims,omitempty"`
	Len    int64    `json:"elements"`
	VSize  int64    `json:"vsizeBytes"`
	Begin  int64    `json:"begin"`
	Record bool     `json:"record,omitempty"`
	Attrs  int      `json:"attributes"`
	Units  string   `json:"units,omitempty"`
	Fill   string   `json:"fillValue,omitempty"`
	Scale  string   `json:"scaleFactor,omitempty"`
	AddOff string   `json:"addOffset,omitempty"`
}

type inspectAttr struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type inspectReport struct {
	Path       string        `json:"path"`
	Container  string        `json:"container"`
	Magic      string        `json:"magic"`
	Size       int64         `json:"sizeBytes"`
	NumRecs    int64         `json:"records"`
	RecSize    int64         `json:"recordSizeBytes"`
	Dimensions []inspectDim  `json:"dimensions,omitempty"`
	Globals    []inspectAttr `json:"globals,omitempty"`
	Variables  []inspectVar  `json:"variables,omitempty"`
	Notes      []string      `json:"notes,omitempty"`
	Err        string        `json:"error,omitempty"`
	Code       string        `json:"errorCode,omitempty"`
}

func cmdInspect(ctx context.Context, args []string) int {
	fs := newFlagSet("inspect")
	jsonFlag := fs.Bool("json", false, "emit JSON instead of a table")
	full := fs.Bool("globals", false, "print attribute values in full instead of truncating them")
	maxVars := fs.Int("vars", 0, "limit the number of variables listed (0 = all)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	paths := fs.Args()
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "cdf2ms inspect: at least one file is required")
		return exitUsage
	}

	reports := make([]inspectReport, 0, len(paths))
	bad := 0
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		rep := inspectOne(p, *full, *maxVars)
		reports = append(reports, rep)
		if rep.Err != "" {
			bad++
		}
	}
	if *jsonFlag {
		if err := jsonOut(os.Stdout, reports); err != nil {
			fmt.Fprintln(os.Stderr, "cdf2ms inspect:", err)
			return exitUsage
		}
	} else {
		for _, rep := range reports {
			printInspect(os.Stdout, rep)
		}
	}
	if bad > 0 {
		return exitFailed
	}
	return exitOK
}

func inspectOne(path string, full bool, maxVars int) inspectReport {
	rep := inspectReport{Path: path}
	enc, magic, err := netcdfio.Detect(path)
	rep.Magic = magic
	rep.Container = enc.String()
	if st, serr := os.Stat(path); serr == nil {
		rep.Size = st.Size()
	}
	if errors.Is(err, netcdfio.ErrUnsupportedEncoding) {
		// The container was identified even though no reader here can decode it;
		// saying so beats pretending the file is empty.
		rep.Err = err.Error()
		rep.Code = string(msdata.CodeNCUnsupportedEncoding)
		rep.Notes = append(rep.Notes,
			"container identified; cdf2ms converts classic CDF-1/2/5 only (see docs/compatibility.md)")
		return rep
	}
	if err != nil {
		rep.Err = err.Error()
		rep.Code = string(msdata.CodeNCOpenFailed)
		return rep
	}
	f, err := netcdfio.Open(path)
	if err != nil {
		rep.Err = err.Error()
		rep.Code = string(msdata.CodeOf(err))
		if rep.Code == "" {
			rep.Code = string(msdata.CodeNCHeaderCorrupt)
		}
		return rep
	}
	defer f.Close()

	rep.Container = f.Encoding().String()
	rep.Magic = printableMagic(f.Magic())
	rep.Size = f.FileSize()
	rep.NumRecs = f.NumRecs
	rep.RecSize = f.RecSize
	for _, d := range f.Dimensions {
		rep.Dimensions = append(rep.Dimensions, inspectDim{Name: d.Name, Size: d.Size, Unlimited: d.Unlimited})
	}
	for _, name := range sortedKeys(f.Globals) {
		a := f.Globals[name]
		rep.Globals = append(rep.Globals, inspectAttr{Name: name, Kind: a.Kind(), Value: clip(a.Value(), full)})
	}
	names := f.VarNames()
	listed := names
	if maxVars > 0 && len(listed) > maxVars {
		listed = listed[:maxVars]
		rep.Notes = append(rep.Notes, fmt.Sprintf("%d of %d variables shown (--vars)", maxVars, len(names)))
	}
	for _, name := range listed {
		v, ok := f.Vars[name]
		if !ok || v == nil {
			continue
		}
		iv := inspectVar{
			Name:   v.Name,
			Type:   v.Type.String(),
			Dims:   v.Dims,
			Len:    v.Len,
			VSize:  v.VSize,
			Begin:  v.Begin,
			Record: v.IsRecord,
			Attrs:  len(v.Attrs),
		}
		for _, key := range []struct {
			attr string
			dst  *string
		}{{"units", &iv.Units}, {"_FillValue", &iv.Fill}, {"scale_factor", &iv.Scale}, {"add_offset", &iv.AddOff}} {
			if a, ok := v.Attrs[key.attr]; ok {
				*key.dst = clip(a.Value(), true)
			}
		}
		rep.Variables = append(rep.Variables, iv)
	}
	return rep
}

func printableMagic(m string) string {
	// The magic contains a raw version byte; render it readably.
	r := strings.NewReplacer("\x01", "1", "\x02", "2", "\x05", "5", "\x89", "89 ")
	return r.Replace(m)
}

func clip(s string, full bool) string {
	if full || len(s) <= 72 {
		return s
	}
	return s[:69] + "..."
}

func printInspect(w *os.File, rep inspectReport) {
	fmt.Fprintf(w, "%s\n", rep.Path)
	fmt.Fprintf(w, "  container: %s  magic: %q  size: %s\n", rep.Container, rep.Magic, humanBytes(rep.Size))
	if rep.Err != "" {
		fmt.Fprintf(w, "  ERROR [%s] %s\n", rep.Code, rep.Err)
		for _, n := range rep.Notes {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
		return
	}
	fmt.Fprintf(w, "  records: %d  record size: %s\n", rep.NumRecs, humanBytes(rep.RecSize))
	if len(rep.Dimensions) > 0 {
		fmt.Fprint(w, "  dimensions:")
		for _, d := range rep.Dimensions {
			if d.Unlimited {
				fmt.Fprintf(w, " %s=%d(unlimited)", d.Name, d.Size)
			} else {
				fmt.Fprintf(w, " %s=%d", d.Name, d.Size)
			}
		}
		fmt.Fprintln(w)
	}
	if len(rep.Globals) > 0 {
		fmt.Fprintf(w, "  globals (%d):\n", len(rep.Globals))
		for _, a := range rep.Globals {
			fmt.Fprintf(w, "    %-28s %-6s %s\n", a.Name, a.Kind, a.Value)
		}
	}
	if len(rep.Variables) > 0 {
		fmt.Fprintf(w, "  variables (%d):\n", len(rep.Variables))
		fmt.Fprintf(w, "    %-26s %-6s %-24s %10s %10s %s\n", "NAME", "TYPE", "DIMS", "ELEMENTS", "VSIZE", "FLAGS")
		for _, v := range rep.Variables {
			flags := make([]string, 0, 4)
			if v.Record {
				flags = append(flags, "record")
			}
			if v.Scale != "" || v.AddOff != "" {
				flags = append(flags, "packed")
			}
			if v.Fill != "" {
				flags = append(flags, "fill="+v.Fill)
			}
			fmt.Fprintf(w, "    %-26s %-6s %-24s %10d %10s %s\n",
				v.Name, v.Type, strings.Join(v.Dims, ","), v.Len, humanBytes(v.VSize), strings.Join(flags, " "))
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
}
