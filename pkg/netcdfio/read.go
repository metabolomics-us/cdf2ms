package netcdfio

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// ErrOutOfBounds is returned when a requested range exceeds a variable extent.
var ErrOutOfBounds = errors.New("netcdfio: range out of bounds")

// ErrTypeMismatch is returned when a caller asks for the wrong element class.
var ErrTypeMismatch = errors.New("netcdfio: variable type not compatible with request")

// trimFloat renders a float attribute value without gratuitous digits.
func trimFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Extent returns the number of addressable elements of a variable, treating the
// unlimited dimension as its current length.
func (v *Var) Extent() int64 {
	if v.IsRecord {
		return v.InnerSize * recordCountOf(v)
	}
	return v.Len
}

// recordCountOf is resolved by the owning File; the field is cached on the Var
// during header parsing via shape[0].
func recordCountOf(v *Var) int64 {
	if len(v.Shape) == 0 {
		return 1
	}
	return v.Shape[0]
}

// ensureRecordCounts resolves unlimited dimensions to numrecs in shapes.
func (f *File) ensureRecordCounts() {
	for _, v := range f.Vars {
		if v.IsRecord && len(v.Shape) > 0 {
			v.Shape[0] = f.NumRecs
			total := int64(1)
			for _, s := range v.Shape {
				total *= s
			}
			v.Len = total
		}
	}
}

// readAt copies len(p) bytes starting at off using a sequential read-ahead
// cache. Scanning a run is dominated by cache hits because ANDI variables are
// consumed in increasing offset order.
func (f *File) readAt(off int64, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if off < 0 || off+int64(len(p)) > f.fileSize {
		return fmt.Errorf("%w: reading %d bytes at %d in %d byte file", ErrShortFile, len(p), off, f.fileSize)
	}
	if f.cacheBuf == nil {
		f.cacheBuf = make([]byte, f.cacheSize)
		f.cacheOff = -1
	}
	// Cache hit?
	if f.cacheValid > 0 && off >= f.cacheOff && off+int64(len(p)) <= f.cacheOff+int64(f.cacheValid) {
		start := off - f.cacheOff
		copy(p, f.cacheBuf[start:start+int64(len(p))])
		f.stats.CacheHits++
		return nil
	}
	f.stats.Seeks++
	n := len(p)
	if n < len(f.cacheBuf) {
		// Fill the cache from off, bounded by EOF.
		want := len(f.cacheBuf)
		if off+int64(want) > f.fileSize {
			want = int(f.fileSize - off)
		}
		read, err := f.f.ReadAt(f.cacheBuf[:want], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		f.cacheOff = off
		f.cacheValid = read
		f.stats.Reads++
		f.stats.BytesRead += int64(read)
		if read < len(p) {
			return fmt.Errorf("%w: short read at %d", ErrShortFile, off)
		}
		copy(p, f.cacheBuf[:len(p)])
		return nil
	}
	// Large read: bypass the cache.
	_, err := f.f.ReadAt(p, off)
	f.stats.Reads++
	f.stats.BytesRead += int64(len(p))
	return err
}

// maxChunk bounds a single low-level read so memory stays bounded even when a
// caller asks for a very large slice.
const maxChunk = 1 << 20 // 1 MiB

// ReadFloats reads count elements of v beginning at logical index start,
// converting to float64, and appends them to dst. Record-aware.
func (f *File) ReadFloats(v *Var, start, count int64, dst []float64) ([]float64, error) {
	if !v.Type.Numeric() && v.Type != TypeChar {
		return dst, WrapErrorLocal(ErrTypeMismatch, v.Name, v.Type)
	}
	if start < 0 || count < 0 {
		return dst, fmt.Errorf("%w: start=%d count=%d for %q", ErrOutOfBounds, start, count, v.Name)
	}
	if count == 0 {
		return dst, nil
	}
	if count > f.limits.MaxScalarElems {
		return dst, fmt.Errorf("%w: request of %d elements of %q exceeds limit %d", ErrOutOfBounds, count, v.Name, f.limits.MaxScalarElems)
	}
	ext := v.Extent()
	if start+count > ext || start+count < start {
		return dst, fmt.Errorf("%w: %q slice [%d,%d) beyond extent %d", ErrOutOfBounds, v.Name, start, start+count, ext)
	}
	esize := int64(v.Type.Size())
	if f.raw == nil {
		f.raw = make([]byte, 0, maxChunk)
	}
	readRun := func(off int64, n int64) error {
		want := n * esize
		if int64(cap(f.raw)) < want {
			f.raw = make([]byte, want)
		} else {
			f.raw = f.raw[:want]
		}
		if err := f.readAt(off, f.raw); err != nil {
			return err
		}
		var err error
		dst, err = appendConverted(dst, f.raw, v.Type)
		return err
	}

	if !v.IsRecord {
		off := v.Begin + start*esize
		remaining := count
		per := maxChunk / esize
		if per < 1 {
			per = 1
		}
		for remaining > 0 {
			n := minInt64(per, remaining)
			if err := readRun(off, n); err != nil {
				return dst, err
			}
			off += n * esize
			remaining -= n
		}
		return dst, nil
	}

	inner := v.InnerSize
	if inner <= 0 {
		inner = 1
	}
	row := start / inner
	col := start % inner
	remaining := count
	for remaining > 0 {
		take := minInt64(inner-col, remaining)
		off := v.Begin + row*f.RecSize + col*esize
		for take > 0 {
			n := minInt64(maxChunk/esize, take)
			if n < 1 {
				n = 1
			}
			if err := readRun(off, n); err != nil {
				return dst, err
			}
			off += n * esize
			take -= n
			remaining -= n
		}
		row++
		col = 0
	}
	return dst, nil
}

// ReadInts is ReadFloats for integer-typed variables; floats are truncated
// toward zero only when they are integral, otherwise an error is returned.
func (f *File) ReadInts(v *Var, start, count int64) ([]int64, error) {
	if v.Type == TypeFloat || v.Type == TypeDouble || v.Type == TypeUInt64 {
		fl, err := f.ReadFloats(v, start, count, nil)
		if err != nil {
			return nil, err
		}
		out := make([]int64, len(fl))
		for i, x := range fl {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return nil, fmt.Errorf("netcdfio: %q element %d is not finite", v.Name, start+int64(i))
			}
			if x != math.Trunc(x) {
				return nil, fmt.Errorf("netcdfio: %q element %d = %v is not integral", v.Name, start+int64(i), x)
			}
			out[i] = int64(x)
		}
		return out, nil
	}
	fl, err := f.ReadFloats(v, start, count, nil)
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(fl))
	for i, x := range fl {
		out[i] = int64(x)
	}
	return out, nil
}

