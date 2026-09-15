package mzxml

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests validate generated documents against the official mzXML 3.2 schema
// files committed under testdata/schema. The required attribute sets, permitted
// enumeration values, child-element order, and datatype restrictions are read
// out of the schema at test time, so the schema — not this file — decides what
// conformance means.

const xsiNS = "http://www.w3.org/2001/XMLSchema-instance"

// ---------- schema model ----------

type attrDecl struct {
	name     string
	use      string // required | optional | prohibited
	typ      string
	fixed    string
	enums    []string
	length   int
	minInc   string
	maxInc   string
	pattern  string
	hasRange bool
}

type decl struct {
	name      string
	attrs     map[string]attrDecl
	items     []item
	nillable  bool
	contentTy string    // simple content type, e.g. cff:strictBase64Type
	text      *attrDecl // restrictions on the element's own text content
	wildcard  bool      // the schema part we need was not resolvable
}

type item struct {
	d   *decl
	min int
	max int // -1 == unbounded
	// sub holds a nested xs:sequence, which repeats as a unit. Flattening it
	// would over-approximate the model and let an invalid child order through, so
	// it is matched as a group.
	sub []item
}

type schema struct {
	elements map[string]*decl
	types    map[string]*xsdComplexType
	src      map[string]*decl // memo for named types
}

// ---------- XSD subset decoding ----------

type xsdSchemaFile struct {
	Elements     []xsdElement       `xml:"element"`
	ComplexTypes []xsdComplexType   `xml:"complexType"`
	SimpleTypes  []xsdSimpleTypeDef `xml:"simpleType"`
}

type xsdElement struct {
	Name        string            `xml:"name,attr"`
	Type        string            `xml:"type,attr"`
	Ref         string            `xml:"ref,attr"`
	MinOccurs   string            `xml:"minOccurs,attr"`
	MaxOccurs   string            `xml:"maxOccurs,attr"`
	Nillable    string            `xml:"nillable,attr"`
	ComplexType *xsdComplexType   `xml:"complexType"`
	SimpleType  *xsdSimpleTypeDef `xml:"simpleType"`
}

type xsdComplexType struct {
	Name           string               `xml:"name,attr"`
	Sequence       *xsdSequence         `xml:"sequence"`
	All            *xsdSequence         `xml:"all"`
	Attributes     []xsdAttribute       `xml:"attribute"`
	SimpleContent  *xsdSimpleExtension  `xml:"simpleContent>extension"`
	ComplexContent *xsdComplexExtension `xml:"complexContent>extension"`
}

type xsdComplexExtension struct {
	Base       string         `xml:"base,attr"`
	Sequence   *xsdSequence   `xml:"sequence"`
	Attributes []xsdAttribute `xml:"attribute"`
}

type xsdSimpleExtension struct {
	Base       string         `xml:"base,attr"`
	Attributes []xsdAttribute `xml:"attribute"`
}

type xsdSequence struct {
	MinOccurs string        `xml:"minOccurs,attr"`
	MaxOccurs string        `xml:"maxOccurs,attr"`
	Elements  []xsdElement  `xml:"element"`
	Sequences []xsdSequence `xml:"sequence"`
}

type xsdAttribute struct {
	Name   string            `xml:"name,attr"`
	Use    string            `xml:"use,attr"`
	Type   string            `xml:"type,attr"`
	Fixed  string            `xml:"fixed,attr"`
	Simple *xsdSimpleTypeDef `xml:"simpleType"`
}

type xsdSimpleTypeDef struct {
	Restriction *xsdRestriction `xml:"restriction"`
}

type xsdRestriction struct {
	Base  string `xml:"base,attr"`
	Enums []struct {
		Value string `xml:"value,attr"`
	} `xml:"enumeration"`
	Length struct {
		Value int `xml:"value,attr"`
	} `xml:"length"`
	MinInclusive struct {
		Value string `xml:"value,attr"`
	} `xml:"minInclusive"`
	MaxInclusive struct {
		Value string `xml:"value,attr"`
	} `xml:"maxInclusive"`
	Pattern struct {
		Value string `xml:"value,attr"`
	} `xml:"pattern"`
}

