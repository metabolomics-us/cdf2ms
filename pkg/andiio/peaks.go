package andiio

import (
	"fmt"
	"math"

	"github.com/metabolomics-us/cdf2ms/pkg/netcdfio"
)

// peakReader fetches the m/z and intensity arrays of consecutive scans.
//
// ANDI/MS stores mass_values, intensity_values and time_values as record
// variables interleaved inside one record block. Reading them scan-by-scan
// through independent positional reads issues one syscall or cache lookup per
// peak. Instead the reader slurps a window of records once and de-interleaves it
// in memory: sequential I/O, one allocation, and peak memory bounded by the
// window size regardless of file size.
type peakReader struct {
	f    *netcdfio.File
	mass *VarRef
	inte *VarRef
	tv   *VarRef

	interleaved bool
	recSize     int64
	recoffset   int64
	massIntra   int64
	inteIntra   int64
	tvIntra     int64

	// window state
	win      []byte
	winFirst int64 // first point index covered
	winN     int64 // number of records/points covered
	maxWin   int64 // max records per window
}

// maxWindowBytes bounds the de-interleaving window.
const maxWindowBytes = 8 << 20 // 8 MiB

func newPeakReader(f *netcdfio.File, l *Layout) *peakReader {
	p := &peakReader{f: f, mass: l.Mass, inte: l.Intensity, tv: l.TimeValues, maxWin: 65536}
	m, i := l.Mass.Var, l.Intensity.Var
	if m.IsRecord && i.IsRecord && f.RecSize > 0 {
		recOffset := f.RecordOffset()
		innerOK := m.InnerSize <= 1 && i.InnerSize <= 1 && (l.TimeValues == nil || l.TimeValues.Var.InnerSize <= 1)
		sameType := m.Type == i.Type
		if innerOK && sameType && m.Begin >= recOffset && i.Begin >= recOffset {
			p.interleaved = true
			p.recSize = f.RecSize
			p.recoffset = recOffset
			p.massIntra = m.Begin - recOffset
			p.inteIntra = i.Begin - recOffset
			if l.TimeValues != nil {
				p.tvIntra = l.TimeValues.Var.Begin - recOffset
			}
			p.maxWin = int64(maxWindowBytes) / p.recSize
			if p.maxWin < 1024 {
				p.maxWin = 1024
			}
		}
	}
	return p
}

// ensure loads a window covering point indices [start, start+n).
func (p *peakReader) ensure(start, n int64) error {
	if n <= 0 {
		return nil
	}
	if p.winN > 0 && start >= p.winFirst && start+n <= p.winFirst+p.winN {
		return nil
	}
	count := n
	if count < p.maxWin {
		count = p.maxWin
	}
	if count > p.maxWin {
		count = p.maxWin
	}
	ext := p.mass.Var.Extent()
	if start+count > ext {
		count = ext - start
	}
	if count <= 0 {
		return fmt.Errorf("andiio: window start %d past extent %d", start, ext)
	}
	need := count * p.recSize
	if int64(cap(p.win)) < need {
		p.win = make([]byte, need)
	}
	win := p.win[:need]
	if err := p.f.ReadRaw(p.recoffset+start*p.recSize, win); err != nil {
		return fmt.Errorf("andiio: reading record window at point %d: %w", start, err)
	}
	p.winFirst = start
	p.winN = count
	return nil
}

