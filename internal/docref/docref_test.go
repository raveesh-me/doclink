package docref

import "testing"

func TestRoundTrip(t *testing.T) {
	for _, s := range []string{
		"pim/item/01J8XYZ",
		"subscriptions/plan/monthly-5",
		"pim/item/tenant-a/sku-1", // ids may contain slashes
	} {
		r, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got := String(r); got != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "pim", "pim/item", "/item/1", "pim//1"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil error, want ErrMalformed", s)
		}
	}
}

func TestTypeKey(t *testing.T) {
	if got := TypeKey(MustParse("pim/item/1")); got != "pim/item" {
		t.Errorf("TypeKey = %q, want pim/item", got)
	}
}