func loadSchema(t *testing.T) *schema {
	t.Helper()
	dir := filepath.Join("testdata", "schema")
	// The include chain is mzXML_idx_3.2.xsd -> mzXML_3.2.xsd ->
	// {separation_technique_1.0.xsd, general_types_1.0.xsd}. Globals are merged,
	// which is what xs:include means for same-namespace schemas.
	for _, f := range []string{"mzXML_idx_3.2.xsd", "mzXML_3.2.xsd", "general_types_1.0.xsd", "separation_technique_1.0.xsd"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Skipf("schema file %s missing (run scripts/fetch_mzxml_schema.py): %v", f, err)
		}
	}
	s := &schema{elements: map[string]*decl{}, types: map[string]*xsdComplexType{}, src: map[string]*decl{}}
	slots := map[string]*decl{}
	globals := map[string]xsdElement{}
	var order []string
	for _, f := range []string{"general_types_1.0.xsd", "separation_technique_1.0.xsd", "mzXML_3.2.xsd", "mzXML_idx_3.2.xsd"} {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var x xsdSchemaFile
		if err := xml.Unmarshal(raw, &x); err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for i := range x.ComplexTypes {
			s.types[x.ComplexTypes[i].Name] = &x.ComplexTypes[i]
		}
		for _, e := range x.Elements {
			name := e.Name
			if name == "" {
				continue
			}
			// Later files win: mzXML_idx_3.2.xsd re-declares the <mzXML> root with
			// the index envelope, which is the declaration cdf2ms must satisfy.
			if _, seen := globals[name]; !seen {
				order = append(order, name)
			}
			// Pre-register the slot so sequences that reference this element
			// (xs:element ref="cff:software") resolve to the same declaration even
			// when the reference appears before the declaration in the file.
			slots[name] = &decl{name: name, attrs: map[string]attrDecl{}}
			// Publish the empty slot now so forward references resolve to it; the
			// content is filled in by the second pass below.
			s.elements[name] = slots[name]
			globals[name] = e
		}
	}
	for _, name := range order {
		built := s.declFor(globals[name], 0)
		slot := slots[name]
		*slot = *built
		slot.name = name
	}
	return s
}

func unref(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func occurs(s string, def int) int {
	switch s {
	case "":
		return def
	case "unbounded":
		return -1
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return def
		}
		return n
	}
}

func truthy(s string) bool { return s == "1" || s == "true" }

// building guards against self-referential types.
var building = map[string]bool{}

// declFor materialises a declaration for an element declaration.
func (s *schema) declFor(e xsdElement, depth int) *decl {
	if depth > 12 {
		return &decl{name: unref(e.Name), wildcard: true}
	}
	d := &decl{name: unref(e.Name), attrs: map[string]attrDecl{}, nillable: truthy(e.Nillable)}
	if e.Name == "" && e.Ref != "" {
		d.name = unref(e.Ref)
		if cached, ok := s.elements[d.name]; ok {
			return cached
		}
		// Forward reference: materialise from the named type if possible.
		d.wildcard = true
		return d
	}
	ct := e.ComplexType
	if ct == nil && e.Type != "" {
		key := unref(e.Type)
		if bt, ok := s.types[key]; ok {
			if building[key] {
				return &decl{name: d.name, attrs: d.attrs, wildcard: true}
			}
			building[key] = true
			named := s.fromComplexType(key, *bt, depth)
			delete(building, key)
			named.name = d.name // the element keeps its own name
			return named
		}
		// A simple type: text content, no children.
		d.contentTy = e.Type
		return d
	}
	if ct == nil {
		if r := simpleRestriction(e.SimpleType); r != nil {
			ad := attrDecl{name: "#text", use: "optional", typ: r.Base}
			for _, en := range r.Enums {
				ad.enums = append(ad.enums, en.Value)
			}
			ad.length = r.Length.Value
			ad.minInc, ad.maxInc = r.MinInclusive.Value, r.MaxInclusive.Value
			ad.hasRange = ad.minInc != "" || ad.maxInc != ""
			ad.pattern = r.Pattern.Value
			d.text = &ad
			return d
		}
		d.wildcard = true
		return d
	}
	built := s.fromComplexType(d.name, *ct, depth)
	built.nillable = d.nillable // nillability belongs to the element declaration
	return built
}

