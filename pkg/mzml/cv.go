// Package mzml writes PSI-MS mzML 1.1 documents.
//
// The writer is streaming (one spectrum at a time, bounded buffers), emits
// little-endian binary arrays as required by the mzML specification, and only
// emits controlled-vocabulary terms that exist in the PSI-MS ontology with the
// exact accession/name pair carried in the cvParam. That invariant is enforced
// by a test against a snapshot of the official ontology (see cv_test.go and
// scripts/fetch_psi_ms_cv.sh), because a cvParam whose name does not match its
// accession is rejected by downstream tools in confusing ways.
package mzml

import (
	"strings"

	"github.com/metabolomics-us/cdf2ms/pkg/msdata"
)

// CVTerm is a controlled-vocabulary term reference.
type CVTerm struct {
	Accession string
	Name      string
	// UnitTerm is the unit cvParam when the term requires one.
	UnitTerm *CVTerm
}

// CV namespace identifiers written into cvList.
const (
	NamespaceURI   = "http://psi.hupo.org/ms/mzml"
	XSIURI         = "http://www.w3.org/2001/XMLSchema-instance"
	SchemaLocation = NamespaceURI + " https://raw.githubusercontent.com/HUPO-PSI/mzML/master/schema/schema_1.1/mzML1.1.0.xsd"
	MSRef          = "MS"
	UORef          = "UO"
)

// CV terms used by this writer. Every accession/name pair below was taken from
// the official PSI-MS ontology (psi-ms.obo); the names are not paraphrased.
//
// Verification note: several pairs contradict common folklore (zlib compression
// is MS:1000574 not MS:1000576; "intensity array" not "peak intensity values";
// the second/minute units are UO:0000010/UO:0000031; "base64 array" does not
// exist in the current ontology and is therefore not emitted).
var (
	// Document structure
	TermFileFormatConversion = CVTerm{"MS:1000530", "file format conversion", nil}
	TermConversionToMzML     = CVTerm{"MS:1000544", "Conversion to mzML", nil}
	TermSoftware             = CVTerm{"MS:1000531", "software", nil}
	TermContactName          = CVTerm{"MS:1000586", "contact name", nil}
	TermContactAffiliation   = CVTerm{"MS:1000590", "contact affiliation", nil}
	TermContactAddress       = CVTerm{"MS:1000587", "contact address", nil}
	TermContactEmail         = CVTerm{"MS:1000589", "contact email", nil}
	TermSHA256               = CVTerm{"MS:1003151", "SHA-256", nil}
	TermAndiMSFormat         = CVTerm{"MS:1002441", "Andi-MS format", nil}
	TermNoNativeID           = CVTerm{"MS:1000824", "no nativeID format", nil}

	// Spectrum content
	TermMS1Spectrum     = CVTerm{"MS:1000579", "MS1 spectrum", nil}
	TermMSnSpectrum     = CVTerm{"MS:1000580", "MSn spectrum", nil}
	TermCentroid        = CVTerm{"MS:1000127", "centroid spectrum", nil}
	TermProfile         = CVTerm{"MS:1000128", "profile spectrum", nil}
	TermPositiveScan    = CVTerm{"MS:1000130", "positive scan", nil}
	TermNegativeScan    = CVTerm{"MS:1000129", "negative scan", nil}
	TermMSLevel         = CVTerm{"MS:1000511", "ms level", nil}
	TermTotalIonCurrent = CVTerm{"MS:1000285", "total ion current", nil}
	TermBasePeakMZ      = CVTerm{"MS:1000504", "base peak m/z", &TermMZ}
	TermBasePeakInt     = CVTerm{"MS:1000505", "base peak intensity", &TermCounts}
	TermLowestMZ        = CVTerm{"MS:1000528", "lowest observed m/z", &TermMZ}
	TermHighestMZ       = CVTerm{"MS:1000527", "highest observed m/z", &TermMZ}

	// Scan
	TermNoCombination = CVTerm{"MS:1000795", "no combination", nil}
	TermScanStartTime = CVTerm{"MS:1000016", "scan start time", &TermSecond}
	TermFilterString  = CVTerm{"MS:1000512", "filter string", nil}

	// Precursor
	TermIsolationTarget = CVTerm{"MS:1000827", "isolation window target m/z", &TermMZ}
	TermIsolationLower  = CVTerm{"MS:1000828", "isolation window lower offset", &TermMZ}
	TermIsolationUpper  = CVTerm{"MS:1000829", "isolation window upper offset", &TermMZ}
	TermSelectedIonMZ   = CVTerm{"MS:1000744", "selected ion m/z", &TermMZ}
	TermPeakIntensity   = CVTerm{"MS:1000042", "peak intensity", nil}
	TermChargeState     = CVTerm{"MS:1000041", "charge state", nil}
	TermCID             = CVTerm{"MS:1000133", "collision-induced dissociation", nil}
	TermCollisionEnergy = CVTerm{"MS:1000045", "collision energy", &TermElectronVolt}

	// Binary arrays
	TermMZArray       = CVTerm{"MS:1000514", "m/z array", &TermMZ}
	TermIntensityArr  = CVTerm{"MS:1000515", "intensity array", &TermCounts}
	TermFloat32       = CVTerm{"MS:1000521", "32-bit float", nil}
	TermFloat64       = CVTerm{"MS:1000523", "64-bit float", nil}
	TermNoCompression = CVTerm{"MS:1000576", "no compression", nil}
	TermZlib          = CVTerm{"MS:1000574", "zlib compression", nil}

	// Instrument
	TermMassSpectrometer = CVTerm{"MS:1000293", "mass spectrometer", nil}
	TermInstrumentModel  = CVTerm{"MS:1000031", "instrument model", nil}
	TermInstrumentVendor = CVTerm{"MS:1001269", "instrument vendor", nil}
	TermInstrumentSerial = CVTerm{"MS:1000529", "instrument serial number", nil}
	TermDetectorType     = CVTerm{"MS:1000026", "detector type", nil}

	// Units
	TermMZ           = CVTerm{"MS:1000040", "m/z", nil}
	TermCounts       = CVTerm{"MS:1000131", "number of detector counts", nil}
	TermSecond       = CVTerm{"UO:0000010", "second", nil}
	TermMinute       = CVTerm{"UO:0000031", "minute", nil}
	TermElectronVolt = CVTerm{"UO:0000266", "electronvolt", nil}

	// Sample
	TermSampleName = CVTerm{"MS:1000002", "sample name", nil}
)

