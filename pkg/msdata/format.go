package msdata

import "strconv"

func strconvFTOG(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
