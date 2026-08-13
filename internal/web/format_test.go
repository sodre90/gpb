package web

import "testing"

func TestCountsAreGroupedForReading(t *testing.T) {
	for _, each := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{1000, "1,000"},
		{44164, "44,164"},
		{1234567, "1,234,567"},
	} {
		if got := humanCount(each.n); got != each.want {
			t.Errorf("humanCount(%d) = %q, want %q", each.n, got, each.want)
		}
	}
}

func TestANounAgreesWithItsCount(t *testing.T) {
	for _, each := range []struct {
		n    int
		want string
	}{
		{0, "0 albums"},
		{1, "1 album"},
		{2, "2 albums"},
		{45013, "45,013 albums"},
	} {
		if got := quantity(each.n, "album"); got != each.want {
			t.Errorf("quantity(%d) = %q, want %q", each.n, got, each.want)
		}
	}
}
