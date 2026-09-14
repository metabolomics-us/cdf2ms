// Package netcdfio is a dependency-free reader for the NetCDF classic family
// (CDF-1, CDF-2 64-bit-offset, CDF-5 64-bit-data) plus container detection for
// HDF5-backed NetCDF4.
//
// Design notes:
//
//   - Reads are positional (io.ReaderAt) so a single scan's peak array can be
//     fetched without materialising the whole variable. This is what keeps the
//     converter's memory footprint O(points-in-current-spectrum).
//   - Record (unlimited-dimension) variables are fully supported: ANDI/MS files
//     declare point_number as the unlimited dimension, so mass_values,
//     time_values and intensity_values are interleaved one record apart and must
//     be read with record-aware addressing.
//   - Every header quantity is validated against the physical file size. A
//     malformed or truncated file produces a typed error, never a panic.
package netcdfio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// Type is a NetCDF classic external data type.
type Type uint32

const (
	TypeByte   Type = 1
	TypeChar   Type = 2
	TypeShort  Type = 3
	TypeInt    Type = 4
	TypeFloat  Type = 5
	TypeDouble Type = 6
	// CDF-64-bit-data (CDF-5) additions:
	TypeUByte  Type = 7
	TypeUShort Type = 8
	TypeUInt   Type = 9
	TypeInt64  Type = 10
	TypeUInt64 Type = 11
)

// String implements fmt.Stringer using CDL names.
func (t Type) String() string {
	switch t {
	case TypeByte:
		return "byte"
	case TypeChar:
		return "char"
	case TypeShort:
		return "short"
	case TypeInt:
		return "int"
	case TypeFloat:
		return "float"
	case TypeDouble:
		return "double"
	case TypeUByte:
		return "ubyte"
	case TypeUShort:
		return "ushort"
	case TypeUInt:
		return "uint"
	case TypeInt64:
		return "int64"
	case TypeUInt64:
		return "uint64"
	}
	return fmt.Sprintf("unknown(%d)", uint32(t))
}

// Size returns the packed on-disk size in bytes.
func (t Type) Size() int {
	switch t {
	case TypeByte, TypeChar, TypeUByte:
		return 1
	case TypeShort, TypeUShort:
		return 2
	case TypeInt, TypeUInt, TypeFloat:
		return 4
	case TypeDouble, TypeInt64, TypeUInt64:
		return 8
	}
	return 0
}

// Numeric reports whether the type holds numbers (as opposed to char).
func (t Type) Numeric() bool { return t != TypeChar && t.Size() > 0 }

// Encoding identifies the classic container variant.
type Encoding int

const (
	EncodingUnknown Encoding = iota
	EncodingCDF1
	EncodingCDF2
	EncodingCDF5
	EncodingHDF5 // NetCDF4 / NetCDF4-classic: detected, not decoded
)

// String implements fmt.Stringer with human-readable labels used in reports.
func (e Encoding) String() string {
	switch e {
	case EncodingCDF1:
		return "CDF-1 (classic)"
	case EncodingCDF2:
		return "CDF-2 (64-bit offset)"
	case EncodingCDF5:
		return "CDF-5 (64-bit data)"
	case EncodingHDF5:
		return "HDF5 (NetCDF4)"
	}
	return "unknown"
}

// ErrUnsupportedEncoding is returned for containers this engine cannot decode.
var ErrUnsupportedEncoding = errors.New("netcdfio: unsupported container encoding")

// Detect sniffs the container encoding from the first bytes of a file.
func Detect(path string) (Encoding, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return EncodingUnknown, "", err
	}
	defer f.Close()
	var hdr [8]byte
	n, err := io.ReadFull(f, hdr[:4])
	if err != nil && !(errors.Is(err, io.ErrUnexpectedEOF)) {
		return EncodingUnknown, "", err
	}
	if n < 4 {
		return EncodingUnknown, "", fmt.Errorf("%w: file smaller than 4 bytes", ErrUnsupportedEncoding)
	}
	switch {
	case hdr[0] == 'C' && hdr[1] == 'D' && hdr[2] == 'F' && hdr[3] == 1:
		return EncodingCDF1, "CDF\x01", nil
	case hdr[0] == 'C' && hdr[1] == 'D' && hdr[2] == 'F' && hdr[3] == 2:
		return EncodingCDF2, "CDF\x02", nil
	case hdr[0] == 'C' && hdr[1] == 'D' && hdr[2] == 'F' && hdr[3] == 5:
		return EncodingCDF5, "CDF\x05", nil
	case hdr[0] == 0x89 && hdr[1] == 'H' && hdr[2] == 'D' && hdr[3] == 'F':
		return EncodingHDF5, "\x89HDF", nil
	}
	return EncodingUnknown, fmt.Sprintf("%02x %02x %02x %02x", hdr[0], hdr[1], hdr[2], hdr[3]),
		fmt.Errorf("%w: magic %02x %02x %02x %02x", ErrUnsupportedEncoding, hdr[0], hdr[1], hdr[2], hdr[3])
}

