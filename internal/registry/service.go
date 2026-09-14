package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	extv1 "github.com/raveesh-me/doclink/gen/ext/v1"
	"github.com/raveesh-me/doclink/gen/ext/v1/extv1connect"
	registryv1 "github.com/raveesh-me/doclink/gen/registry/v1"
	"github.com/raveesh-me/doclink/gen/registry/v1/registryv1connect"
)

// Invalidator evicts cached fragments. The composition engine's cache
// implements it; the registry calls it without knowing what a cache is.
type Invalidator interface {
	Invalidate(tenantID, moduleID string, productIDs []string) int
}

type Service struct {
	Store       *Store
	Schemas     *Schemas
	HTTPClient  connect.HTTPClient
	DrainGrace  time.Duration
	Invalidator Invalidator // optional
	Log         *slog.Logger
}

var _ registryv1connect.RegistryHandler = (*Service)(nil)

const (
	defaultTimeoutMS  = 500
	maxTimeoutMS      = 60_000
	describeTimeout   = 3 * time.Second
	maxModuleIDLength = 32
)

var (
	// reserved module_ids can never be extension keys: "core" would shadow
	// Product.core in a consumer's mental model, the others are PIM's own.
	reserved   = map[string]bool{"core": true, "pim": true, "_meta": true}
	moduleIDRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	pointRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
)

func (s *Service) RegisterModule(ctx context.Context, req *connect.Request[registryv1.RegisterModuleRequest]) (*connect.Response[registryv1.RegisterModuleResponse], error) {
	m := req.Msg
	if m.TenantId == "" {
		return nil, invalid("tenant_id is required")
	}
	baseURL, err := normalizeBaseURL(m.BaseUrl)
	if err != nil {
		return nil, invalid("base_url: " + err.Error())
	}
	timeoutMS := m.TimeoutMs
	if timeoutMS == 0 {
		timeoutMS = defaultTimeoutMS
	}
	if timeoutMS < 0 || timeoutMS > maxTimeoutMS {
		return nil, invalid(fmt.Sprintf("timeout_ms must be between 1 and %d", maxTimeoutMS))
	}
	mode := m.FailureMode
	if mode == registryv1.FailureMode_FAILURE_MODE_UNSPECIFIED {
		mode = registryv1.FailureMode_FAIL_OPEN
	}

	// The handshake. A URL that cannot answer Describe is rejected now, not
	// discovered later as a dead row that degrades every read.
	dctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	client := extv1connect.NewModuleExtensionClient(s.HTTPClient, baseURL)
	desc, err := client.Describe(dctx, connect.NewRequest(&extv1.DescribeRequest{}))
	if err != nil {
		s.Log.Warn("registration rejected: describe failed", "tenant", m.TenantId, "base_url", baseURL, "err", err)
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("describe %s failed: %w", baseURL, err))
	}
	d := desc.Msg
	if err := s.validateDescribe(d); err != nil {
		s.Log.Warn("registration rejected: invalid describe", "tenant", m.TenantId, "base_url", baseURL, "err", err)
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("module at %s rejected: %w", baseURL, err))
	}

	now := timestamppb.Now()
	rows := make([]*registryv1.ModuleRegistration, 0, len(d.Points))
	for _, p := range d.Points {
		rows = append(rows, &registryv1.ModuleRegistration{
			TenantId:        m.TenantId,
			ModuleId:        d.ModuleId,
			Version:         d.Version,
			BaseUrl:         baseURL,
			ExtensionPoint:  p.Name,
			CacheTtlSeconds: p.CacheTtlSeconds,
			ContextKeys:     p.ContextKeys,
			SchemaRef:       p.SchemaRef,
			JsonSchema:      p.JsonSchema,
			TimeoutMs:       timeoutMS,
			FailureMode:     mode,
			State:           registryv1.ModuleState_ACTIVE,
			RegisteredAt:    now,
			LastVerifiedAt:  now,
		})
	}
	err = s.Store.Insert(ctx, rows)
	if errors.Is(err, ErrInstalled) {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("module_id %q is already installed for tenant %q", d.ModuleId, m.TenantId))
	}
	if err != nil {
		return nil, s.internal("insert registration", err)
	}
	s.invalidate(m.TenantId, d.ModuleId, nil)
	s.Log.Info("module registered", "tenant", m.TenantId, "module", d.ModuleId, "version", d.Version,
		"points", len(rows), "base_url", baseURL, "timeout_ms", timeoutMS, "failure_mode", mode)
	return connect.NewResponse(&registryv1.RegisterModuleResponse{Registrations: rows}), nil
}