// Read returns the m/z and intensity arrays for points [start, start+count).
// dstMZ/dstInt are reused buffers owned by the caller; they are truncated to the
// requested length.
func (p *peakReader) Read(start, count int64, dstMZ, dstInt []float64) ([]float64, []float64, error) {
	dstMZ = dstMZ[:0]
	dstInt = dstInt[:0]
	if count == 0 {
		return dstMZ, dstInt, nil
	}
	if count > int64(cap(dstMZ)) {
		dstMZ = make([]float64, count)
		dstInt = make([]float64, count)
	}
	dstMZ = dstMZ[:count]
	dstInt = dstInt[:count]

	if !p.interleaved {
		var err error
		dstMZ, err = p.f.ReadFloats(p.mass.Var, start, count, dstMZ[:0])
		if err != nil {
			return nil, nil, err
		}
		dstInt, err = p.f.ReadFloats(p.inte.Var, start, count, dstInt[:0])
		if err != nil {
			return nil, nil, err
		}
		if int64(len(dstMZ)) != count || int64(len(dstInt)) != count {
			return nil, nil, fmt.Errorf("andiio: peak array length mismatch at point %d: mz=%d inten=%d want %d",
				start, len(dstMZ), len(dstInt), count)
		}
		unpackInPlace(dstMZ, p.mass)
		unpackInPlace(dstInt, p.inte)
		return dstMZ, dstInt, nil
	}

	if err := p.ensure(start, count); err != nil {
		return nil, nil, err
	}
	off := start - p.winFirst
	decodeStride(p.win[off*p.recSize:], p.recSize, p.massIntra, p.mass.Var.Type, count, dstMZ)
	decodeStride(p.win[off*p.recSize:], p.recSize, p.inteIntra, p.inte.Var.Type, count, dstInt)
	applyScale(dstMZ, p.mass)
	applyScale(dstInt, p.inte)
	return dstMZ, dstInt, nil
}

// ReadTimeValues returns the point-level time_values array for a scan, or nil
// when the variable is absent. ANDI exporters frequently leave it filled with
// the NetCDF fill value; callers must treat it as optional.
func (p *peakReader) ReadTimeValues(start, count int64) ([]float64, error) {
	if p.tv == nil || count == 0 {
		return nil, nil
	}
	out := make([]float64, 0, count)
	if !p.interleaved {
		return p.f.ReadFloats(p.tv.Var, start, count, out)
	}
	if err := p.ensure(start, count); err != nil {
		return nil, err
	}
	off := start - p.winFirst
	out = append(out, make([]float64, count)...)
	decodeStride(p.win[off*p.recSize:], p.recSize, p.tvIntra, p.tv.Var.Type, count, out)
	applyScale(out, p.tv)
	return out, nil
}

// decodeStride reads n values of type t out of an interleaved record buffer and
// leaves them raw (no packing applied).
func decodeStride(buf []byte, recSize, intra int64, t netcdfio.Type, n int64, out []float64) {
	ts := int64(t.Size())
	for i := int64(0); i < n; i++ {
		base := i*recSize + intra
		if base+ts > int64(len(buf)) {
			break
		}
		b := buf[base : base+ts]
		var v float64
		switch t {
		case netcdfio.TypeFloat:
			v = float64(math.Float32frombits(be32b(b)))
		case netcdfio.TypeDouble:
			v = math.Float64frombits(be64b(b))
		case netcdfio.TypeByte:
			v = float64(int8(b[0]))
		case netcdfio.TypeUByte:
			v = float64(b[0])
		case netcdfio.TypeShort:
			v = float64(int16(be16b(b)))
		case netcdfio.TypeUShort:
			v = float64(be16b(b))
		case netcdfio.TypeInt:
			v = float64(int32(be32b(b)))
		case netcdfio.TypeUInt:
			v = float64(be32b(b))
		case netcdfio.TypeInt64:
			v = float64(int64(be64b(b)))
		case netcdfio.TypeUInt64:
			v = float64(be64b(b))
		}
		out[i] = v
	}
}

func applyScale(v []float64, r *VarRef) {
	if r == nil || !r.Packed {
		return
	}
	for i := range v {
		v[i] = v[i]*r.Scale + r.Add
	}
}

func unpackInPlace(v []float64, r *VarRef) { applyScale(v, r) }

func be16b(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func be32b(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
func be64b(b []byte) uint64 {
	return uint64(be32b(b))<<32 | uint64(be32b(b[4:]))
}
