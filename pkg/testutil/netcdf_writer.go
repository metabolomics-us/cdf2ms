// Package testutil builds synthetic NetCDF classic files and ANDI/MS runs in
// pure Go so the test-suite has real, byte-level fixtures without shipping
// large binary blobs.
//
// The writer implements the NetCDF-3 classic layout (CDF-1, CDF-2 and CDF-5)
// including record (unlimited-dimension) interleaving, which is what ANDI/MS
// files use for mass_values/intensity_values/time_values. Files produced here
// are verified against the reference netCDF C library via
// scripts/verify_with_netcdf4.py during development.
package testutil

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
)

// NCType mirrors the NetCDF external data type tags.
type NCType uint32

const (
	NCByte   NCType = 1
	NCChar   NCType = 2
	NCShort  NCType = 3
	NCInt    NCType = 4
	NCFloat  NCType = 5
	NCDouble NCType = 6
	NCUByte  NCType = 7
	NCUShort NCType = 8
	NCUInt   NCType = 9
	NCInt64  NCType = 10
	NCUInt64 NCType = 11
)

func (t NCType) size() int {
	switch t {
	case NCByte, NCChar, NCUByte:
		return 1
	case NCShort, NCUShort:
		return 2
	case NCInt, NCUInt, NCFloat:
		return 4
	case NCDouble, NCInt64, NCUInt64:
		return 8
	}
	return 0
}

// Attr is a NetCDF attribute. Exactly one of Str/Floats/Ints is used.
type Attr struct {
	Type   NCType
	Str    string
	Floats []float64
	Ints   []int64
}

// StringAttr is a convenience constructor.
func StringAttr(s string) Attr { return Attr{Type: NCChar, Str: s} }

// FloatAttr builds a double-valued attribute.
func FloatAttr(v ...float64) Attr { return Attr{Type: NCDouble, Floats: v} }

// Float32Attr builds a float-valued attribute (netCDF "float").
func Float32Attr(v ...float64) Attr { return Attr{Type: NCFloat, Floats: v} }

// IntAttr builds an int-valued attribute.
func IntAttr(v ...int64) Attr { return Attr{Type: NCInt, Ints: v} }

// Dim declares a dimension. Unlimited dimensions must sort before use in the
// record data block, but declaration order is free.
type Dim struct {
	Name      string
	Size      int64
	Unlimited bool
}

// Var declares a variable plus its (raw, already-packed) data.
//
// Data is supplied as a typed slice: []float32, []float64, []int16, []int32,
// []int8, []uint8 (for char) or []int64. The writer emits big-endian values and
// applies no scaling: packed datasets can be simulated exactly.
type Var struct {
	Name string
	Type NCType
	Dims []string
	Data any
	Attr map[string]Attr
}

// FileSpec is a whole synthetic file.
type FileSpec struct {
	Version int // 1, 2 or 5
	Global  map[string]Attr
	Dims    []Dim
	Vars    []Var
	// NumRecs overrides the derived record count.
	NumRecs int64
}