// ionizationMap maps ANDI test_ionization_mode / test_ms_inlet wording onto
// PSI-MS "ionization type" terms. Only unambiguous, well-known vendor spellings
// are mapped; anything else is carried as a userParam instead of being guessed
// into a CV term.
var ionizationMap = []struct {
	needle string
	term   CVTerm
}{
	{"electron impact", CVTerm{"MS:1000389", "electron ionization", nil}},
	{"electron ionization", CVTerm{"MS:1000389", "electron ionization", nil}},
	{"atmospheric pressure chemical ionization", CVTerm{"MS:1000070", "atmospheric pressure chemical ionization", nil}},
	{"apci", CVTerm{"MS:1000070", "atmospheric pressure chemical ionization", nil}},
	{"chemical ionization", CVTerm{"MS:1000071", "chemical ionization", nil}},
	{"electrospray ionization", CVTerm{"MS:1000073", "electrospray ionization", nil}},
	{"electrospray", CVTerm{"MS:1000073", "electrospray ionization", nil}},
	{"esi", CVTerm{"MS:1000073", "electrospray ionization", nil}},
	{"matrix-assisted laser desorption ionization", CVTerm{"MS:1000075", "matrix-assisted laser desorption ionization", nil}},
	{"maldi", CVTerm{"MS:1000075", "matrix-assisted laser desorption ionization", nil}},
	{"fast atom bombardment ionization", CVTerm{"MS:1000074", "fast atom bombardment ionization", nil}},
	{"fab", CVTerm{"MS:1000074", "fast atom bombardment ionization", nil}},
	{"atmospheric pressure photoionization", CVTerm{"MS:1000382", "atmospheric pressure photoionization", nil}},
	{"appi", CVTerm{"MS:1000382", "atmospheric pressure photoionization", nil}},
}

// MapIonization returns the CV term for ANDI ionization wording, if any.
func MapIonization(s string) (CVTerm, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	if k == "" {
		return CVTerm{}, false
	}
	for _, e := range ionizationMap {
		if e.needle == "" {
			continue
		}
		if strings.Contains(k, e.needle) {
			return e.term, true
		}
	}
	return CVTerm{}, false
}

// detectorMap maps ANDI test_detector_type wording onto PSI-MS "detector type"
// child terms.
var detectorMap = []struct {
	needle string
	term   CVTerm
}{
	{"electron multiplier", CVTerm{"MS:1000253", "electron multiplier", nil}},
	{"microchannel plate", CVTerm{"MS:1000114", "microchannel plate detector", nil}},
	{"conversion dynode", CVTerm{"MS:1000346", "conversion dynode", nil}},
	{"faraday", CVTerm{"MS:1000112", "faraday cup", nil}},
	{"channeltron", CVTerm{"MS:1000107", "channeltron", nil}},
	{"array detector", CVTerm{"MS:1000345", "array detector", nil}},
	{"ion-to-photon", CVTerm{"MS:1000349", "ion-to-photon detector", nil}},
}

// MapDetector returns the CV term for ANDI detector wording, if any.
func MapDetector(s string) (CVTerm, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	if k == "" {
		return CVTerm{}, false
	}
	for _, e := range detectorMap {
		if strings.Contains(k, e.needle) {
			return e.term, true
		}
	}
	return CVTerm{}, false
}

