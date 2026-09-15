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

// SpectrumWriter streams normalized spectra into one output document.
//
// mzml.Writer and mzxml.Writer both satisfy it. A writer owns its output file:
// it writes to a temporary path and publishes the final path only once the
// document is complete and internally consistent.
type SpectrumWriter interface {
	// Write appends one spectrum. Spectra must arrive with increasing Index.
	Write(s *Spectrum) error

	// Close flushes trailers (indexes, checksums) and verifies that the number
	// of spectra actually written matches the count the document header
	// declared. A mismatch returns an ErrCountMismatch-typed error and leaves
	// the artifact at <path>.partial instead of publishing it.
	Close() error

	// Abort discards pending state and removes the partial artifact.
	Abort() error

	// Summary reports what was produced, for reports and provenance.
	Summary() WriteSummary
}

// WriteSummary is the format-neutral result of writing one document.
type WriteSummary struct {
	Format       string       `json:"format"`
	Version      string       `json:"version"`
	Path         string       `json:"path"`
	Bytes        int64        `json:"bytes"`
	Spectra      int64        `json:"spectra"`
	Points       int64        `json:"points"`
	Indexed      bool         `json:"indexed,omitempty"`
	Compressed   bool         `json:"compressed,omitempty"`
	F32Arrays    int          `json:"f32Arrays"`
	F64Arrays    int          `json:"f64Arrays"`
	EmptySpectra int          `json:"emptySpectra,omitempty"`
	SourceSHA1   string       `json:"sourceSha1,omitempty"`
	SourceSHA256 string       `json:"sourceSha256,omitempty"`
	Warnings     []Diagnostic `json:"warnings,omitempty"`
}