func (s *schema) fromComplexType(name string, ct xsdComplexType, depth int) *decl {
	d := &decl{name: name, attrs: map[string]attrDecl{}}
	attrs := ct.Attributes
	seq := ct.Sequence
	if seq == nil {
		seq = ct.All
	}
	if ct.SimpleContent != nil {
		d.contentTy = ct.SimpleContent.Base
		if unref(ct.SimpleContent.Base) == "strictBase64Type" {
			d.text = &attrDecl{name: "#text", use: "optional", typ: "strictBase64Type"}
		}
		attrs = append(attrs, ct.SimpleContent.Attributes...)
	}
	if ct.ComplexContent != nil {
		attrs = append(attrs, ct.ComplexContent.Attributes...)
		if base := unref(ct.ComplexContent.Base); base != "" {
			if bt, ok := s.types[base]; ok && !building[base] {
				building[base] = true
				bd := s.fromComplexType(base, *bt, depth)
				delete(building, base)
				for k, v := range bd.attrs {
					d.attrs[k] = v
				}
				d.items = append(d.items, bd.items...)
			}
		}
		if ct.ComplexContent.Sequence != nil {
			seq = ct.ComplexContent.Sequence
		}
	}
	for _, a := range attrs {
		ad := attrDecl{name: a.Name, use: a.Use, typ: a.Type, fixed: a.Fixed}
		if ad.use == "" {
			ad.use = "optional"
		}
		if r := simpleRestriction(a.Simple); r != nil {
			if ad.typ == "" {
				ad.typ = r.Base
			}
			for _, en := range r.Enums {
				ad.enums = append(ad.enums, en.Value)
			}
			ad.length = r.Length.Value
			ad.minInc, ad.maxInc = r.MinInclusive.Value, r.MaxInclusive.Value
			ad.hasRange = ad.minInc != "" || ad.maxInc != ""
			ad.pattern = r.Pattern.Value
		}
		d.attrs[ad.name] = ad
	}
	if seq != nil {
		d.items = append(d.items, s.itemsFromSequence(*seq, depth)...)
	}
	return d
}

func simpleRestriction(t *xsdSimpleTypeDef) *xsdRestriction {
	if t == nil {
		return nil
	}
	return t.Restriction
}

// itemsFromSequence turns an xs:sequence into items. A nested xs:sequence stays
// a group (item.sub) because it repeats as a unit: flattening it would accept
// child orders the schema forbids, which is exactly the class of bug this harness
// exists to catch.
func (s *schema) itemsFromSequence(seq xsdSequence, depth int) []item {
	var out []item
	for _, e := range seq.Elements {
		sub := s.declFor(e, depth+1)
		if e.Ref != "" {
			if g, ok := s.elements[unref(e.Ref)]; ok {
				sub = g
			}
		}
		out = append(out, item{d: sub, min: occurs(e.MinOccurs, 1), max: occurs(e.MaxOccurs, 1)})
	}
	for _, inner := range seq.Sequences {
		out = append(out, item{
			sub: s.itemsFromSequence(inner, depth+1),
			min: occurs(inner.MinOccurs, 1),
			max: occurs(inner.MaxOccurs, 1),
		})
	}
	return out
}

// ---------- document tree ----------

