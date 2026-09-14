package pim

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// The architecture rests on one boundary: PIM never compiles a module's code or
// types. If it did, it could marshal them as Any, switch on them, or import a
// module's helpers "just this once", and the registry would stop being the only
// way PIM learns a module exists.
//
// This walks the full transitive import graph — test files included — of PIM
// and its binary, and fails on any path into a module.

const modulePath = "github.com/raveesh-me/doclink"

var roots = []string{
	modulePath + "/internal/pim/...",
	modulePath + "/cmd/pim",
}

// forbidden reports whether importPath is module code or module-owned types.
// Modules' generated protos live under gen/modules, which would put them in
// protoregistry.GlobalTypes in the PIM binary; that is as much a breach as
// importing the implementation.
func forbidden(importPath string) bool {
	return strings.Contains(importPath, "internal/modules") || strings.Contains(importPath, "gen/modules")
}

func TestNoModuleImports(t *testing.T) {
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedImports | packages.NeedDeps,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, roots...)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// A graph that failed to load would pass vacuously.
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("%d package errors; is gen/ generated? (make proto)", n)
	}
	rootSeen := map[string]bool{}
	for _, p := range pkgs {
		rootSeen[p.PkgPath] = true
	}
	for _, want := range []string{modulePath + "/internal/pim", modulePath + "/cmd/pim"} {
		if !rootSeen[want] {
			t.Fatalf("root package %s not loaded; the check would be vacuous", want)
		}
	}

	// Walk every edge, remembering how each package was reached so a failure
	// names the chain, not just the offender.
	via := map[string]string{}
	var violations []string
	seen := map[string]bool{}
	var walk func(p *packages.Package)
	walk = func(p *packages.Package) {
		if seen[p.ID] {
			return
		}
		seen[p.ID] = true
		for path, dep := range p.Imports {
			if _, ok := via[path]; !ok {
				via[path] = p.PkgPath
			}
			if forbidden(path) {
				violations = append(violations, chain(via, path))
			}
			walk(dep)
		}
	}
	for _, p := range pkgs {
		walk(p)
	}

	// Prove the walk went deep enough to matter: the extension contract is a
	// transitive dependency of cmd/pim.
	if _, ok := via[modulePath+"/gen/ext/v1"]; !ok {
		t.Fatalf("walk never reached gen/ext/v1; the graph is not being traversed (saw %d packages)", len(seen))
	}

	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("PIM imports module code: %s", v)
	}
}

func chain(via map[string]string, path string) string {
	parts := []string{path}
	for i := 0; i < 32; i++ {
		parent, ok := via[path]
		if !ok || parent == path {
			break
		}
		parts = append([]string{parent}, parts...)
		path = parent
	}
	return strings.Join(parts, " -> ")
}

func TestForbiddenMatcher(t *testing.T) {
	for path, want := range map[string]bool{
		modulePath + "/internal/modules/taxes":  true,
		modulePath + "/internal/modules/modkit": true,
		modulePath + "/gen/modules/saas/v1":     true,
		modulePath + "/gen/ext/v1":              false,
		modulePath + "/internal/registry":       false,
	} {
		if got := forbidden(path); got != want {
			t.Errorf("forbidden(%q) = %v, want %v", path, got, want)
		}
	}
}
