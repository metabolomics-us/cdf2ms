package mzml

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// TestCVTermsMatchOfficialOntology checks every term the writer can emit against
// a committed snapshot of psi-ms.obo: the accession must exist, must not be
// obsolete, and the name must match character for character.
//
// A cvParam whose name disagrees with its accession is the single most common
// way to produce an mzML file that downstream tools reject in confusing ways.
func TestCVTermsMatchOfficialOntology(t *testing.T) {
	path := filepath.Join("testdata", "psi_ms_terms.tsv")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open snapshot: %v (regenerate with scripts/fetch_psi_ms_cv.sh)", err)
	}
	defer f.Close()

	onto := map[string]struct{ name, flag string }{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		flag := ""
		if len(parts) > 2 {
			flag = parts[2]
		}
		onto[parts[0]] = struct{ name, flag string }{parts[1], flag}
	}
	if len(onto) < 40 {
		t.Fatalf("snapshot looks truncated (%d terms)", len(onto))
	}

	for _, pair := range UsedTerms() {
		acc, name := pair[0], pair[1]
		e, ok := onto[acc]
		if !ok {
			t.Errorf("%s (%q) is not in the ontology snapshot; run scripts/fetch_psi_ms_cv.sh", acc, name)
			continue
		}
		if e.flag == "obsolete" {
			t.Errorf("%s (%q) is obsolete in the ontology", acc, name)
		}
		if e.name != name {
			t.Errorf("%s: emitted name %q, ontology name %q", acc, name, e.name)
		}
	}
}

// TestNoFabricatedBase64Term documents a deliberate omission: "base64 array" is
// not a term in the current PSI-MS ontology, so emitting it (as many older
// converters do) produces a cvParam that cannot be resolved.
func TestNoFabricatedBase64Term(t *testing.T) {
	for _, pair := range UsedTerms() {
		if strings.EqualFold(pair[1], "base64 array") {
			t.Errorf("writer emits %s/%s which is absent from psi-ms.obo", pair[0], pair[1])
		}
	}
}

// nativeIDPattern is the restriction the mzML 1.1 schema puts on
// <spectrum id>: "\S+=\S+( \S+=\S+)*".
var nativeIDPattern = regexp.MustCompile(`^\S+=\S+( \S+=\S+)*$`)

// TestSpectrumIDMatchesSchemaPattern guards the @id shape requirement.
func TestSpectrumIDMatchesSchemaPattern(t *testing.T) {
	n := int64(42)
	cases := []*msdata.Spectrum{
		{Index: 0, ScanNumber: &n},
		{Index: 7},
		{Index: 8, ScanNumber: ptrInt64(-3)},
	}
	for _, sp := range cases {
		id := spectrumID(sp)
		if !nativeIDPattern.MatchString(id) {
			t.Errorf("spectrumID(%+v) = %q does not match %s", sp.Index, id, nativeIDPattern)
		}
	}
}

func ptrInt64(v int64) *int64 { return &v }