// WriteNetCDF writes a classic NetCDF file to disk.
func WriteNetCDF(path string, spec FileSpec) error {
	b, err := EncodeNetCDF(spec)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// EncodeNetCDF serializes a FileSpec into classic NetCDF bytes.
func EncodeNetCDF(spec FileSpec) ([]byte, error) {
	if spec.Version == 0 {
		spec.Version = 1
	}
	if spec.Version != 1 && spec.Version != 2 && spec.Version != 5 {
		return nil, fmt.Errorf("testutil: unsupported CDF version %d", spec.Version)
	}
	cdf5 := spec.Version == 5
	wideOffsets := spec.Version == 2 || spec.Version == 5

	dimIndex := map[string]int{}
	for i, d := range spec.Dims {
		dimIndex[d.Name] = i
	}
	numRecs := spec.NumRecs
	if numRecs == 0 {
		for _, v := range spec.Vars {
			for _, d := range v.Dims {
				if dimIndex[d] >= 0 && spec.Dims[dimIndex[d]].Unlimited {
					// derive from data length
					n, err := dataLen(v)
					if err != nil {
						return nil, err
					}
					inner := int64(1)
					for _, d2 := range v.Dims[1:] {
						inner *= spec.Dims[dimIndex[d2]].Size
					}
					if inner > 0 {
						numRecs = n / inner
					}
				}
			}
		}
	}

	// ---- data layout ----
	placements := make([]placement, len(spec.Vars))
	var recVars []int
	recVarSize := int64(0)
	for i, v := range spec.Vars {
		if len(v.Dims) > 0 && spec.Dims[dimIndex[v.Dims[0]]].Unlimited {
			recVars = append(recVars, i)
			inner := int64(1)
			for _, d := range v.Dims[1:] {
				inner *= spec.Dims[dimIndex[d]].Size
			}
			vs := pad4(inner * int64(v.Type.size()))
			placements[i].vsize = vs
			// rec_size is the sum of every record variable's per-record size.
			recVarSize += vs
			continue
		}
		n, err := dataLen(v)
		if err != nil {
			return nil, err
		}
		placements[i].vsize = pad4(n * int64(v.Type.size()))
	}

	// header size: build once with placeholder offsets
	hdr, err := buildHeader(spec, cdf5, wideOffsets, numRecs, pad4(recVarSize), placements, true)
	if err != nil {
		return nil, err
	}
	offset := int64(len(hdr))
	for i, v := range spec.Vars {
		isRec := false
		if len(v.Dims) > 0 {
			isRec = spec.Dims[dimIndex[v.Dims[0]]].Unlimited
		}
		if isRec {
			continue
		}
		placements[i].begin = offset
		offset += placements[i].vsize
	}
	recSize := pad4(recVarSize)
	recStart := offset
	{
		// Each record variable's begin is the record block start plus its offset
		// within an interleaved record (verified against real ANDI/MS exports and
		// the netCDF reference writer).
		intra := int64(0)
		for _, i := range recVars {
			placements[i].begin = recStart + intra
			intra += placements[i].vsize
			if intra > recSize {
				return nil, fmt.Errorf("testutil: record layout overflow: %d > %d", intra, recSize)
			}
		}
	}

	// ---- final header ----
	hdr, err = buildHeader(spec, cdf5, wideOffsets, numRecs, recSize, placements, false)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, len(hdr)+int(offset-recStart)+int(numRecs*recSize))
	out = append(out, hdr...)

	// non-record data, in declaration order
	for i, v := range spec.Vars {
		isRec := false
		if len(v.Dims) > 0 {
			isRec = spec.Dims[dimIndex[v.Dims[0]]].Unlimited
		}
		if isRec {
			continue
		}
		_ = placements[i]
		raw, err := encodeValues(v.Type, v.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, raw...)
		out = appendPadding(out, pad4(int64(len(raw)))-int64(len(raw)))
	}

	// record data block: interleaved per record, in declaration order
	if len(recVars) > 0 && numRecs > 0 {
		chunks := make([][]byte, len(recVars))
		recBytes := make([]int64, len(recVars))
		for k, idx := range recVars {
			v := spec.Vars[idx]
			raw, err := encodeValues(v.Type, v.Data)
			if err != nil {
				return nil, err
			}
			inner := int64(1)
			for _, d := range v.Dims[1:] {
				inner *= spec.Dims[dimIndex[d]].Size
			}
			rb := inner * int64(v.Type.size())
			if int64(len(raw)) < rb*numRecs {
				return nil, fmt.Errorf("testutil: variable %q has %d bytes, need %d",
					v.Name, len(raw), rb*numRecs)
			}
			chunks[k] = raw
			recBytes[k] = rb
		}
		for r := int64(0); r < numRecs; r++ {
			for k := range recVars {
				rb := recBytes[k]
				out = append(out, chunks[k][r*rb:(r+1)*rb]...)
				// Each record variable is individually padded to a 4-byte
				// boundary; rec_size is the sum of those padded sizes.
				out = appendPadding(out, pad4(rb)-rb)
			}
		}
	}
	return out, nil
}

func appendPadding(b []byte, n int64) []byte {
	for i := int64(0); i < n; i++ {
		b = append(b, 0)
	}
	return b
}

func dataLen(v Var) (int64, error) {
	switch d := v.Data.(type) {
	case []float32:
		return int64(len(d)), nil
	case []float64:
		return int64(len(d)), nil
	case []int16:
		return int64(len(d)), nil
	case []int32:
		return int64(len(d)), nil
	case []int64:
		return int64(len(d)), nil
	case []uint16:
		return int64(len(d)), nil
	case []uint32:
		return int64(len(d)), nil
	case []uint64:
		return int64(len(d)), nil
	case []int8:
		return int64(len(d)), nil
	case []uint8:
		return int64(len(d)), nil
	case nil:
		return 0, nil
	}
	return 0, fmt.Errorf("testutil: unsupported data type %T for %q", v.Data, v.Name)
}