// Limits bound hostile headers. They are configurable because legitimate
// multi-million-scan files exist.
type Limits struct {
	MaxDimensions   int
	MaxVariables    int
	MaxAttributes   int
	MaxVarDims      int
	MaxNameLen      int
	MaxAttrElems    int64
	MaxScalarElems  int64 // elements of a single read request
	HeaderScanLimit int64 // max bytes of header to parse
}

// DefaultLimits returns conservative but corpus-friendly limits.
func DefaultLimits() Limits {
	return Limits{
		MaxDimensions:   16384,
		MaxVariables:    32768,
		MaxAttributes:   32768,
		MaxVarDims:      16,
		MaxNameLen:      8192,
		MaxAttrElems:    1 << 26, // 64 Mi elements
		MaxScalarElems:  1 << 26,
		HeaderScanLimit: 1 << 28, // 256 MiB of header
	}
}

// Attribute is a typed NetCDF attribute value.
type Attribute struct {
	Type   Type
	Objs   []any // []int64 | []uint64 | []float64 | []byte (char) | []int8
	Str    string
	Nums   []float64
	Ints   []int64
	IsChar bool
}

// Kind returns a short label for reports: "string", "float", "int", "byte".
func (a Attribute) Kind() string {
	switch {
	case a.IsChar:
		return "string"
	case len(a.Nums) > 0:
		return "float"
	case len(a.Ints) > 0:
		return "int"
	}
	return "other"
}

// Value renders the attribute for humans/reports.
func (a Attribute) Value() string {
	if a.IsChar {
		return a.Str
	}
	if len(a.Nums) == 1 {
		return trimFloat(a.Nums[0])
	}
	if len(a.Ints) == 1 {
		return fmt.Sprintf("%d", a.Ints[0])
	}
	return fmt.Sprintf("%d values", len(a.Objs))
}

// Float returns the first numeric value when present.
func (a Attribute) Float() (float64, bool) {
	if len(a.Nums) > 0 {
		return a.Nums[0], true
	}
	if len(a.Ints) > 0 {
		return float64(a.Ints[0]), true
	}
	return 0, false
}

// Var describes one NetCDF variable.
type Var struct {
	Name     string
	Type     Type
	DimIDs   []uint32
	Dims     []string
	Shape    []int64
	VSize    int64
	Begin    int64
	IsRecord bool

	// Len is the total number of elements across all dimensions.
	Len int64

	// InnerSize is the number of elements per record (record vars only).
	InnerSize int64

	Attrs map[string]Attribute
}

// DimSize returns the named dimension size.
func (f *File) DimSize(name string) (int64, bool) {
	for _, d := range f.Dimensions {
		if d.Name == name {
			return d.Size, true
		}
	}
	return 0, false
}

// Dimension is a declared dimension. Unlimited dimensions report their current
// length (numrecs), not zero.
type Dimension struct {
	Name      string
	Size      int64
	Unlimited bool
}

// File is an opened classic NetCDF file.
type File struct {
	path       string
	f          *os.File
	encoding   Encoding
	magic      string
	NumRecs    int64
	RecSize    int64
	Dimensions []Dimension
	Globals    map[string]Attribute
	Vars       map[string]*Var
	varOrder   []string
	fileSize   int64
	limits     Limits

	// Truncated records: header claims more data than the file contains.
	Truncated   bool
	TruncDetail string

	// varWarnings records benign header inconsistencies per variable.
	varWarnings map[string]string

	// dataCache is a small sequential read-ahead cache shared by all reads.
	cacheOff   int64
	cacheBuf   []byte
	cacheValid int
	cacheSize  int
	raw        []byte
	stats      Stats
}

// Stats counts low-level I/O performed by a File.
type Stats struct {
	Reads     int64
	BytesRead int64
	CacheHits int64
	Seeks     int64
}

