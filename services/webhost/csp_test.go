package main

import (
	"log/slog"
	"net/url"
	"os"
	"testing"
)

func patterns(t *testing.T, in ...string) []originPattern {
	t.Helper()
	return parsePatterns(in, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

func TestOriginPatternMatching(t *testing.T) {
	cases := []struct {
		pattern string
		origin  string
		want    bool
		why     string
	}{
		{"http://*.doclink.localhost", "http://subs.doclink.localhost:18080", true,
			"wildcard covers a subdomain on any port"},
		{"http://*.doclink.localhost", "http://ship.doclink.localhost:18080", true,
			"and every other satellite, with no per-satellite config"},
		{"http://*.doclink.localhost", "http://doclink.localhost:18080", false,
			"the bare domain is not a subdomain of itself"},
		{"http://*.doclink.localhost", "http://evildoclink.localhost:18080", false,
			"suffix collision must not match: this is the attack the '.' guards"},
		{"http://*.doclink.localhost", "https://subs.doclink.localhost:18080", false,
			"scheme must match exactly"},
		{"http://*.doclink.localhost", "http://subs.doclink.localhost.evil.com", false,
			"a longer domain that merely contains ours must not match"},
		{"http://localhost", "http://localhost:5182", true,
			"a pattern with no port matches any port"},
		{"http://localhost", "http://localhost:5183", true, "same"},
		{"http://localhost:5182", "http://localhost:5183", false,
			"but an explicit port is exact"},
		{"http://localhost", "http://notlocalhost", false, "exact host otherwise"},
	}

	for _, c := range cases {
		p := patterns(t, c.pattern)
		if len(p) != 1 {
			t.Fatalf("pattern %q did not parse", c.pattern)
		}
		u, err := url.Parse(c.origin)
		if err != nil {
			t.Fatalf("origin %q: %v", c.origin, err)
		}
		if got := p[0].matches(u); got != c.want {
			t.Errorf("%q vs %q = %v, want %v — %s", c.pattern, c.origin, got, c.want, c.why)
		}
	}
}

func TestEmptyPatternsAllowEverything(t *testing.T) {
	// Documented behaviour, not an accident: an unconfigured deployment keeps
	// working and logs an error rather than silently blanking every card.
	p := &policy{}
	u, _ := url.Parse("http://anything.example.com")
	if !p.allowed(u) {
		t.Error("empty pattern list should allow all origins")
	}
}

func TestConfiguredPatternsRejectOutsiders(t *testing.T) {
	p := &policy{allowedPatterns: patterns(t, "http://*.doclink.localhost")}
	u, _ := url.Parse("http://evil.example.com/embed/card.html")
	if p.allowed(u) {
		t.Error("origin outside the allowlist was permitted")
	}
}