// validateDescribe reports every problem at once, so a module author fixes
// their Describe in one round trip rather than one error at a time.
func (s *Service) validateDescribe(d *extv1.DescribeResponse) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch id := d.ModuleId; {
	case reserved[id]:
		fail("module_id %q is reserved", id)
	case !moduleIDRE.MatchString(id) || len(id) > maxModuleIDLength:
		fail("module_id %q must be lowercase [a-z][a-z0-9_-]*, at most %d characters", id, maxModuleIDLength)
	}
	if d.Version == "" {
		fail("version is required")
	}
	if len(d.Points) == 0 {
		fail("at least one extension point is required")
	}

	names, refs := map[string]bool{}, map[string]bool{}
	propOwner := map[string]string{}
	for i, p := range d.Points {
		label := fmt.Sprintf("points[%d] (%s)", i, p.Name)
		if !pointRE.MatchString(p.Name) {
			fail("%s: name must be dotted lowercase", label)
		}
		if names[p.Name] {
			fail("%s: duplicate point name", label)
		}
		names[p.Name] = true
		if p.SchemaRef == "" {
			fail("%s: schema_ref is required", label)
		} else if refs[p.SchemaRef] {
			fail("%s: duplicate schema_ref %q", label, p.SchemaRef)
		}
		refs[p.SchemaRef] = true
		if p.CacheTtlSeconds < 0 {
			fail("%s: cache_ttl_seconds must not be negative", label)
		}
		keys := map[string]bool{}
		for _, k := range p.ContextKeys {
			if k == "" || keys[k] {
				fail("%s: context_keys must be non-empty and unique", label)
			}
			keys[k] = true
		}
		if _, err := s.Schemas.Compile(p.JsonSchema); err != nil {
			fail("%s: json_schema does not compile: %v", label, err)
			continue
		}
		props, err := topLevelProperties(p.JsonSchema)
		if err != nil {
			fail("%s: %v", label, err)
			continue
		}
		for _, prop := range props {
			if other, ok := propOwner[prop]; ok {
				fail("%s: property %q is also declared by point %s; a module's fragments merge into one object", label, prop, other)
			}
			propOwner[prop] = p.Name
		}
	}
	return errors.Join(errs...)
}

func (s *Service) UnregisterModule(ctx context.Context, req *connect.Request[registryv1.UnregisterModuleRequest]) (*connect.Response[registryv1.UnregisterModuleResponse], error) {
	m := req.Msg
	if m.TenantId == "" || m.ModuleId == "" {
		return nil, invalid("tenant_id and module_id are required")
	}
	rows, err := s.Store.List(ctx, m.TenantId, m.ModuleId)
	if err != nil {
		return nil, s.internal("list registration", err)
	}
	if len(rows) == 0 {
		return nil, notInstalled(m.TenantId, m.ModuleId)
	}
	if _, err := s.Store.SetState(ctx, m.TenantId, m.ModuleId, registryv1.ModuleState_DRAINING); err != nil {
		return nil, s.internal("drain", err)
	}

	// DRAINING drops the module from new fan-out immediately. Reads that
	// already resolved it keep their snapshot and finish against a module that
	// is still registered. Deletion waits out the grace period.
	deleteAfter := time.Now().Add(s.DrainGrace)
	tenant, module := m.TenantId, m.ModuleId
	time.AfterFunc(s.DrainGrace, func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		n, err := s.Store.DeleteDraining(dctx, tenant, module)
		if err != nil {
			s.Log.Error("drain delete failed", "tenant", tenant, "module", module, "err", err)
			return
		}
		s.invalidate(tenant, module, nil)
		s.Log.Info("module drained and deleted", "tenant", tenant, "module", module, "rows", n)
	})
	s.Log.Info("module draining", "tenant", tenant, "module", module, "delete_after", deleteAfter.Format(time.RFC3339Nano))
	return connect.NewResponse(&registryv1.UnregisterModuleResponse{DeleteAfter: timestamppb.New(deleteAfter)}), nil
}