// Open opens a classic NetCDF file. HDF5 containers are reported as
// unsupported rather than silently mis-parsed.
func Open(path string) (*File, error) { return OpenWithLimits(path, DefaultLimits()) }

// OpenWithLimits opens a file with explicit header limits.
func OpenWithLimits(path string, lim Limits) (*File, error) {
	enc, magic, err := Detect(path)
	if err != nil {
		return nil, err
	}
	if enc == EncodingHDF5 {
		return nil, fmt.Errorf("%w: %s is HDF5-backed NetCDF4; the pure-Go classic engine reads CDF-1/2/5 only", ErrUnsupportedEncoding, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("netcdfio: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("netcdfio: stat %s: %w", path, err)
	}
	nf := &File{
		path:      path,
		f:         f,
		encoding:  enc,
		magic:     magic,
		fileSize:  st.Size(),
		limits:    lim,
		Globals:   map[string]Attribute{},
		Vars:      map[string]*Var{},
		cacheSize: 1 << 18, // 256 KiB read-ahead
	}
	if err := nf.parseHeader(); err != nil {
		f.Close()
		return nil, err
	}
	return nf, nil
}

// Close releases the file handle.
func (f *File) Close() error {
	if f.f == nil {
		return nil
	}
	err := f.f.Close()
	f.f = nil
	return err
}

// Path returns the opened path.
func (f *File) Path() string { return f.path }

// Encoding returns the container variant.
func (f *File) Encoding() Encoding { return f.encoding }

// Magic returns the 4-byte magic rendered for reports.
func (f *File) Magic() string { return f.magic }

// FileSize returns the physical size in bytes.
func (f *File) FileSize() int64 { return f.fileSize }

// VarNames returns variable names in header order.
func (f *File) VarNames() []string { return append([]string(nil), f.varOrder...) }

// Var returns a variable by name.
func (f *File) Var(name string) (*Var, error) {
	v, ok := f.Vars[name]
	if !ok {
		return nil, fmt.Errorf("netcdfio: variable %q not found in %s", name, f.path)
	}
	return v, nil
}

// Stats returns a copy of the I/O counters.
func (f *File) Stats() Stats { return f.stats }

// ---------------------------------------------------------------------------
// header parsing
// ---------------------------------------------------------------------------

type headerReader struct {
	f        *File
	off      int64
	buf      []byte
	fileSize int64
	// wide is true for CDF-5, where every nelems-like entity (name lengths,
	// list counts, dimension lengths, dimension ids, attribute element counts,
	// vsizes and numrecs) is a 64-bit integer. Tags and nc_type stay 32-bit.
	wide bool
	// wideOffsets is true for CDF-2 and CDF-5 (64-bit data offsets).
	wideOffsets bool
}

// nelems reads a count/length field whose width follows the container version.
func (h *headerReader) nelems() (int64, error) {
	if h.wide {
		return h.i64()
	}
	u, err := h.u32()
	return int64(u), err
}

func (h *headerReader) need(n int64) error {
	if n < 0 || h.off+n > h.fileSize {
		return fmt.Errorf("%w: header needs %d bytes at offset %d but file is %d bytes",
			ErrShortFile, n, h.off, h.fileSize)
	}
	return nil
}

// ErrShortFile signals a file shorter than its header claims.
var ErrShortFile = errors.New("netcdfio: truncated file")

func (h *headerReader) u32() (uint32, error) {
	if err := h.need(4); err != nil {
		return 0, err
	}
	var b [4]byte
	if _, err := h.f.f.ReadAt(b[:], h.off); err != nil {
		return 0, err
	}
	h.off += 4
	return binary.BigEndian.Uint32(b[:]), nil
}

func (h *headerReader) i64() (int64, error) {
	if err := h.need(8); err != nil {
		return 0, err
	}
	var b [8]byte
	if _, err := h.f.f.ReadAt(b[:], h.off); err != nil {
		return 0, err
	}
	h.off += 8
	return int64(binary.BigEndian.Uint64(b[:])), nil
}

func (h *headerReader) offset() (int64, error) {
	if h.wideOffsets {
		return h.i64()
	}
	u, err := h.u32()
	if err != nil {
		return 0, err
	}
	return int64(u), nil
}

func (h *headerReader) name() (string, error) {
	n64, err := h.nelems()
	if err != nil {
		return "", err
	}
	n := int(n64)
	if n == 0 || int(n) > h.f.limits.MaxNameLen {
		return "", fmt.Errorf("%w: name length %d out of range", ErrCorruptHeader, n)
	}
	if err := h.need(int64(n)); err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := h.f.f.ReadAt(b, h.off); err != nil {
		return "", err
	}
	h.off += int64(n)
	pad := (4 - int64(n)%4) % 4
	h.off += pad
	return string(b), nil
}

// ErrCorruptHeader marks structurally invalid headers.
var ErrCorruptHeader = errors.New("netcdfio: corrupt header")

const (
	tagDimension  = 0x0A
	tagAttributes = 0x0C
	tagVariables  = 0x0B
)

func (f *File) parseHeader() error {
	cdf5 := f.encoding == EncodingCDF5
	h := &headerReader{f: f, off: 4, fileSize: f.fileSize,
		wide: cdf5, wideOffsets: cdf5 || f.encoding == EncodingCDF2}

	// numrecs
	nr, err := h.nelems()
	if err != nil {
		return err
	}
	f.NumRecs = nr
	if f.NumRecs < 0 || f.NumRecs > 1<<34 {
		return fmt.Errorf("%w: numrecs %d out of range", ErrCorruptHeader, f.NumRecs)
	}

	// dim_list
	tag, err := h.u32()
	if err != nil {
		return err
	}
	cnt, err := h.nelems()
	if err != nil {
		return err
	}
	if tag == 0 && cnt == 0 {
		// ABSENT
	} else if tag != tagDimension {
		return fmt.Errorf("%w: expected dimension list tag, got 0x%x", ErrCorruptHeader, tag)
	} else {
		if cnt > int64(f.limits.MaxDimensions) {
			return fmt.Errorf("%w: %d dimensions exceeds limit %d", ErrCorruptHeader, cnt, f.limits.MaxDimensions)
		}
		f.Dimensions = make([]Dimension, 0, cnt)
		for i := int64(0); i < cnt; i++ {
			name, err := h.name()
			if err != nil {
				return err
			}
			size, err := h.nelems()
			if err != nil {
				return err
			}
			if size < 0 {
				return fmt.Errorf("%w: dimension %q has negative size", ErrCorruptHeader, name)
			}
			d := Dimension{Name: name, Size: size, Unlimited: size == 0}
			if d.Unlimited {
				d.Size = f.NumRecs
			}
			f.Dimensions = append(f.Dimensions, d)
		}
	}

	// gatt_list
	if err := f.parseAttributes(h, f.Globals); err != nil {
		return err
	}

	// var_list
	tag, err = h.u32()
	if err != nil {
		return err
	}
	cnt, err = h.nelems()
	if err != nil {
		return err
	}
	if tag == 0 && cnt == 0 {
		return fmt.Errorf("%w: file declares no variables", ErrCorruptHeader)
	}
	if tag != tagVariables {
		return fmt.Errorf("%w: expected variable list tag, got 0x%x", ErrCorruptHeader, tag)
	}
	if cnt > int64(f.limits.MaxVariables) {
		return fmt.Errorf("%w: %d variables exceeds limit %d", ErrCorruptHeader, cnt, f.limits.MaxVariables)
	}
	// A variable header is at least name+nelems+vatt_list+type+vsize+begin.
	minVar := int64(4 + 4 + 4 + 4 + 4 + 4)
	if cnt*minVar > f.fileSize {
		return fmt.Errorf("%w: %d declared variables cannot fit in %d byte file", ErrCorruptHeader, cnt, f.fileSize)
	}
	recVarSize := int64(0)
	for i := int64(0); i < cnt; i++ {
		v := &Var{Attrs: map[string]Attribute{}}
		v.Name, err = h.name()
		if err != nil {
			return err
		}
		ndims, err := h.nelems()
		if err != nil {
			return err
		}
		if ndims < 0 || ndims > int64(f.limits.MaxVarDims) {
			return fmt.Errorf("%w: variable %q has %d dimensions (limit %d)", ErrCorruptHeader, v.Name, ndims, f.limits.MaxVarDims)
		}
		v.DimIDs = make([]uint32, ndims)
		v.Dims = make([]string, ndims)
		v.Shape = make([]int64, ndims)
		total := int64(1)
		for d := int64(0); d < ndims; d++ {
			id, err := h.nelems()
			if err != nil {
				return err
			}
			if id < 0 || id >= int64(len(f.Dimensions)) {
				return fmt.Errorf("%w: variable %q references dimension id %d out of range", ErrCorruptHeader, v.Name, id)
			}
			v.DimIDs[d] = uint32(id)
			v.Dims[d] = f.Dimensions[id].Name
			v.Shape[d] = f.Dimensions[id].Size
			if f.Dimensions[id].Unlimited {
				if d != 0 {
					return fmt.Errorf("%w: variable %q has unlimited dimension not in first position", ErrCorruptHeader, v.Name)
				}
				v.IsRecord = true
			} else if f.Dimensions[id].Size == 0 {
				return fmt.Errorf("%w: variable %q has zero-length dimension %q", ErrCorruptHeader, v.Name, v.Dims[d])
			}
			if total > 0 && f.Dimensions[id].Size > 0 && total > (1<<56)/maxInt64(f.Dimensions[id].Size, 1) {
				return fmt.Errorf("%w: variable %q element count overflows", ErrCorruptHeader, v.Name)
			}
			total *= f.Dimensions[id].Size
		}
		// Official grammar:
		//   var = name nelems [dimid ...] vatt_list nc_type vsize begin
		if err := f.parseAttributes(h, v.Attrs); err != nil {
			return err
		}
		tu, err := h.u32()
		if err != nil {
			return err
		}
		v.Type = Type(tu)
		if v.Type.Size() == 0 {
			return fmt.Errorf("%w: variable %q has unsupported type %d", ErrCorruptHeader, v.Name, tu)
		}
		if v.Type > TypeDouble && !cdf5 {
			return fmt.Errorf("%w: variable %q uses CDF-5 type %s in %s container", ErrCorruptHeader, v.Name, v.Type, f.encoding)
		}
		v.VSize, err = h.nelems()
		if err != nil {
			return err
		}
		v.Begin, err = h.offset()
		if err != nil {
			return err
		}
		if len(v.Shape) == 0 {
			v.Len = 1
		} else {
			v.Len = total
		}
		if v.IsRecord {
			// Classic format: rec_size is the sum of the (4-byte padded) vsize
			// of every record variable, because records interleave them all.
			recVarSize += pad4(v.VSize)
		} else if v.VSize > 0 && v.VSize != pad4(v.Len*int64(v.Type.Size())) {
			// Redundant field; tolerate vendors that round differently, but keep
			// the larger value so reads never run past an allocation.
			if v.VSize < pad4(v.Len*int64(v.Type.Size())) {
				f.noteWarning(v.Name, v.VSize)
			}
		}
		if v.Begin < 0 || v.Begin > f.fileSize {
			return fmt.Errorf("%w: variable %q begins at %d, past end of file (%d)",
				ErrCorruptHeader, v.Name, v.Begin, f.fileSize)
		}
		f.Vars[v.Name] = v
		f.varOrder = append(f.varOrder, v.Name)
	}
	if recVarSize > 0 {
		f.RecSize = pad4(recVarSize)
	}
	for _, v := range f.Vars {
		if !v.IsRecord {
			continue
		}
		inner := int64(1)
		for _, s := range v.Shape[1:] {
			inner *= s
		}
		v.InnerSize = inner
	}
	f.ensureRecordCounts()
	f.detectTruncation()
	return nil
}

// noteWarning records a benign header inconsistency for the report.
func (f *File) noteWarning(varName string, vsize int64) {
	if f.varWarnings == nil {
		f.varWarnings = map[string]string{}
	}
	f.varWarnings[varName] = fmt.Sprintf("declared vsize %d smaller than computed size", vsize)
}

// parseAttributes reads an attribute list (ABSENT | NC_ATTRIBUTE nelems [attr...]).
func (f *File) parseAttributes(h *headerReader, dst map[string]Attribute) error {
	tag, err := h.u32()
	if err != nil {
		return err
	}
	cnt, err := h.nelems()
	if err != nil {
		return err
	}
	if tag == 0 && cnt == 0 {
		return nil
	}
	if tag != tagAttributes {
		return fmt.Errorf("%w: expected attribute list tag, got 0x%x", ErrCorruptHeader, tag)
	}
	if cnt > int64(f.limits.MaxAttributes) {
		return fmt.Errorf("%w: %d attributes exceeds limit %d", ErrCorruptHeader, cnt, f.limits.MaxAttributes)
	}
	if cnt*12 > f.fileSize {
		return fmt.Errorf("%w: %d attributes cannot fit in %d byte file", ErrCorruptHeader, cnt, f.fileSize)
	}
	if dst == nil {
		return fmt.Errorf("%w: nil attribute destination", ErrCorruptHeader)
	}
	return f.readAttrValues(h, dst, cnt)
}

func (f *File) readAttrValues(h *headerReader, dst map[string]Attribute, count int64) error {
	for i := int64(0); i < count; i++ {
		name, err := h.name()
		if err != nil {
			return err
		}
		tu, err := h.u32()
		if err != nil {
			return err
		}
		t := Type(tu)
		if t.Size() == 0 {
			return fmt.Errorf("%w: attribute %q has unsupported type %d", ErrCorruptHeader, name, tu)
		}
		nelems, err := h.nelems()
		if err != nil {
			return err
		}
		if nelems < 0 || nelems > f.limits.MaxAttrElems {
			return fmt.Errorf("%w: attribute %q declares %d elements", ErrCorruptHeader, name, nelems)
		}
		byteLen := nelems * int64(t.Size())
		padded := pad4(byteLen)
		if padded > f.fileSize-h.off {
			return fmt.Errorf("%w: attribute %q needs %d bytes at offset %d, file is %d bytes",
				ErrShortFile, name, padded, h.off, f.fileSize)
		}
		raw := make([]byte, padded)
		if _, err := h.f.f.ReadAt(raw, h.off); err != nil {
			return fmt.Errorf("%w: reading attribute %q: %v", ErrShortFile, name, err)
		}
		h.off += padded

		a := Attribute{Type: t}
		switch t {
		case TypeChar:
			a.IsChar = true
			a.Str = CleanNetCDFString(raw[:byteLen])
		case TypeFloat, TypeDouble:
			n := int(nelems)
			a.Nums = make([]float64, 0, n)
			a.Objs = make([]any, 0, n)
			for j := int64(0); j < nelems; j++ {
				var val float64
				if t == TypeFloat {
					val = float64(math.Float32frombits(be32(raw[j*4:])))
				} else {
					val = math.Float64frombits(be64(raw[j*8:]))
				}
				a.Nums = append(a.Nums, val)
				a.Objs = append(a.Objs, val)
			}
		default:
			n := int(nelems)
			a.Ints = make([]int64, 0, n)
			a.Objs = make([]any, 0, n)
			for j := int64(0); j < nelems; j++ {
				off := j * int64(t.Size())
				var val int64
				switch t {
				case TypeByte:
					val = int64(int8(raw[off]))
				case TypeShort:
					val = int64(int16(be16(raw[off:])))
				case TypeInt:
					val = int64(int32(be32(raw[off:])))
				case TypeUByte:
					val = int64(raw[off])
				case TypeUShort:
					val = int64(be16(raw[off:]))
				case TypeUInt:
					val = int64(be32(raw[off:]))
				case TypeInt64:
					val = int64(be64(raw[off:]))
				case TypeUInt64:
					u := be64(raw[off:])
					if u > math.MaxInt64 {
						val = math.MaxInt64
					} else {
						val = int64(u)
					}
				}
				a.Ints = append(a.Ints, val)
				a.Objs = append(a.Objs, val)
			}
		}
		dst[name] = a
	}
	return nil
}

// detectTruncation compares the header's implied size against the real file
// size. ANDI/MS files copied over flaky FTP are frequently short by a few
// records; we surface that instead of silently returning zeros.
func (f *File) detectTruncation() {
	implied := int64(0)
	for _, name := range f.varOrder {
		v := f.Vars[name]
		if v.IsRecord {
			continue
		}
		end := v.Begin + v.VSize
		if v.VSize == 0 {
			end = v.Begin + pad4(v.Len*int64(v.Type.Size()))
		}
		if end > implied {
			implied = end
		}
	}
	if f.RecSize > 0 && f.NumRecs > 0 {
		recoffset := int64(-1)
		for _, name := range f.varOrder {
			v := f.Vars[name]
			if v.IsRecord && (recoffset < 0 || v.Begin < recoffset) {
				recoffset = v.Begin
			}
		}
		if recoffset >= 0 {
			end := recoffset + f.NumRecs*f.RecSize
			if end > implied {
				implied = end
			}
		}
	}
	if implied > f.fileSize {
		f.Truncated = true
		f.TruncDetail = fmt.Sprintf("header implies %d bytes, file has %d (%.2f%% missing)",
			implied, f.fileSize, 100*float64(implied-f.fileSize)/float64(implied))
	}
}

func pad4(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n + 3) & ^int64(3)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