// analyzerMap maps free-text instrument descriptions onto PSI-MS "mass analyzer"
// terms. ANDI rarely names the analyzer, so this only fires on explicit wording;
// an unmatched instrument simply has no componentList rather than a guessed one.
var analyzerMap = []struct {
	needle string
	term   CVTerm
}{
	{"quadrupole ion trap", CVTerm{"MS:1000082", "quadrupole ion trap", nil}},
	{"ion trap", CVTerm{"MS:1000082", "quadrupole ion trap", nil}},
	{"orbitrap", CVTerm{"MS:1003953", "orbitrap instrument", nil}},
	{"ion cyclotron", CVTerm{"MS:1003948", "fourier transform ion cyclotron resonance instrument", nil}},
	{"fourier transform", CVTerm{"MS:1003948", "fourier transform ion cyclotron resonance instrument", nil}},
	{"time-of-flight", CVTerm{"MS:1000287", "time-of-flight mass spectrometer", nil}},
	{"time of flight", CVTerm{"MS:1000287", "time-of-flight mass spectrometer", nil}},
	{"tof", CVTerm{"MS:1000287", "time-of-flight mass spectrometer", nil}},
	{"magnetic sector", CVTerm{"MS:1003949", "magnetic sector instrument", nil}},
	{"quadrupole", CVTerm{"MS:1000303", "transmission quadrupole mass spectrometer", nil}},
}

// MapAnalyzer returns a mass-analyzer term for free-text instrument wording.
func MapAnalyzer(texts ...string) (CVTerm, bool) {
	for _, raw := range texts {
		k := strings.ToLower(strings.TrimSpace(raw))
		if k == "" {
			continue
		}
		for _, e := range analyzerMap {
			if strings.Contains(k, e.needle) {
				return e.term, true
			}
		}
	}
	return CVTerm{}, false
}

// MapSeparation maps ANDI test_separation_type wording onto a PSI-MS separation
// term.
func MapSeparation(s string) (CVTerm, bool) {
	k := strings.ToLower(strings.TrimSpace(s))
	switch {
	case k == "":
		return CVTerm{}, false
	case strings.Contains(k, "gas chromat"):
		return CVTerm{"MS:1002272", "gas chromatography separation", nil}, true
	case strings.Contains(k, "liquid chromat"):
		return CVTerm{"MS:1002271", "liquid chromatography separation", nil}, true
	}
	return CVTerm{}, false
}

// spectrumLevelTerm picks the MS1/MSn content term for a spectrum.
func spectrumLevelTerm(level int) CVTerm {
	if level == 1 {
		return TermMS1Spectrum
	}
	return TermMSnSpectrum
}

// polarityTerm maps a polarity to its CV term.
func polarityTerm(p msdata.Polarity) (CVTerm, bool) {
	switch p {
	case msdata.PolarityPositive:
		return TermPositiveScan, true
	case msdata.PolarityNegative:
		return TermNegativeScan, true
	}
	return CVTerm{}, false
}

// centroidTerm maps a centroid state to its CV term.
func centroidTerm(c msdata.CentroidState) (CVTerm, bool) {
	switch c {
	case msdata.CentroidCentroided:
		return TermCentroid, true
	case msdata.CentroidProfile:
		return TermProfile, true
	}
	return CVTerm{}, false
}

// allTerms lists every CV term this package can emit. cv_test.go checks each one
// against a snapshot of the official PSI-MS ontology, so an accession/name pair
// can never drift out of sync with the ontology without a failing test.
func allTerms() []CVTerm {
	out := []CVTerm{
		TermFileFormatConversion, TermConversionToMzML, TermSoftware,
		TermContactName, TermContactAffiliation, TermContactAddress, TermContactEmail,
		TermSHA256, TermAndiMSFormat, TermNoNativeID,
		TermMS1Spectrum, TermMSnSpectrum, TermCentroid, TermProfile,
		TermPositiveScan, TermNegativeScan, TermMSLevel, TermTotalIonCurrent,
		TermBasePeakMZ, TermBasePeakInt, TermLowestMZ, TermHighestMZ,
		TermNoCombination, TermScanStartTime, TermFilterString,
		TermIsolationTarget, TermIsolationLower, TermIsolationUpper,
		TermSelectedIonMZ, TermPeakIntensity, TermChargeState, TermCID, TermCollisionEnergy,
		TermMZArray, TermIntensityArr, TermFloat32, TermFloat64, TermNoCompression, TermZlib,
		TermMassSpectrometer, TermInstrumentModel, TermInstrumentVendor, TermInstrumentSerial,
		TermDetectorType, TermMZ, TermCounts, TermSecond, TermMinute, TermElectronVolt,
		TermSampleName,
	}
	for _, e := range ionizationMap {
		out = append(out, e.term)
	}
	for _, e := range detectorMap {
		out = append(out, e.term)
	}
	for _, e := range analyzerMap {
		out = append(out, e.term)
	}
	out = append(out,
		CVTerm{"MS:1002272", "gas chromatography separation", nil},
		CVTerm{"MS:1002271", "liquid chromatography separation", nil},
	)
	return out
}

// UsedTerms returns the accession/name pairs this package can emit, for
// documentation and for the ontology conformance test.
func UsedTerms() [][2]string {
	seen := map[string]bool{}
	var out [][2]string
	for _, t := range allTerms() {
		if seen[t.Accession] {
			continue
		}
		seen[t.Accession] = true
		out = append(out, [2]string{t.Accession, t.Name})
	}
	return out
}
