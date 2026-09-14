package pim

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreakers() (*Breakers, *fakeClock) {
	c := &fakeClock{t: time.Unix(0, 0)}
	return &Breakers{Threshold: 5, Cooldown: 10 * time.Second, Now: c.now}, c
}

func TestBreakerOpensAfterFiveConsecutiveFailures(t *testing.T) {
	b, _ := newTestBreakers()
	for i := 0; i < 4; i++ {
		if !b.Allow("t1", "saas") {
			t.Fatalf("closed breaker refused call %d", i)
		}
		b.Record("t1", "saas", false)
	}
	// A success resets the count: failures must be consecutive.
	b.Allow("t1", "saas")
	b.Record("t1", "saas", true)
	for i := 0; i < 4; i++ {
		b.Allow("t1", "saas")
		b.Record("t1", "saas", false)
	}
	if !b.Allow("t1", "saas") {
		t.Fatal("opened after 4 consecutive failures")
	}
	b.Record("t1", "saas", false)
	if b.Allow("t1", "saas") {
		t.Fatal("still closed after 5 consecutive failures")
	}
	if !b.Allow("t1", "taxes") || !b.Allow("t2", "saas") {
		t.Fatal("breakers are not independent per (tenant, module)")
	}
}

func TestBreakerHalfOpenAdmitsOneProbe(t *testing.T) {
	b, clock := newTestBreakers()
	for i := 0; i < 5; i++ {
		b.Allow("t1", "saas")
		b.Record("t1", "saas", false)
	}
	clock.advance(9 * time.Second)
	if b.Allow("t1", "saas") {
		t.Fatal("admitted a call before the 10s cooldown")
	}
	clock.advance(time.Second)
	if !b.Allow("t1", "saas") {
		t.Fatal("refused the half-open probe")
	}
	if b.Allow("t1", "saas") {
		t.Fatal("admitted a second call while the probe is in flight")
	}

	// Failed probe: open for another full cooldown.
	b.Record("t1", "saas", false)
	clock.advance(5 * time.Second)
	if b.Allow("t1", "saas") {
		t.Fatal("failed probe did not reopen")
	}
	clock.advance(5 * time.Second)
	if !b.Allow("t1", "saas") {
		t.Fatal("refused the second probe")
	}

	// Successful probe: closed.
	b.Record("t1", "saas", true)
	for i := 0; i < 3; i++ {
		if !b.Allow("t1", "saas") {
			t.Fatal("successful probe did not close the breaker")
		}
	}
}

func TestBreakerAbandonedProbeFreesTheSlot(t *testing.T) {
	b, clock := newTestBreakers()
	for i := 0; i < 5; i++ {
		b.Allow("t1", "saas")
		b.Record("t1", "saas", false)
	}
	clock.advance(10 * time.Second)
	b.Allow("t1", "saas")
	b.Abandon("t1", "saas")
	if !b.Allow("t1", "saas") {
		t.Fatal("an abandoned probe left the breaker unable to probe again")
	}
}
