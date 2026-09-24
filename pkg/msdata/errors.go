package msdata

import (
	"errors"
	"fmt"
)

// ErrEndOfData is returned by RunReader.Next when the last spectrum has been
// consumed. It is a normal terminator, not a failure.
var ErrEndOfData = errors.New("msdata: end of spectra")

// Sentinel error categories. They classify *why* something failed so callers can
// branch (retry vs. reject vs. ask the operator) without matching message text.
// Every sentinel is paired with a Code above for machine-readable reporting.
var (
	// ErrSourceUnsupported marks a file that cdf2ms cannot convert at all (not
	// ANDI/MS, missing peak arrays, no way to partition peaks into spectra).
	ErrSourceUnsupported = errors.New("cdf2ms: source is not convertible")
	// ErrCorruptSource marks a file whose structure contradicts its own header.
	ErrCorruptSource = errors.New("cdf2ms: source is corrupt")
	// ErrFileTooLarge marks a file that exceeds an explicit resource limit.
	ErrFileTooLarge = errors.New("cdf2ms: source exceeds configured limits")
	// ErrUnitUndetermined marks an retention-time unit that could not be
	// established without guessing; it always names an operator action.
	ErrUnitUndetermined = errors.New("cdf2ms: retention-time unit undetermined")
	// ErrNumericMismatch marks a read-back value that differs from the value
	// that was written. It is always fatal: cdf2ms never ships numbers it
	// cannot prove.
	ErrNumericMismatch = errors.New("cdf2ms: numeric read-back mismatch")
	// ErrValidationFailed marks output that failed schema or structural
	// validation.
	ErrValidationFailed = errors.New("cdf2ms: output validation failed")
	// ErrCountMismatch marks a document whose spectrum count differs from the
	// count its header promised.
	ErrCountMismatch = errors.New("cdf2ms: spectrum count mismatch")
	// ErrWriteFailed means output bytes could not be written, flushed, or moved
	// into place. Nothing partial is presented as a finished document.
	ErrWriteFailed = errors.New("cdf2ms: output write failed")
	// ErrOutputCollision marks a destination that already exists and would be
	// overwritten.
	ErrOutputCollision = errors.New("cdf2ms: output already exists")
	// ErrCancelled marks a run stopped by the user.
	ErrCancelled = errors.New("cdf2ms: cancelled")
)

// Code is a stable, machine-readable error/warning code. Reports and downstream
// automation must rely on these codes rather than on message text.
type Code string