type xnode struct {
	name     string
	attrs    map[string]string
	children []*xnode
	text     string
}

func parseTree(t *testing.T, raw []byte) *xnode {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	var root *xnode
	var stack []*xnode
	for {
		tk, err := dec.Token()
		if err != nil {
			break
		}
		switch el := tk.(type) {
		case xml.StartElement:
			n := &xnode{name: el.Name.Local, attrs: map[string]string{}}
			for _, a := range el.Attr {
				if a.Name.Space == "xmlns" || a.Name.Space == "xml" || a.Name.Local == "xmlns" {
					continue // namespace declarations, not schema attributes
				}
				key := a.Name.Local
				if a.Name.Space == xsiNS || a.Name.Space == "xsi" {
					if a.Name.Local == "nil" {
						key = "xsi:nil"
					} else {
						continue // xsi:schemaLocation and friends are always permitted
					}
				}
				n.attrs[key] = a.Value
			}
			if len(stack) == 0 {
				root = n
			} else {
				top := stack[len(stack)-1]
				top.children = append(top.children, n)
			}
			stack = append(stack, n)
		case xml.CharData:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.text += string(el)
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if root == nil {
		t.Fatal("no root element parsed")
	}
	return root
}

// ---------- validation ----------

var durationRe = regexp.MustCompile(`^-?P((\d+Y)?(\d+M)?(\d+D)?(T(\d+H)?(\d+M)?(\d+(\.\d+)?S)?)?|(\d+)W)$`)

func (s *schema) validate(root *xnode) []string {
	var probs []string
	d, ok := s.elements[root.name]
	if !ok {
		return []string{fmt.Sprintf("root element <%s> is not declared by the mzXML 3.2 schema", root.name)}
	}
	if root.name != "mzXML" {
		probs = append(probs, "root element must be <mzXML>, got <"+root.name+">")
	}
	probs = append(probs, s.check(root, d, "")...)
	return probs
}

func (s *schema) check(n *xnode, d *decl, path string) []string {
	where := "/" + n.name
	if path != "" {
		where = path + where
	}
	if d == nil || d.wildcard {
		return nil
	}
	var probs []string

	if _, isNil := n.attrs["xsi:nil"]; isNil && !d.nillable {
		probs = append(probs, where+": xsi:nil used on a non-nillable element")
	}

	for k, v := range n.attrs {
		if strings.HasPrefix(k, "xsi:") {
			continue // schemaLocation and friends live in the instance namespace
		}
		ad, declared := d.attrs[k]
		if !declared {
			probs = append(probs, fmt.Sprintf("%s: attribute %q is not declared for <%s>", where, k, d.name))
			continue
		}
		if ad.use == "prohibited" {
			probs = append(probs, fmt.Sprintf("%s: attribute %q is prohibited", where, k))
			continue
		}
		if err := checkAttrType(where, ad, v); err != nil {
			probs = append(probs, err.Error())
		}
	}
	for _, ad := range d.attrs {
		if ad.use == "required" {
			if _, ok := n.attrs[ad.name]; !ok {
				probs = append(probs, fmt.Sprintf("%s: required attribute %q is missing from <%s>", where, ad.name, d.name))
			}
		}
	}

	if d.text != nil {
		if err := checkAttrType(where+" (text)", *d.text, strings.TrimSpace(n.text)); err != nil {
			probs = append(probs, err.Error())
		}
	}

	// A nil-ed element carries no children.
	if _, isNil := n.attrs["xsi:nil"]; isNil {
		return probs
	}

	names := make([]string, len(n.children))
	for i, c := range n.children {
		names[i] = c.name
	}
	if !modelAccepts(d.items, names) {
		probs = append(probs, fmt.Sprintf("%s: children %v do not satisfy the content model of <%s> (%v)",
			where, names, d.name, itemNames(d.items)))
	}
	for _, c := range n.children {
		cd := declForChild(d.items, c.name)
		if cd == nil {
			// An undeclared child is already reported by the model check.
			continue
		}
		probs = append(probs, s.check(c, cd, where)...)
	}
	return probs
}

func itemNames(items []item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.sub != nil {
			out = append(out, fmt.Sprintf("seq(%v)[%d,%s]", itemNames(it.sub), it.min, maxStr(it.max)))
			continue
		}
		nm := "?"
		if it.d != nil {
			nm = it.d.name
		}
		out = append(out, fmt.Sprintf("%s[%d,%s]", nm, it.min, maxStr(it.max)))
	}
	return out
}

func maxStr(m int) string {
	if m == -1 {
		return "unbounded"
	}
	return strconv.Itoa(m)
}

// endsFrom returns every position reachable by consuming a prefix of
// names[start:] with items, exactly as an xs:sequence permits (nested sequences
// repeat as units). The document is valid when len(names) is reachable.
func endsFrom(items []item, names []string, start int) []int {
	n := len(names)
	reach := make([]bool, n+1)
	reach[start] = true
	for _, it := range items {
		next := make([]bool, n+1)
		for j := start; j <= n; j++ {
			if !reach[j] {
				continue
			}
			if it.sub != nil {
				if it.min == 0 {
					next[j] = true
				}
				cur := []int{j}
				for reps := 1; ; reps++ {
					if it.max != -1 && reps > it.max {
						break
					}
					var follow []int
					for _, p := range cur {
						for _, q := range endsFrom(it.sub, names, p) {
							if q > p { // require progress so unbounded groups terminate
								follow = append(follow, q)
								if reps >= it.min {
									next[q] = true
								}
							}
						}
					}
					if len(follow) == 0 {
						break
					}
					cur = follow
				}
				continue
			}
			if it.min == 0 {
				next[j] = true
			}
			count := 0
			for k := j; k < n; k++ {
				if it.d != nil && names[k] != it.d.name {
					break
				}
				count++
				if count >= it.min {
					next[k+1] = true
				}
				if it.max != -1 && count >= it.max {
					break
				}
			}
		}
		reach = next
	}
	var out []int
	for j, ok := range reach {
		if ok {
			out = append(out, j)
		}
	}
	return out
}

func modelAccepts(items []item, names []string) bool {
	for _, end := range endsFrom(items, names, 0) {
		if end == len(names) {
			return true
		}
	}
	return false
}

// declForChild picks the declaration to validate a child against. Two items in
// the same sequence may allow the same element name only if they declare it
// identically or reference the same global element, so the first name match is
// sufficient for attribute checking.
func declForChild(items []item, name string) *decl {
	for _, it := range items {
		if it.sub != nil {
			if d := declForChild(it.sub, name); d != nil {
				return d
			}
			continue
		}
		if it.d != nil && it.d.name == name {
			return it.d
		}
	}
	return nil
}

func checkAttrType(where string, ad attrDecl, v string) error {
	if len(ad.enums) > 0 {
		allowed := false
		for _, e := range ad.enums {
			if e == v {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("%s: attribute %s=%q is not one of the schema's permitted values %v", where, ad.name, v, ad.enums)
		}
	}
	if ad.fixed != "" && v != ad.fixed {
		return fmt.Errorf("%s: attribute %s=%q must equal the schema's fixed value %q", where, ad.name, v, ad.fixed)
	}
	if ad.length > 0 && len(v) != ad.length {
		return fmt.Errorf("%s: attribute %s=%q must be exactly %d characters", where, ad.name, v, ad.length)
	}
	if ad.hasRange {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%s: attribute %s=%q is not a number", where, ad.name, v)
		}
		if ad.minInc != "" {
			lo, err := strconv.ParseFloat(ad.minInc, 64)
			if err == nil && f < lo {
				return fmt.Errorf("%s: attribute %s=%q is below the minimum %s", where, ad.name, v, ad.minInc)
			}
		}
		if ad.maxInc != "" {
			hi, err := strconv.ParseFloat(ad.maxInc, 64)
			if err == nil && f > hi {
				return fmt.Errorf("%s: attribute %s=%q is above the maximum %s", where, ad.name, v, ad.maxInc)
			}
		}
	}
	switch unref(ad.typ) {
	case "positiveInteger":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:positiveInteger", where, ad.name, v)
		}
	case "nonNegativeInteger":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:nonNegativeInteger", where, ad.name, v)
		}
	case "int", "integer", "long", "nonPositiveInteger", "unsignedInt":
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:%s", where, ad.name, v, unref(ad.typ))
		}
	case "float", "double":
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:%s", where, ad.name, v, unref(ad.typ))
		}
	case "boolean":
		if v != "true" && v != "false" && v != "1" && v != "0" {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:boolean", where, ad.name, v)
		}
	case "duration":
		if !durationRe.MatchString(v) {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:duration", where, ad.name, v)
		}
	case "dateTime":
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:dateTime", where, ad.name, v)
		}
	case "anyURI":
		if _, err := url.Parse(v); err != nil {
			return fmt.Errorf("%s: attribute %s=%q is not an xs:anyURI: %v", where, ad.name, v, err)
		}
	case "strictBase64Type":
		clean := strings.TrimSpace(v)
		if _, err := base64.StdEncoding.DecodeString(clean); err != nil {
			return fmt.Errorf("%s: element text is not valid base64: %v", where, err)
		}
	}
	return nil
}

