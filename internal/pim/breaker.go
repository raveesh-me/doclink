package pim

import (
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// ErrCircuitOpen is the failure recorded for a module skipped by its breaker.
var ErrCircuitOpen = errors.New("circuit open: module skipped without a call")

const (
	defaultBreakerThreshold = 5
	defaultBreakerCooldown  = 10 * time.Second
)

type breakerState string

const (
	stateClosed   breakerState = "closed"
	stateOpen     breakerState = "open"
	stateHalfOpen breakerState = "half_open"
)

// Breakers holds one in-memory circuit breaker per (tenant, module).
//
// Threshold consecutive failures open it for Cooldown; after that, half-open
// admits exactly one request. That probe's result closes it or reopens it.
type Breakers struct {
	Threshold int
	Cooldown  time.Duration
	Now       func() time.Time
	Log       *slog.Logger

	mu sync.Mutex
	m  map[breakerKey]*breaker
}

type breakerKey struct{ tenant, module string }

type breaker struct {
	state     breakerState
	failures  int
	openUntil time.Time
	probing   bool
}

func NewBreakers(log *slog.Logger) *Breakers {
	return &Breakers{Threshold: defaultBreakerThreshold, Cooldown: defaultBreakerCooldown, Now: time.Now, Log: log}
}

func (b *Breakers) get(tenant, module string) *breaker {
	if b.m == nil {
		b.m = map[breakerKey]*breaker{}
	}
	k := breakerKey{tenant, module}
	br, ok := b.m[k]
	if !ok {
		br = &breaker{state: stateClosed}
		b.m[k] = br
	}
	return br
}

// Allow reports whether a call may be made now. A true result from a half-open
// breaker makes that call the probe, and the caller must Record or Abandon it.
func (b *Breakers) Allow(tenant, module string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(tenant, module)
	switch br.state {
	case stateOpen:
		if b.Now().Before(br.openUntil) {
			return false
		}
		b.transition(tenant, module, br, stateHalfOpen)
		br.probing = true
		return true
	case stateHalfOpen:
		if br.probing {
			return false
		}
		br.probing = true
		return true
	default:
		return true
	}
}

// Record reports the result of an allowed call.
func (b *Breakers) Record(tenant, module string, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	br := b.get(tenant, module)
	switch br.state {
	case stateHalfOpen:
		br.probing = false
		if ok {
			br.failures = 0
			b.transition(tenant, module, br, stateClosed)
		} else {
			b.open(tenant, module, br)
		}
	case stateClosed:
		if ok {
			br.failures = 0
			return
		}
		br.failures++
		if br.failures >= b.Threshold {
			b.open(tenant, module, br)
		}
	case stateOpen:
		// A call admitted before the breaker opened, finishing late. It
		// proves nothing about the module now.
	}
}

// Abandon releases an allowed call whose result says nothing about the module:
// the read was cancelled or out of budget, or a FAIL_CLOSED sibling failed.
func (b *Breakers) Abandon(tenant, module string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if br := b.get(tenant, module); br.state == stateHalfOpen {
		br.probing = false
	}
}

func (b *Breakers) open(tenant, module string, br *breaker) {
	br.openUntil = b.Now().Add(b.Cooldown)
	b.transition(tenant, module, br, stateOpen)
}

func (b *Breakers) transition(tenant, module string, br *breaker, to breakerState) {
	if br.state == to && to != stateOpen {
		return
	}
	br.state = to
	if b.Log != nil {
		b.Log.Warn("circuit "+string(to), "tenant", tenant, "module", module, "failures", br.failures)
	}
}

// BreakerStatus is one breaker as reported by GET /debug/breakers.
type BreakerStatus struct {
	TenantID  string     `json:"tenant_id"`
	ModuleID  string     `json:"module_id"`
	State     string     `json:"state"`
	Failures  int        `json:"consecutive_failures"`
	OpenUntil *time.Time `json:"open_until,omitempty"`
}

func (b *Breakers) Snapshot() []BreakerStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BreakerStatus, 0, len(b.m))
	for k, br := range b.m {
		s := BreakerStatus{TenantID: k.tenant, ModuleID: k.module, State: string(br.state), Failures: br.failures}
		if br.state == stateOpen {
			t := br.openUntil
			s.OpenUntil = &t
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TenantID != out[j].TenantID {
			return out[i].TenantID < out[j].TenantID
		}
		return out[i].ModuleID < out[j].ModuleID
	})
	return out
}
