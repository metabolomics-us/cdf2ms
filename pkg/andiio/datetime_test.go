package andiio

import (
	"testing"
	"time"
)

func TestParseANDIDateTime(t *testing.T) {
	want := time.Date(2017, 3, 21, 7, 52, 39, 0, time.UTC)
	for _, s := range []string{"20170320235239-0800", " 20170320235239-0800 ", "20170321075239+0000", "20170321075239Z", "2017-03-20T23:52:39-08:00"} {
		got, ok := ParseANDIDateTime(s)
		if !ok || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("ParseANDIDateTime(%q) = %v, %v; want %v", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "20170320235239", "2017-03-20 23:52:39", "Mon Mar 20 2017"} {
		if got, ok := ParseANDIDateTime(s); ok {
			t.Errorf("ParseANDIDateTime(%q) = %v; want rejection (no UTC offset)", s, got)
		}
	}
}