const (
	// Source container problems.
	CodeNCOpenFailed           Code = "NC_OPEN_FAILED"
	CodeNCUnsupportedEncoding  Code = "NC_UNSUPPORTED_ENCODING"
	CodeNCHeaderCorrupt        Code = "NC_HEADER_CORRUPT"
	CodeNCVarNotFound          Code = "NC_VARIABLE_NOT_FOUND"
	CodeNCReadFailed           Code = "NC_READ_FAILED"
	CodeSourceTruncated        Code = "SOURCE_TRUNCATED"
	CodeSourceTooLarge         Code = "SOURCE_TOO_LARGE"
	CodeSourceUnreadablePoints Code = "SOURCE_UNREADABLE_POINTS"

	// ANDI mapping problems.
	CodeANDIRequiredVarMissing Code = "ANDI_REQUIRED_VARIABLE_MISSING"
	CodeANDIInvalidScanIndex   Code = "ANDI_INVALID_SCAN_INDEX"
	CodeANDIPointCountMismatch Code = "ANDI_POINT_COUNT_MISMATCH"
	// CodeANDIPointCountDerived records that point_count is absent and per-scan
	// peak counts were derived from scan_index. Nothing is wrong with the file;
	// the reader simply had to compute what the file never stated.
	CodeANDIPointCountDerived Code = "ANDI_POINT_COUNT_DERIVED"
	// CodeANDIScanLayoutAssumed records that neither scan_index nor point_count
	// exists and the one-peak-per-scan layout was assumed from the array extent.
	CodeANDIScanLayoutAssumed    Code = "ANDI_SCAN_LAYOUT_ASSUMED"
	CodeANDIUnknownRTUnit        Code = "ANDI_UNKNOWN_RT_UNIT"
	CodeANDIMassIntensityLenMis  Code = "ANDI_MASS_INTENSITY_LENGTH_MISMATCH"
	CodeANDIMissingRetentionTime Code = "ANDI_MISSING_RETENTION_TIME"
	CodeANDIInvalidNumeric       Code = "ANDI_INVALID_NUMERIC_VALUE"
	CodeANDIMetadataNotUTF8      Code = "ANDI_METADATA_NOT_UTF8"
	CodeANDIFFillValues          Code = "ANDI_FILL_VALUES_PRESENT"
	CodeANDINNegativePointCount  Code = "ANDI_NEGATIVE_POINT_COUNT"
	CodeANDIScanIndexNotMono     Code = "ANDI_SCAN_INDEX_NOT_MONOTONIC"
	CodeANDIMSLevelUnknown       Code = "ANDI_MS_LEVEL_UNKNOWN"
	CodeANDICentroidUnknown      Code = "ANDI_CENTROID_STATE_UNKNOWN"
	CodeANDIPolarityUnknown      Code = "ANDI_POLARITY_UNKNOWN"
	CodeANDIZeroPointScan        Code = "ANDI_ZERO_POINT_SCAN"
	CodeANDIScanIndexBase        Code = "ANDI_SCAN_INDEX_BASE_ASSUMED"
	CodeANDIPackingApplied       Code = "ANDI_PACKING_APPLIED"
	CodeANDIAmbiguousUnits       Code = "ANDI_AMBIGUOUS_UNITS"
	CodeANDIUnitInferredFromMag  Code = "ANDI_UNIT_INFERRED_FROM_MAGNITUDE"
	CodeANDIUsedPointTimeUnit    Code = "ANDI_USED_POINT_TIME_UNIT"
	// CodeANDIScanNumbersUnusable: the scan-number variable holds negative or
	// fill values (ANDI exporters write -9999 for "not recorded"), so every
	// spectrum is numbered by ordinal instead.
	CodeANDIScanNumbersUnusable Code = "ANDI_SCAN_NUMBERS_UNUSABLE"
	// CodeANDIAcquisitionTimeUnusable: experiment_date_time_stamp is present
	// but unparseable or carries no UTC offset, so the run start is omitted.
	CodeANDIAcquisitionTimeUnusable Code = "ANDI_ACQUISITION_TIME_UNUSABLE"

	// Output problems.
	CodeOutputWriteFailed Code = "OUTPUT_WRITE_FAILED"
	// CodeFileNotFound marks a path the operator named that does not exist.
	CodeFileNotFound Code = "FILE_NOT_FOUND"
	// CodeInvalidOption marks operator input that cannot be honoured.
	CodeInvalidOption Code = "INVALID_OPTION"
	// CodeOpenFailed marks a source that could not be opened at all.
	CodeOpenFailed Code = "OPEN_FAILED"
	// CodeReadFailed marks an I/O failure while streaming a source.
	CodeReadFailed            Code = "READ_FAILED"
	CodeOutputTempFailed      Code = "OUTPUT_TEMP_FAILED"
	CodeOutputRenameFailed    Code = "OUTPUT_RENAME_FAILED"
	CodeMZMLValidationFailed  Code = "MZML_VALIDATION_FAILED"
	CodeMZXMLValidationFailed Code = "MZXML_VALIDATION_FAILED"
	// CodeMZXMLInstrumentUnreported marks a source that states nothing about the
	// instrument, so <msInstrument> is omitted (mzXML requires every component
	// whenever the element is present).
	CodeMZXMLInstrumentUnreported Code = "MZXML_INSTRUMENT_UNREPORTED"
	// CodeMZXMLInstrumentPlaceholder marks an instrument component written as the
	// literal "Unknown" because mzXML has no way to express absence.
	CodeMZXMLInstrumentPlaceholder Code = "MZXML_INSTRUMENT_PLACEHOLDER"
	// CodeDiagnosticsTruncated marks that further diagnostics of a kind were
	// suppressed to keep memory bounded; the count is reported separately.
	CodeDiagnosticsTruncated Code = "DIAGNOSTICS_TRUNCATED"
	// CodeMZXMLScanNumberFallback marks scan/@num falling back to 1-based ordinals
	// because source scan numbers were absent, non-positive, or unordered.
	CodeMZXMLScanNumberFallback Code = "MZXML_SCAN_NUMBER_FALLBACK"
	CodeNumericMismatch         Code = "NUMERIC_MISMATCH"
	CodeNumericPrecisionLoss    Code = "NUMERIC_PRECISION_LOSS"
	CodeCountMismatch           Code = "COUNT_MISMATCH"
	CodeReportWriteFailed       Code = "REPORT_WRITE_FAILED"
	CodeDiscoveryFailed         Code = "DISCOVERY_FAILED"
	CodeCollision               Code = "OUTPUT_NAME_COLLISION"
	CodeCancelled               Code = "CANCELLED"
	// CodeVerified records that an output was re-opened and matched the source.
	CodeVerified Code = "OUTPUT_VERIFIED"
	// CodeSkipResumed records that a file was skipped because an earlier run
	// already produced (and reported) a matching output.
	CodeSkipResumed Code = "SKIPPED_RESUMED"
	// CodeFailFast records that a file was never started because an earlier file
	// failed and the run was told to stop on the first failure.
	CodeFailFast Code = "FAIL_FAST_ABORTED"
)

// Severity classifies a Diagnostics entry.
type Severity int

const (
	SeverityWarning Severity = iota
	SeverityError
)