// ReadString reads a whole char variable (or a byte array) as a string,
// trimming NUL padding and trailing blanks the way NetCDF text fields do.
func (f *File) ReadString(v *Var) (string, error) {
	fl, err := f.ReadFloats(v, 0, v.Extent(), nil)
	if err != nil {
		return "", err
	}
	b := make([]byte, len(fl))
	for i, x := range fl {
		b[i] = byte(int8(x))
	}
	return CleanNetCDFString(b), nil
}

// CleanNetCDFString trims NUL terminators and trailing spaces from a NetCDF
// text field without touching interior bytes.
func CleanNetCDFString(b []byte) string {
	// Drop NULs anywhere (classic writers NUL-terminate char arrays) and then
	// trailing whitespace, preserving interior content verbatim.
	if i := indexFirstNUL(b); i >= 0 {
		b = b[:i]
	}
	return strings.TrimRight(string(b), " \t\r\n\x00")
}

func indexFirstNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// appendConverted appends raw big-endian elements of type t to dst as float64.
func appendConverted(dst []float64, raw []byte, t Type) ([]float64, error) {
	esize := int(t.Size())
	if esize == 0 {
		return dst, fmt.Errorf("netcdfio: cannot convert type %s", t)
	}
	if len(raw)%esize != 0 {
		return dst, fmt.Errorf("%w: %d bytes not a multiple of element size %d", ErrCorruptHeader, len(raw), esize)
	}
	n := len(raw) / esize
	for i := 0; i < n; i++ {
		p := raw[i*esize:]
		switch t {
		case TypeByte:
			dst = append(dst, float64(int8(p[0])))
		case TypeUByte, TypeChar:
			dst = append(dst, float64(p[0]))
		case TypeShort:
			dst = append(dst, float64(int16(be16(p))))
		case TypeUShort:
			dst = append(dst, float64(be16(p)))
		case TypeInt:
			dst = append(dst, float64(int32(be32(p))))
		case TypeUInt:
			dst = append(dst, float64(be32(p)))
		case TypeInt64:
			dst = append(dst, float64(int64(be64(p))))
		case TypeUInt64:
			dst = append(dst, float64(be64(p)))
		case TypeFloat:
			dst = append(dst, float64(math.Float32frombits(be32(p))))
		case TypeDouble:
			dst = append(dst, math.Float64frombits(be64(p)))
		default:
			return dst, fmt.Errorf("netcdfio: unsupported type %d", t)
		}
	}
	return dst, nil
}

func be16(p []byte) uint16 { return uint16(p[0])<<8 | uint16(p[1]) }
func be32(p []byte) uint32 {
	return uint32(p[0])<<24 | uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
}
func be64(p []byte) uint64 {
	return uint64(be32(p))<<32 | uint64(be32(p[4:]))
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// WrapErrorLocal adapts a sentinel error with context.
func WrapErrorLocal(err error, args ...any) error {
	if len(args) == 0 {
		return err
	}
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	return fmt.Errorf("%w: %s", err, strings.Join(parts, " "))
}

// IsFillFloat reports whether v equals the NetCDF default fill value for t
// (as promoted to float64).
func IsFillFloat(t Type, v float64) bool {
	switch t {
	case TypeByte:
		return v == -127
	case TypeShort:
		return v == -32767
	case TypeInt:
		return v == -2147483647
	case TypeFloat:
		return v == float64(float32(9.969209968386869e36))
	case TypeDouble:
		return v == 9.969209968386869e36
	case TypeInt64:
		return v == -9223372036854775806
	}
	return false
}