func (s *Service) ListModules(ctx context.Context, req *connect.Request[registryv1.ListModulesRequest]) (*connect.Response[registryv1.ListModulesResponse], error) {
	if req.Msg.TenantId == "" {
		return nil, invalid("tenant_id is required")
	}
	rows, err := s.Store.List(ctx, req.Msg.TenantId, "")
	if err != nil {
		return nil, s.internal("list modules", err)
	}
	return connect.NewResponse(&registryv1.ListModulesResponse{Registrations: rows}), nil
}

func (s *Service) InvalidateFragments(ctx context.Context, req *connect.Request[registryv1.InvalidateFragmentsRequest]) (*connect.Response[registryv1.InvalidateFragmentsResponse], error) {
	m := req.Msg
	if m.TenantId == "" || m.ModuleId == "" {
		return nil, invalid("tenant_id and module_id are required")
	}
	rows, err := s.Store.List(ctx, m.TenantId, m.ModuleId)
	if err != nil {
		return nil, s.internal("list registration", err)
	}
	if len(rows) == 0 {
		return nil, notInstalled(m.TenantId, m.ModuleId)
	}
	n := s.invalidate(m.TenantId, m.ModuleId, m.ProductIds)
	s.Log.Info("fragments invalidated", "tenant", m.TenantId, "module", m.ModuleId,
		"products", len(m.ProductIds), "evicted", n)
	return connect.NewResponse(&registryv1.InvalidateFragmentsResponse{Evicted: int32(n)}), nil
}

func (s *Service) UpdateModulePolicy(ctx context.Context, req *connect.Request[registryv1.UpdateModulePolicyRequest]) (*connect.Response[registryv1.UpdateModulePolicyResponse], error) {
	m := req.Msg
	if m.TenantId == "" || m.ModuleId == "" {
		return nil, invalid("tenant_id and module_id are required")
	}
	if m.TimeoutMs < 0 || m.TimeoutMs > maxTimeoutMS {
		return nil, invalid(fmt.Sprintf("timeout_ms must be between 1 and %d", maxTimeoutMS))
	}
	if m.TimeoutMs == 0 && m.FailureMode == registryv1.FailureMode_FAILURE_MODE_UNSPECIFIED {
		return nil, invalid("set timeout_ms, failure_mode, or both")
	}
	n, err := s.Store.UpdatePolicy(ctx, m.TenantId, m.ModuleId, m.TimeoutMs, m.FailureMode)
	if err != nil {
		return nil, s.internal("update policy", err)
	}
	if n == 0 {
		return nil, notInstalled(m.TenantId, m.ModuleId)
	}
	rows, err := s.Store.List(ctx, m.TenantId, m.ModuleId)
	if err != nil {
		return nil, s.internal("list registration", err)
	}
	s.Log.Info("module policy updated", "tenant", m.TenantId, "module", m.ModuleId,
		"timeout_ms", rows[0].TimeoutMs, "failure_mode", rows[0].FailureMode)
	return connect.NewResponse(&registryv1.UpdateModulePolicyResponse{Registrations: rows}), nil
}

func (s *Service) invalidate(tenantID, moduleID string, productIDs []string) int {
	if s.Invalidator == nil {
		return 0
	}
	return s.Invalidator.Invalidate(tenantID, moduleID, productIDs)
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("scheme must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("host is required")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not carry a query or fragment")
	}
	return u.String(), nil
}

func (s *Service) internal(op string, err error) error {
	s.Log.Error(op, "err", err)
	return connect.NewError(connect.CodeInternal, errors.New(op+" failed"))
}

func invalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func notInstalled(tenantID, moduleID string) error {
	return connect.NewError(connect.CodeNotFound,
		fmt.Errorf("module %q is not installed for tenant %q", moduleID, tenantID))
}

// DefaultHTTPClient is used for handshakes and fan-out.
var DefaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	},
}