// ---------- tests ----------

// TestSchemaDeclaresScanNotSpectrum locks in the conclusion the writer depends
// on: mzXML 3.2 names its spectrum element <scan>, and the indexed envelope uses
// <index>/<indexOffset>/<sha1> inside the root element.
func TestSchemaDeclaresScanNotSpectrum(t *testing.T) {
	s := loadSchema(t)
	scan, ok := s.elements["scan"]
	if !ok {
		t.Fatal("<scan> must be a global element of mzXML 3.2")
	}
	for _, want := range []string{"num", "msLevel", "peaksCount"} {
		if _, ok := scan.attrs[want]; !ok {
			t.Fatalf("scan/@%s is not declared", want)
		}
	}
	if _, ok := s.elements["indexedListmzXML"]; ok {
		t.Fatal("mzXML 3.2 must not declare <indexedListmzXML>")
	}
	if _, ok := s.elements["indexListOffset"]; ok {
		t.Fatal("mzXML 3.2 must not declare <indexListOffset>")
	}
	root, ok := s.elements["mzXML"]
	if !ok {
		t.Fatal("<mzXML> root missing")
	}
	var want []string
	for _, it := range root.items {
		if it.d != nil {
			want = append(want, it.d.name)
		}
	}
	for _, nm := range []string{"msRun", "index", "indexOffset", "sha1"} {
		found := false
		for _, w := range want {
			if w == nm {
				found = true
			}
		}
		if !found {
			t.Fatalf("root content model %v lacks %s", want, nm)
		}
	}
	// indexOffset is required, which is why a document with no index must write
	// xsi:nil rather than omit it.
	for _, it := range root.items {
		if it.d != nil && it.d.name == "indexOffset" && it.min != 1 {
			t.Fatalf("indexOffset should be required, min=%d", it.min)
		}
	}
}

func assertSchemaBasics(t *testing.T, s *schema) {
	t.Helper()
	if _, ok := s.elements["mzXML"]; !ok {
		t.Fatal("the schema set does not declare an <mzXML> root")
	}
	if _, ok := s.elements["scan"]; !ok {
		t.Fatal("the schema set does not declare a <scan> element")
	}
	if _, ok := s.elements["spectrum"]; ok {
		t.Fatal("unexpected: mzXML 3.2 must not declare <spectrum>")
	}
}

func mustCompile(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("bad pattern %q: %v", pattern, err)
	}
	return re
}
