package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	doclinkv1 "github.com/raveesh-me/doclink/gen/doclink/v1"
	"github.com/raveesh-me/doclink/gen/doclink/v1/doclinkv1connect"
)

// policy computes the Content-Security-Policy served with the shell.
//
// The split matters and is the reason this exists at all:
//
//	connect-src  PIM's own backends. PIM knows these — they are its
//	             dependencies, configured at deploy time alongside config.js.
//	frame-src    the satellites'. PIM cannot know these, and must not have to
//	             be redeployed when one appears, so they are read from the
//	             registry at runtime.
//
// The registry alone is not a sufficient source, though. Deriving the allowlist
// purely from it would mean a rogue registry row adds its own origin to the
// policy that is supposed to constrain it — protection that evaporates against
// the one attacker it names. So registry origins are filtered through
// allowedPatterns, which comes from deployment configuration the registry
// cannot write.
//
// The result keeps both properties: a satellite on an approved domain still
// needs zero per-satellite configuration, and a registry row pointing somewhere
// else is dropped and logged rather than framed.
type policy struct {
	registry       doclinkv1connect.RegistryServiceClient
	extensionPoint string
	// connectOrigins are PIM's own API origins, from configuration.
	connectOrigins []string
	// allowedPatterns constrains which registry origins may be framed at all.
	// Empty means unconstrained, which is logged loudly at startup.
	allowedPatterns []originPattern
	log             *slog.Logger

	mu          sync.RWMutex
	frameSrc    []string
	lastRefresh time.Time
	everLoaded  bool
}

func newPolicy(registryURL, extensionPoint string, connectOrigins, allowed []string,
	log *slog.Logger) *policy {

	l := log.With("component", "csp")
	patterns := parsePatterns(allowed, l)
	if len(patterns) == 0 {
		l.Error("ALLOWED_EMBED_ORIGINS is empty: every origin the registry names " +
			"will be framed, so a rogue registry row is not constrained by this policy")
	}
	return &policy{
		registry: doclinkv1connect.NewRegistryServiceClient(
			&http.Client{Timeout: 5 * time.Second}, registryURL),
		extensionPoint:  extensionPoint,
		connectOrigins:  normalizeOrigins(connectOrigins),
		allowedPatterns: patterns,
		log:             l,
	}
}

// originPattern matches an embed origin against deployment policy.
//
// Scheme must match exactly. Host is either exact or a "*." wildcard covering
// one or more leading labels. A pattern with no port matches any port, which is
// what makes "http://*.doclink.localhost" cover every satellite behind one
// ingress without naming them.
type originPattern struct {
	scheme   string
	host     string // without wildcard prefix
	wildcard bool
	port     string // empty means any
	raw      string
}

func parsePatterns(in []string, log *slog.Logger) []originPattern {
	var out []originPattern
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		probe := strings.Replace(raw, "*.", "wildcard-placeholder.", 1)
		u, err := url.Parse(probe)
		if err != nil || u.Scheme == "" || u.Hostname() == "" {
			log.Warn("ignoring unparseable ALLOWED_EMBED_ORIGINS entry", "entry", raw)
			continue
		}
		host := u.Hostname()
		wild := strings.Contains(raw, "*.")
		if wild {
			host = strings.TrimPrefix(host, "wildcard-placeholder.")
		}
		out = append(out, originPattern{
			scheme: u.Scheme, host: host, wildcard: wild, port: u.Port(), raw: raw,
		})
	}
	return out
}

func (p originPattern) matches(u *url.URL) bool {
	if u.Scheme != p.scheme {
		return false
	}
	if p.port != "" && u.Port() != p.port {
		return false
	}
	h := u.Hostname()
	if p.wildcard {
		// "*.doclink.localhost" covers "subs.doclink.localhost" but not the
		// bare "doclink.localhost", and never a suffix collision like
		// "evildoclink.localhost".
		return strings.HasSuffix(h, "."+p.host)
	}
	return h == p.host
}