func encodeValues(t NCType, data any) ([]byte, error) {
	n, err := dataLen(Var{Type: t, Data: data})
	if err != nil {
		return nil, err
	}
	out := make([]byte, int(n)*t.size())
	put := func(i int, bits uint64, size int) {
		switch size {
		case 1:
			out[i] = byte(bits)
		case 2:
			binary.BigEndian.PutUint16(out[i*2:], uint16(bits))
		case 4:
			binary.BigEndian.PutUint32(out[i*4:], uint32(bits))
		case 8:
			binary.BigEndian.PutUint64(out[i*8:], bits)
		}
	}
	switch t {
	case NCChar:
		switch d := data.(type) {
		case []uint8:
			copy(out, d)
		case []int8:
			for i, c := range d {
				out[i] = byte(c)
			}
		case string:
			copy(out, []byte(d))
			n = int64(len(d))
		default:
			return nil, fmt.Errorf("testutil: char variable needs []byte or string, got %T", data)
		}
		return out[:n], nil
	case NCByte:
		d, ok := data.([]int8)
		if !ok {
			return nil, fmt.Errorf("testutil: byte variable needs []int8, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(uint8(c)), 1)
		}
		return out, nil
	case NCUByte:
		d, ok := data.([]uint8)
		if !ok {
			return nil, fmt.Errorf("testutil: ubyte variable needs []uint8, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(c), 1)
		}
		return out, nil
	case NCShort:
		d, ok := data.([]int16)
		if !ok {
			return nil, fmt.Errorf("testutil: short variable needs []int16, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(uint16(c)), 2)
		}
		return out, nil
	case NCUShort:
		d, ok := data.([]uint16)
		if !ok {
			return nil, fmt.Errorf("testutil: ushort variable needs []uint16, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(c), 2)
		}
		return out, nil
	case NCInt:
		d, ok := data.([]int32)
		if !ok {
			return nil, fmt.Errorf("testutil: int variable needs []int32, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(uint32(c)), 4)
		}
		return out, nil
	case NCUInt:
		d, ok := data.([]uint32)
		if !ok {
			return nil, fmt.Errorf("testutil: uint variable needs []uint32, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(c), 4)
		}
		return out, nil
	case NCInt64:
		d, ok := data.([]int64)
		if !ok {
			return nil, fmt.Errorf("testutil: int64 variable needs []int64, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(c), 8)
		}
		return out, nil
	case NCUInt64:
		d, ok := data.([]uint64)
		if !ok {
			return nil, fmt.Errorf("testutil: uint64 variable needs []uint64, got %T", data)
		}
		for i, c := range d {
			put(i, c, 8)
		}
		return out, nil
	case NCFloat:
		d, ok := data.([]float32)
		if !ok {
			return nil, fmt.Errorf("testutil: float variable needs []float32, got %T", data)
		}
		for i, c := range d {
			put(i, uint64(math.Float32bits(c)), 4)
		}
		return out, nil
	case NCDouble:
		d, ok := data.([]float64)
		if !ok {
			return nil, fmt.Errorf("testutil: double variable needs []float64, got %T", data)
		}
		for i, c := range d {
			put(i, math.Float64bits(c), 8)
		}
		return out, nil
	}
	return nil, fmt.Errorf("testutil: unknown type %d", t)
}

func buildHeader(spec FileSpec, cdf5, wideOffsets bool, numRecs, recSize int64, placements []placement, placeholder bool) ([]byte, error) {
	b := &bucket{wide: cdf5}
	b.bytes([]byte("CDF"))
	b.byte(byte(spec.Version))
	b.nelems(numRecs)

	// dim_list
	if len(spec.Dims) == 0 {
		b.u32(0)
		b.nelems(0)
	} else {
		b.u32(0x0A)
		b.nelems(int64(len(spec.Dims)))
		for _, d := range spec.Dims {
			b.name(d.Name)
			sz := int64(0)
			if !d.Unlimited {
				sz = d.Size
			}
			b.nelems(sz)
		}
	}

	// gatt_list
	keys := sortedKeys(spec.Global)
	if len(keys) == 0 {
		b.u32(0)
		b.nelems(0)
	} else {
		b.u32(0x0C)
		b.nelems(int64(len(keys)))
		for _, k := range keys {
			if err := b.attr(k, spec.Global[k]); err != nil {
				return nil, err
			}
		}
	}

	// var_list
	if len(spec.Vars) == 0 {
		b.u32(0)
		b.nelems(0)
	} else {
		b.u32(0x0B)
		b.nelems(int64(len(spec.Vars)))
		dimIndex := map[string]int{}
		for i, d := range spec.Dims {
			dimIndex[d.Name] = i
		}
		for i, v := range spec.Vars {
			b.name(v.Name)
			b.nelems(int64(len(v.Dims)))
			for _, d := range v.Dims {
				id, ok := dimIndex[d]
				if !ok {
					return nil, fmt.Errorf("testutil: variable %q uses undeclared dimension %q", v.Name, d)
				}
				b.nelems(int64(id))
			}
			var vsize int64
			if len(v.Dims) > 0 && spec.Dims[dimIndex[v.Dims[0]]].Unlimited {
				inner := int64(1)
				for _, d := range v.Dims[1:] {
					inner *= spec.Dims[dimIndex[d]].Size
				}
				vsize = pad4(inner * int64(v.Type.size()))
			} else {
				vsize = placements[i].vsize
			}
			// vatt_list precedes nc_type/vsize/begin in the official grammar.
			attrs := sortedKeys(v.Attr)
			if len(attrs) == 0 {
				b.u32(0)
				b.nelems(0)
			} else {
				b.u32(0x0C)
				b.nelems(int64(len(attrs)))
				for _, k := range attrs {
					if err := b.attr(k, v.Attr[k]); err != nil {
						return nil, err
					}
				}
			}
			b.u32(uint32(v.Type))
			b.nelems(vsize)
			begin := int64(0)
			if !placeholder {
				begin = placements[i].begin
			}
			if wideOffsets {
				b.i64(begin)
			} else {
				b.u32(uint32(begin))
			}
		}
	}
	return b.out, nil
}

type placement struct {
	begin int64
	vsize int64
}

type bucket struct {
	out  []byte
	wide bool // CDF-5: nelems-like entities are 64-bit
}

// nelems writes a count/length field with the width the container expects.
func (b *bucket) nelems(v int64) {
	if b.wide {
		b.i64(v)
		return
	}
	b.u32(uint32(v))
}

func (b *bucket) bytes(p []byte) { b.out = append(b.out, p...) }
func (b *bucket) byte(c byte)    { b.out = append(b.out, c) }
func (b *bucket) u32(v uint32) {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	b.out = append(b.out, t[:]...)
}
func (b *bucket) i64(v int64) {
	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(v))
	b.out = append(b.out, t[:]...)
}

func (b *bucket) name(s string) {
	b.nelems(int64(len(s)))
	b.out = append(b.out, s...)
	for i := (4 - len(s)%4) % 4; i > 0; i-- {
		b.out = append(b.out, 0)
	}
}

func (b *bucket) attr(key string, a Attr) error {
	b.name(key)
	b.u32(uint32(a.Type))
	switch a.Type {
	case NCChar:
		n := len(a.Str)
		b.nelems(int64(n))
		b.out = append(b.out, a.Str...)
		for i := (4 - n%4) % 4; i > 0; i-- {
			b.out = append(b.out, 0)
		}
		return nil
	case NCFloat:
		b.nelems(int64(len(a.Floats)))
		for _, v := range a.Floats {
			b.u32(uint32(math.Float32bits(float32(v))))
		}
		return nil
	case NCDouble:
		b.nelems(int64(len(a.Floats)))
		for _, v := range a.Floats {
			b.i64(int64(math.Float64bits(v)))
		}
		return nil
	case NCInt, NCShort, NCByte, NCUByte, NCUShort, NCUInt, NCInt64, NCUInt64:
		b.nelems(int64(len(a.Ints)))
		sz := a.Type.size()
		for _, v := range a.Ints {
			switch sz {
			case 1:
				b.out = append(b.out, byte(v))
			case 2:
				var t [2]byte
				binary.BigEndian.PutUint16(t[:], uint16(v))
				b.out = append(b.out, t[:]...)
			case 4:
				b.u32(uint32(v))
			case 8:
				b.i64(v)
			}
		}
		pad := (4 - (len(a.Ints)*sz)%4) % 4
		for i := 0; i < pad; i++ {
			b.out = append(b.out, 0)
		}
		return nil
	}
	return fmt.Errorf("testutil: unknown attribute type %d", a.Type)
}

func sortedKeys(m map[string]Attr) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pad4(n int64) int64 { return (n + 3) & ^int64(3) }
