package main

import (
	"github.com/metabolomics-us/cdf2ms/pkg/mzml"
	"github.com/metabolomics-us/cdf2ms/pkg/mzxml"
)

// psiMSVersion is the PSI-MS CV release the embedded term table was verified
// against. It is printed by `version` and written into converted documents.
func psiMSVersion() string { return mzml.DefaultMSVersion }

// formatVersions documents the exact output standards, which the spec requires to
// be stated rather than implied.
func formatVersions() (mzmlVersion, mzxmlVersion string) {
	return mzml.SchemaVersion, mzxml.SchemaVersion
}