func (p *policy) allowed(u *url.URL) bool {
	if len(p.allowedPatterns) == 0 {
		return true
	}
	for _, pat := range p.allowedPatterns {
		if pat.matches(u) {
			return true
		}
	}
	return false
}

// Refresh reloads the allowed frame origins from the registry.
//
// On failure the previous list is kept: a registry blip must not retroactively
// break cards that are already registered and working.
func (p *policy) Refresh(ctx context.Context) error {
	resp, err := p.registry.ListContributions(ctx, connect.NewRequest(
		&doclinkv1.ListContributionsRequest{ExtensionPointId: p.extensionPoint}))
	if err != nil {
		return fmt.Errorf("list contributions: %w", err)
	}

	seen := map[string]struct{}{}
	for _, c := range resp.Msg.Contributions {
		u, err := url.Parse(c.EmbedUrl)
		if err != nil || u.Scheme == "" || u.Host == "" {
			// A contribution we cannot parse is a contribution we will not
			// frame. Saying so is better than widening the policy to cover it.
			p.log.Warn("contribution has an unusable embed_url; it will be blocked",
				"contribution", c.Id, "embed_url", c.EmbedUrl)
			continue
		}
		if !p.allowed(u) {
			// The case this whole mechanism exists for. Loud, because it is
			// either a misconfigured satellite or someone writing rows they
			// should not be able to write, and both need a human.
			p.log.Error("embed origin is outside ALLOWED_EMBED_ORIGINS; refusing to frame it",
				"contribution", c.Id, "namespace", c.Namespace, "embed_url", c.EmbedUrl)
			continue
		}
		seen[u.Scheme+"://"+u.Host] = struct{}{}
	}

	origins := make([]string, 0, len(seen))
	for o := range seen {
		origins = append(origins, o)
	}
	sort.Strings(origins)

	p.mu.Lock()
	changed := strings.Join(p.frameSrc, " ") != strings.Join(origins, " ")
	p.frameSrc = origins
	p.lastRefresh = time.Now()
	p.everLoaded = true
	p.mu.Unlock()

	if changed {
		p.log.Info("frame-src updated", "origins", origins)
	}
	return nil
}

// Run keeps the list current. A satellite that registers after the shell
// started must become frameable without a redeploy, which is the entire premise.
func (p *policy) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Refresh(ctx); err != nil {
				p.log.Warn("refresh failed; serving last known origins", "err", err)
			}
		}
	}
}

// Header renders the policy.
//
// If the registry has never answered, frame-src ends up empty, which blocks
// every card. That is deliberate and costs nothing: the shell discovers its
// cards through the same call, so a registry that cannot answer already means
// no cards to frame. Failing open here would trade real protection for no
// benefit at all.
func (p *policy) Header() string {
	p.mu.RLock()
	frame := append([]string(nil), p.frameSrc...)
	loaded := p.everLoaded
	p.mu.RUnlock()

	frameSrc := "'none'"
	if len(frame) > 0 {
		frameSrc = strings.Join(frame, " ")
	} else if !loaded {
		frameSrc = "'none'"
	}

	connect := append([]string{"'self'"}, p.connectOrigins...)

	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'",
		// Vue writes style="" attributes from templates, and Vite injects a
		// style element for scoped CSS in dev. Narrowing this further would
		// require a nonce pipeline the POC does not have.
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src " + strings.Join(connect, " "),
		"frame-src " + frameSrc,
		// The shell is top-level; nothing should ever frame it.
		"frame-ancestors 'none'",
		"base-uri 'self'",
		"form-action 'none'",
		"object-src 'none'",
	}, "; ")
}

// Status is exposed on /healthz for debugging a card that will not load, which
// otherwise looks identical to a card that is broken.
func (p *policy) Status() (origins []string, age time.Duration, loaded bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.frameSrc...), time.Since(p.lastRefresh), p.everLoaded
}

func normalizeOrigins(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		out = append(out, u.Scheme+"://"+u.Host)
	}
	sort.Strings(out)
	return out
}
