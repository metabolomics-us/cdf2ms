package msdata

import "context"

// RunReader streams a normalized run.
//
// Buffer contract: the *Spectrum returned by Next may reference buffers owned
// by the reader and is only valid until the next call to Next. Callers that
// need to keep a spectrum (validators, tests) must copy it with Clone.
type RunReader interface {
	// Metadata returns the run-level metadata. It is valid for the lifetime of
	// the reader and must be called before or after Next as needed.
	Metadata(ctx context.Context) (*Run, error)

	// Next returns the following spectrum, or ErrEndOfData when exhausted.
	// Implementations may reuse the returned struct and its arrays.
	Next(ctx context.Context) (*Spectrum, error)

	// Diagnostics exposes warnings and the fatal error, if any.
	Diagnostics() *Diagnostics

	// Stats exposes counters for reports (bytes read, spectra produced).
	Stats() ReaderStats

	Close() error
}

// ReaderStats counts work performed by a reader.
type ReaderStats struct {
	Spectra        int64 `json:"spectra"`
	Points         int64 `json:"points"`
	BytesRead      int64 `json:"bytes_read"`
	IndexArrays    int64 `json:"index_array_reads"`
	PeakArrayReads int64 `json:"peak_array_reads"`
}

// Clone returns a deep copy of a spectrum, safe to retain across Next calls.
func (s *Spectrum) Clone() *Spectrum {
	if s == nil {
		return nil
	}
	out := *s
	out.MZ = append([]float64(nil), s.MZ...)
	out.Intensity = append([]float64(nil), s.Intensity...)
	if s.Metadata != nil {
		out.Metadata = make(Metadata, len(s.Metadata))
		for k, v := range s.Metadata {
			out.Metadata[k] = v
		}
	}
	return &out
}

// WriteHint carries information a writer needs before the first spectrum.
type WriteHint struct {
	// ScanCount must be the exact number of spectra that will be written.
	// mzML's spectrumList/@count and mzXML's msRun/@scanCount are required
	// attributes that precede the payload, so a wrong value is a hard error at
	// Finish time rather than a silently invalid document.
	ScanCount int

	// PointCountTotal is advisory (used for mzXML numTuples-style summaries).
	PointCountTotal int64

	// SourceChecksumSHA256 is embedded as provenance when available.
	SourceChecksumSHA256 string
}

// SpectrumWriter writes a normalized run to a target format.
//
// The writer receives an already-buffered, seekable-free io.Writer: writers
// must stream and must not require rewinding.
type SpectrumWriter interface {
	// Begin writes the document header.
	Begin(ctx context.Context, run *Run, hint WriteHint) error

	// WriteSpectrum appends one spectrum.
	WriteSpectrum(ctx context.Context, s *Spectrum) error

	// Finish flushes trailers (indexes, checksums) and returns the result.
	// It must return an error if the number of written spectra differs from
	// hint.ScanCount.
	Finish(ctx context.Context) (Result, error)

	// Abort discards any pending state. The caller is responsible for removing
	// the temporary file.
	Abort() error
}

// Result describes a completed write.
type Result struct {
	Format      string            `json:"format"`
	Version     string            `json:"version"`
	Bytes       int64             `json:"bytes"`
	Spectra     int64             `json:"spectra"`
	Points      int64             `json:"points"`
	Indexed     bool              `json:"indexed"`
	Compressed  bool              `json:"compressed"`
	SHA1Payload string            `json:"sha1_payload,omitempty"`
	Checksums   map[string]string `json:"checksums,omitempty"`
}
