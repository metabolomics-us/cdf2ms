package mzxml

import (
	"os"
	"strings"
	"testing"
)

// The generated document is checked against the committed official schema by
// lxml in CI (scripts/validate_ms_outputs.py). That needs Python and lxml, so the
// same conformance question is also answered here, in pure Go, using the very
// same schema files: the required attribute sets, permitted enumeration values,
// datatype restrictions, and the child-element content models (including nested
// repeating sequences) are read out of testdata/schema at test time.

// TestGeneratedDocumentConformsToOfficialSchema is the primary gate: any drift in
// the writer's output is reported as a schema violation, not as a diff against
// hand-written expectations.
func TestGeneratedDocumentConformsToOfficialSchema(t *testing.T) {
	s := loadSchema(t)
	assertSchemaBasics(t, s)

	path, _ := writeDoc(t, Options{SoftwareVersion: "9.9"},
		spectrum(0, []float64{50.5, 100.25}, []float64{1, 2.5}),
		spectrum(1, []float64{50.5, 100.25}, []float64{1, 2.5}),
		spectrum(2, nil, nil))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if probs := s.validate(parseTree(t, raw)); len(probs) != 0 {
		t.Fatalf("generated mzXML violates the official schema:\n  %s", strings.Join(probs, "\n  "))
	}
}

func TestGeneratedCompressedDocumentConformsToOfficialSchema(t *testing.T) {
	s := loadSchema(t)
	n := 50
	mz := make([]float64, n)
	inten := make([]float64, n)
	for i := range mz {
		mz[i], inten[i] = float64(i)+0.5, float64(i)
	}
	path, _ := writeDoc(t, Options{Compress: true},
		spectrum(0, mz, inten), spectrum(1, mz, inten))
	raw, _ := os.ReadFile(path)
	if probs := s.validate(parseTree(t, raw)); len(probs) != 0 {
		t.Fatalf("compressed document violates the schema:\n  %s", strings.Join(probs, "\n  "))
	}
}

func TestManyProvenanceCommentsStaySchemaLegal(t *testing.T) {
	// mzXML's dataProcessing model is software followed by (processingOperation,
	// comment?) repeated: each repetition REQUIRES a processingOperation, so a
	// block of trailing comments is invalid no matter how many operations came
	// before. Provenance therefore collapses into one comment element.
	s := loadSchema(t)
	extra := []string{
		"extra note one",
		"extra note two",
		"extra note three & <dangerous> text",
	}
	path, _ := writeDoc(t, Options{ExtraComments: extra},
		spectrum(0, []float64{1}, []float64{2}))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if probs := s.validate(parseTree(t, raw)); len(probs) != 0 {
		t.Fatalf("provenance comments broke the dataProcessing content model:\n  %s",
			strings.Join(probs, "\n  "))
	}
	for _, want := range extra {
		if !strings.Contains(string(raw), xmlText(want)) {
			t.Fatalf("provenance comment %q was dropped", want)
		}
	}
}

// TestSchemaHarnessRejectsKnownBadDocuments proves the harness has teeth: each
// case is a real generated document with exactly one thing broken.
func TestSchemaHarnessRejectsKnownBadDocuments(t *testing.T) {
	s := loadSchema(t)
	path, _ := writeDoc(t, Options{},
		spectrum(0, []float64{50.5, 100.25}, []float64{1, 2.5}),
		spectrum(1, []float64{50.5, 100.25}, []float64{1, 2.5}))
	base, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if probs := s.validate(parseTree(t, base)); len(probs) != 0 {
		t.Fatalf("the reference document should be valid before mutation:\n  %s",
			strings.Join(probs, "\n  "))
	}

	cases := []struct {
		name  string
		from  string
		to    string
		wants string
		regex bool
	}{
		{
			name:  "undeclared element",
			from:  "<scan ",
			to:    "<bogus/><scan ",
			wants: "do not satisfy the content model",
		},
		{
			name:  "spectrum element is not mzXML",
			from:  "<scan ",
			to:    "<spectrum/><scan ",
			wants: "do not satisfy the content model",
		},
		{
			name:  "missing required peaks attribute",
			from:  ` compressionType="none"`,
			to:    "",
			wants: "required attribute",
		},
		{
			name:  "wrong byte order",
			from:  `byteOrder="network"`,
			to:    `byteOrder="little"`,
			wants: "fixed value",
		},
		{
			name:  "msLevel zero",
			from:  `msLevel="1"`,
			to:    `msLevel="0"`,
			wants: "positiveInteger",
		},
		{
			name:  "scan num zero",
			from:  `num="1"`,
			to:    `num="0"`,
			wants: "positiveInteger",
		},
		{
			name:  "retention time is not an ISO 8601 duration",
			from:  `retentionTime="PT0.25S"`,
			to:    `retentionTime="00:01.5"`,
			wants: "xs:duration",
		},
		{
			name:  "bogus attribute",
			from:  ` peaksCount="2"`,
			to:    ` peaksCount="2" bogus="x"`,
			wants: "is not declared",
		},
		{
			name:  "bad contentType",
			from:  `contentType="m/z-int"`,
			to:    `contentType="intensity-m/z"`,
			wants: "permitted values",
		},
		{
			// scanCount is optional in the schema, but a declared count of zero is
			// not a positiveInteger and contradicts msRun's requirement for a scan.
			name:  "scanCount zero",
			from:  ` scanCount="2"`,
			to:    ` scanCount="0"`,
			wants: "positiveInteger",
		},
		{
			name:  "missing indexOffset",
			from:  `<indexOffset>[0-9]+</indexOffset>`,
			to:    "",
			wants: "do not satisfy the content model",
			regex: true,
		},
		{
			name:  "sha1 wrong length",
			from:  `<sha1>[0-9a-f]{40}</sha1>`,
			to:    `<sha1>abc</sha1>`,
			wants: "exactly 40 characters",
			regex: true,
		},
		{
			// dataProcessing allows (processingOperation, comment?) repeated, so a
			// second comment with no operation in front of it is invalid.
			name:  "two comments in one repetition",
			from:  `</comment>`,
			to:    `</comment>\n        <comment>duplicate</comment>`,
			wants: "do not satisfy the content model",
			regex: true,
		},
		{
			name:  "comment before its processingOperation",
			from:  `(<processingOperation)`,
			to:    `<comment>x</comment>$1`,
			wants: "do not satisfy the content model",
			regex: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mutated []byte
			if tc.regex {
				re := mustCompile(t, tc.from)
				if !re.Match(base) {
					t.Fatalf("mutation pattern %q does not match the reference document", tc.from)
				}
				mutated = re.ReplaceAll(base, []byte(tc.to))
			} else {
				if !strings.Contains(string(base), tc.from) {
					t.Fatalf("mutation target %q is not present in the reference document", tc.from)
				}
				mutated = []byte(strings.Replace(string(base), tc.from, tc.to, 1))
			}
			probs := s.validate(parseTree(t, mutated))
			if len(probs) == 0 {
				t.Fatalf("the schema harness accepted an invalid document\nmutation: %s -> %s", tc.from, tc.to)
			}
			joined := strings.Join(probs, "\n")
			if !strings.Contains(joined, tc.wants) {
				t.Fatalf("problems do not mention %q:\n%s", tc.wants, joined)
			}
		})
	}
}