// Diagnostic is a single structured warning or error.
type Diagnostic struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Scan    int    `json:"scan,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Diagnostics accumulates warnings and the first fatal error for a file.
//
// It is intentionally allocation-light: large corpora produce millions of
// spectra and we must not log per-spectrum noise.
type Diagnostics struct {
	Warnings []Diagnostic
	Fatal    *Diagnostic

	// counters dedupe repeated warnings: code -> count
	counts map[Code]int
	limit  int
}

// MaxPerCode is the number of individual diagnostics retained per code; beyond
// that only the aggregate counter grows.
const MaxPerCode = 8

// Warn records a warning, retaining at most MaxPerCode instances per code.
func (d *Diagnostics) Warn(code Code, scan int, format string, args ...any) {
	if d.counts == nil {
		d.counts = map[Code]int{}
	}
	d.counts[code]++
	if d.limit > 0 && len(d.Warnings) >= d.limit {
		return
	}
	if d.counts[code] <= MaxPerCode {
		d.Warnings = append(d.Warnings, Diagnostic{
			Code:    code,
			Message: fmt.Sprintf(format, args...),
			Scan:    scan,
		})
	}
}

// WarnDetail records a warning with extra machine-readable detail.
func (d *Diagnostics) WarnDetail(code Code, scan int, detail, format string, args ...any) {
	d.Warn(code, scan, format, args...)
	if len(d.Warnings) > 0 && d.counts[code] <= MaxPerCode {
		d.Warnings[len(d.Warnings)-1].Detail = detail
	}
}

// Fail records the first fatal error; later failures do not overwrite it.
func (d *Diagnostics) Fail(code Code, scan int, format string, args ...any) {
	if d.Fatal != nil {
		return
	}
	d.Fatal = &Diagnostic{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		Scan:    scan,
	}
}

// Count returns how many times a code was raised (including suppressed ones).
func (d *Diagnostics) Count(code Code) int {
	if d.counts == nil {
		return 0
	}
	return d.counts[code]
}

// CountMap exposes the per-code counters for reporting.
func (d *Diagnostics) CountMap() map[Code]int {
	if len(d.counts) == 0 {
		return nil
	}
	out := make(map[Code]int, len(d.counts))
	for k, v := range d.counts {
		out[k] = v
	}
	return out
}

// Failed reports whether a fatal error was recorded.
func (d *Diagnostics) Failed() bool { return d.Fatal != nil }

// Err returns the fatal diagnostic as an error.
func (d *Diagnostics) Err() error {
	if d.Fatal == nil {
		return nil
	}
	return fmt.Errorf("%s: %s", d.Fatal.Code, d.Fatal.Message)
}

// Merge folds another diagnostics into this one (used by workers).
func (d *Diagnostics) Merge(o *Diagnostics) {
	if o == nil {
		return
	}
	for _, w := range o.Warnings {
		d.Warn(w.Code, w.Scan, "%s", w.Message)
	}
	if o.Fatal != nil {
		d.Fail(o.Fatal.Code, o.Fatal.Scan, "%s", o.Fatal.Message)
	}
}

// TypedError wraps a Code with an underlying error for use in error returns.
type TypedError struct {
	Code    Code
	Message string
	Err     error
}

// Error implements error.
func (e *TypedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap implements errors.Unwrap.
func (e *TypedError) Unwrap() error { return e.Err }

// sentinelByCode pairs machine-readable codes with the sentinel category they
// belong to, so errors.Is(err, ErrCountMismatch) matches a TypedError carrying
// the paired CodeCountMismatch. Codes not listed here only match their wrapped
// cause.
var sentinelByCode = map[Code]error{
	CodeCountMismatch:          ErrCountMismatch,
	CodeNumericMismatch:        ErrNumericMismatch,
	CodeMZMLValidationFailed:   ErrValidationFailed,
	CodeMZXMLValidationFailed:  ErrValidationFailed,
	CodeCancelled:              ErrCancelled,
	CodeCollision:              ErrOutputCollision,
	CodeSourceTooLarge:         ErrFileTooLarge,
	CodeNCUnsupportedEncoding:  ErrSourceUnsupported,
	CodeNCVarNotFound:          ErrSourceUnsupported,
	CodeANDIRequiredVarMissing: ErrSourceUnsupported,
	CodeANDIUnknownRTUnit:      ErrUnitUndetermined,
	CodeANDIAmbiguousUnits:     ErrUnitUndetermined,
	CodeNCHeaderCorrupt:        ErrCorruptSource,
	CodeSourceTruncated:        ErrCorruptSource,
	CodeANDIInvalidScanIndex:   ErrCorruptSource,
	CodeANDIPointCountMismatch: ErrCorruptSource,
}

// Is reports whether this typed error belongs to the sentinel category paired
// with its code, in addition to matching its wrapped cause.
func (e *TypedError) Is(target error) bool {
	if s, ok := sentinelByCode[e.Code]; ok && s == target {
		return true
	}
	return e.Err != nil && errors.Is(e.Err, target)
}

// CodeOf extracts a Code from an error chain, or "" when unclassified.
func CodeOf(err error) Code {
	var te *TypedError
	if errors.As(err, &te) {
		return te.Code
	}
	return ""
}

// NewError builds a TypedError.
func NewError(code Code, format string, args ...any) *TypedError {
	return &TypedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WrapError builds a TypedError around a cause.
func WrapError(code Code, err error, format string, args ...any) *TypedError {
	return &TypedError{Code: code, Message: fmt.Sprintf(format, args...), Err: err}
}
