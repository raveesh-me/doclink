// Command demo is the scripted end-to-end walkthrough of the acceptance
// criteria. It starts PIM and both modules from bin/, drives them over plain
// HTTP/JSON — the same surface an API consumer sees — and asserts on every
// step. It exits non-zero if any assertion fails.
//
//	demo                   start everything and run the walkthrough
//	demo -register-only    against a running `make dev`: install t1 and t2's modules
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	pimURL   = "http://localhost:8080"
	taxesURL = "http://localhost:8081"
	saasURL  = "http://localhost:8082"

	// saasTTL is passed to module-saas so the stale-cache step does not wait
	// out the default 60s.
	saasTTL = 3 * time.Second
)

func main() {
	bin := flag.String("bin", "bin", "directory holding the built binaries")
	registerOnly := flag.Bool("register-only", false, "install t1 {taxes, saas} and t2 {taxes} against running services, then exit")
	keep := flag.Bool("keep", false, "keep the temp directory with the database and service logs")
	flag.Parse()

	d := &demo{bin: *bin, procs: map[string]*proc{}, http: &http.Client{Timeout: 10 * time.Second}}
	if *registerOnly {
		os.Exit(d.registerOnly())
	}
	os.Exit(d.run(*keep))
}

type demo struct {
	bin   string
	dir   string
	http  *http.Client
	procs map[string]*proc
	mu    sync.Mutex

	step     string
	passes   int
	failures []string
}

type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// -----------------------------------------------------------------------------
// the walkthrough

func (d *demo) run(keep bool) int {
	for _, port := range []string{"8080", "8081", "8082"} {
		l, err := net.Listen("tcp", ":"+port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "port %s is in use. The demo starts its own services; stop `make dev` first.\n", port)
			return 2
		}
		l.Close()
	}
	dir, err := os.MkdirTemp("", "pim-demo-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	d.dir = dir
	defer func() {
		d.stopAll()
		if keep {
			fmt.Printf("\nkept %s (pim.db, *.log)\n", dir)
		} else {
			os.RemoveAll(dir)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		d.stopAll()
		os.Exit(130)
	}()

	title("Starting services")
	if err := d.start("pim", "-db", filepath.Join(dir, "pim.db")); err != nil {
		return fatal(err)
	}
	if err := d.start("module-taxes"); err != nil {
		return fatal(err)
	}
	if err := d.start("module-saas", "-cache-ttl", fmt.Sprint(int(saasTTL.Seconds()))); err != nil {
		return fatal(err)
	}
	for _, u := range []string{pimURL, taxesURL, saasURL} {
		if err := d.waitHealthy(u); err != nil {
			return fatal(err)
		}
	}
	note("pim :8080 (fresh SQLite, seeded t1 and t2), taxes :8081, saas :8082")
	note("saas runs with -cache-ttl=%d (its default is 60) so step 9 need not wait a minute for a cache entry to expire", int(saasTTL.Seconds()))
	note("service logs: %s", dir)

	steps := []func(){
		d.registerModules,
		d.tenantsDiffer,
		d.buyerStateChangesTax,
		d.cacheHits,
		d.invalidation,
		d.batching,
		d.schemaViolation,
		d.spoofing,
		d.saasDown,
		d.failClosed,
		d.openAPI,
		d.drain,
	}
	for _, s := range steps {
		s()
	}

	title("Result")
	if len(d.failures) == 0 {
		fmt.Printf("  all %d checks passed\n", d.passes)
		return 0
	}
	fmt.Printf("  %d passed, %d FAILED:\n", d.passes, len(d.failures))
	for _, f := range d.failures {
		fmt.Printf("    ✘ %s\n", f)
	}
	return 1
}

// Criterion 1.
func (d *demo) registerModules() {
	d.begin(1, "Tenant t1 registers taxes and saas; tenant t2 registers only taxes")
	for _, r := range []struct{ tenant, url string }{{"t1", taxesURL}, {"t1", saasURL}, {"t2", taxesURL}} {
		status, raw := d.rpc("registry.v1.Registry/RegisterModule", map[string]any{"tenant_id": r.tenant, "base_url": r.url})
		d.check(status == 200, "RegisterModule(%s, %s) -> HTTP %d", r.tenant, r.url, status)
		if status != 200 {
			note("%s", raw)
		}
	}

	note("registration is a handshake: a URL that cannot answer Describe is refused")
	status, raw := d.rpc("registry.v1.Registry/RegisterModule", map[string]any{"tenant_id": "t1", "base_url": "http://localhost:8099"})
	d.check(status != 200 && strings.Contains(string(raw), "failed_precondition"), "RegisterModule(t1, :8099) refused: %s", raw)
	status, raw = d.rpc("registry.v1.Registry/RegisterModule", map[string]any{"tenant_id": "t1", "base_url": taxesURL})
	d.check(status != 200 && strings.Contains(string(raw), "already_exists"), "RegisterModule(t1, taxes) again refused: %s", raw)

	for tenant, want := range map[string]string{"t1": "saas,taxes", "t2": "taxes"} {
		rows := d.listModules(tenant)
		ids := map[string]bool{}
		for _, r := range rows {
			ids[r.ModuleID] = true
			note("%s  %-6s %-26s %-7s ttl=%-3d keys=%v", tenant, r.ModuleID, r.ExtensionPoint, r.State, r.CacheTTLSeconds, r.ContextKeys)
		}
		d.check(setString(ids) == want, "ListModules(%s) = {%s}", tenant, setString(ids))
	}
}

// Criterion 2.
func (d *demo) tenantsDiffer() {
	d.begin(2, "GetProduct for t1 returns both extensions; the same call for t2 returns one")
	ctx := map[string]string{"buyer_state": "KA"}
	for tenant, want := range map[string]string{"t1": "saas,taxes", "t2": "taxes"} {
		note("POST /pim.v1.PIM/GetProduct {\"tenant_id\":%q,\"id\":\"p1\",\"context\":{\"buyer_state\":\"KA\"}}", tenant)
		status, p, raw := d.getProduct(tenant, "p1", ctx)
		fmt.Println(indent(raw, "      "))
		d.check(status == 200 && keysOf(p.Extensions) == want && len(p.Degraded) == 0,
			"%s: extensions {%s}, degraded %v", tenant, keysOf(p.Extensions), p.Degraded)
		d.check(!bytes.Contains(raw, []byte("@type")) && !bytes.Contains(raw, []byte("type.googleapis.com")),
			"%s: plain JSON — no type URLs, no base64", tenant)
	}
}

// Criterion 3.
func (d *demo) buyerStateChangesTax() {
	d.begin(3, `context["buyer_state"] changes the computed tax fragment for the same product`)
	var fragments []map[string]any
	for _, state := range []string{"KA", "MH"} {
		_, p, _ := d.getProduct("t1", "p1", map[string]string{"buyer_state": state})
		tax := p.Extensions["taxes"]
		fragments = append(fragments, tax)
		note("buyer_state=%s  supply=%v rate=%v amountMinor=%v cgst=%v sgst=%v igst=%v",
			state, tax["supply"], tax["rate"], tax["amountMinor"], tax["cgstMinor"], tax["sgstMinor"], tax["igstMinor"])
	}
	d.check(fragments[0]["supply"] == "INTRA_STATE" && fragments[1]["supply"] == "INTER_STATE",
		"KA (seller's state) is intra-state CGST+SGST; MH is inter-state IGST")
	d.check(fragments[0]["hsn"] == fragments[1]["hsn"], "the context-free part (hsn %v) is unchanged", fragments[0]["hsn"])
}

// Criterion 11.
func (d *demo) cacheHits() {
	d.begin(11, "A second identical read hits the cache and makes zero module calls. Changing buyer_state misses the cache; changing an undeclared context key does not")
	d.resetStats()
	read := func(label string, ctx map[string]string, wantTaxes, wantSaas int64) {
		start := time.Now()
		status, _, _ := d.getProduct("t1", "p2", ctx)
		elapsed := time.Since(start)
		tx, sa := d.stats(taxesURL).EnrichCalls, d.stats(saasURL).EnrichCalls
		d.check(status == 200 && tx == wantTaxes && sa == wantSaas,
			"%-34s %6.1fms  module calls so far: taxes=%d saas=%d", label, ms(elapsed), tx, sa)
	}
	read("first read, buyer_state=KA", map[string]string{"buyer_state": "KA"}, 1, 1)
	read("identical read", map[string]string{"buyer_state": "KA"}, 1, 1)
	read("buyer_state=MH (declared key)", map[string]string{"buyer_state": "MH"}, 2, 1)
	read("+ channel=web (undeclared key)", map[string]string{"buyer_state": "MH", "channel": "web"}, 2, 1)
	note("taxes declares context_keys [buyer_state] for its contextual point; saas declares none")
}

// Criterion 12.
func (d *demo) invalidation() {
	d.begin(12, "InvalidateFragments for taxes forces the next read back onto the network")
	d.resetStats()
	ctx := map[string]string{"buyer_state": "KA"}
	_, before, _ := d.getProduct("t1", "p2", ctx)
	d.check(d.stats(taxesURL).EnrichCalls == 0, "read before: served from cache, taxes calls=0, hsn=%v taxClass=%v rate=%v",
		before.Extensions["taxes"]["hsn"], before.Extensions["taxes"]["taxClass"], before.Extensions["taxes"]["rate"])

	note("taxes reclassifies SKU-2 in its own data, then calls Registry.InvalidateFragments(t1, taxes, [p2]) — IDs only, no payload")
	status, raw := d.post(taxesURL+"/admin/reclassify", map[string]any{
		"tenant_id": "t1", "sku": "SKU-2", "hsn": "4902", "tax_class": "reduced", "product_ids": []string{"p2"},
	})
	d.check(status == 200, "POST taxes /admin/reclassify -> %s", strings.TrimSpace(string(raw)))

	_, after, _ := d.getProduct("t1", "p2", ctx)
	d.check(d.stats(taxesURL).EnrichCalls == 1 && after.Extensions["taxes"]["hsn"] == "4902",
		"read after: taxes calls=1, hsn=%v taxClass=%v rate=%v", after.Extensions["taxes"]["hsn"],
		after.Extensions["taxes"]["taxClass"], after.Extensions["taxes"]["rate"])
}

// Criterion 8.
func (d *demo) batching() {
	d.begin(8, "ListProducts with 50 products makes exactly one EnrichProducts call per module, not 50")
	for i := 1; i <= 45; i++ {
		status, raw := d.rpc("pim.v1.PIM/UpsertProduct", map[string]any{"product": map[string]any{
			"tenant_id": "t1", "id": fmt.Sprintf("b%02d", i), "sku": fmt.Sprintf("SKU-B%02d", i),
			"title": fmt.Sprintf("Bulk item %d", i), "price_minor": 10000 + i*100, "currency": "INR",
		}})
		if status != 200 {
			d.check(false, "UpsertProduct b%02d: %s", i, raw)
			return
		}
	}
	note("upserted 45 products into t1 (UpsertProduct touches no module); t1 now has 50")

	d.resetStats()
	status, raw := d.rpc("pim.v1.PIM/ListProducts", map[string]any{"tenant_id": "t1", "page_size": 100, "context": map[string]string{"buyer_state": "TN"}})
	var res struct {
		Products []product `json:"products"`
	}
	_ = json.Unmarshal(raw, &res)
	withTaxes := 0
	for _, p := range res.Products {
		if p.Extensions["taxes"] != nil {
			withTaxes++
		}
	}
	tx, sa := d.stats(taxesURL), d.stats(saasURL)
	d.check(status == 200 && len(res.Products) == 50 && withTaxes == 50, "ListProducts returned %d products, %d with taxes", len(res.Products), withTaxes)
	d.check(tx.EnrichCalls == 1, "taxes: %d EnrichProducts call carrying %d products", tx.EnrichCalls, tx.EnrichProducts)
	d.check(sa.EnrichCalls == 1, "saas:  %d EnrichProducts call carrying %d products (the rest were cache hits)", sa.EnrichCalls, sa.EnrichProducts)
}

// Criterion 6.
func (d *demo) schemaViolation() {
	d.begin(6, "A fragment that violates its registered schema is dropped, logged, and marked degraded; the response is still well-formed")
	d.chaos("bad_schema")
	d.invalidateOnBehalfOf("saas")
	mark := d.logSize("pim")
	status, p, raw := d.getProduct("t1", "p3", map[string]string{"buyer_state": "KA"})
	fmt.Println(indent(raw, "      "))
	d.check(status == 200 && p.Core["id"] == "p3", "HTTP %d, core intact", status)
	d.check(p.Extensions["saas"] == nil && p.Extensions["taxes"] != nil && reflect.DeepEqual(p.Degraded, []string{"saas"}),
		"saas dropped, taxes kept, degraded %v", p.Degraded)
	lines := d.logSince("pim", mark, "fragments dropped")
	d.check(len(lines) == 1 && strings.Contains(lines[0], "violates acme.saas/v1"), "PIM logged it:")
	for _, l := range lines {
		note("%s", trimLog(l))
	}
}

// Criterion 7.
func (d *demo) spoofing() {
	d.begin(7, `A module attempting to write extensions["core"] or extensions["taxes"] while registered as saas is rejected at merge`)
	for _, c := range []struct{ mode, claim string }{{"spoof_core", "core"}, {"spoof_taxes", "taxes"}} {
		d.chaos(c.mode)
		d.invalidateOnBehalfOf("saas")
		mark := d.logSize("pim")
		status, p, _ := d.getProduct("t1", "p3", map[string]string{"buyer_state": "KA"})
		d.check(status == 200 && keysOf(p.Extensions) == "taxes" && reflect.DeepEqual(p.Degraded, []string{"saas"}),
			"saas claims module_id %q: extensions {%s}, degraded %v", c.claim, keysOf(p.Extensions), p.Degraded)
		if c.claim == "taxes" {
			d.check(p.Extensions["taxes"]["hsn"] == "6109", "extensions.taxes is still the real taxes module's (hsn %v, not the spoofed 0000)", p.Extensions["taxes"]["hsn"])
		}
		for _, l := range d.logSince("pim", mark, "fragments dropped") {
			note("%s", trimLog(l))
		}
	}
	d.chaos("off")
	d.invalidateOnBehalfOf("saas")
	_, p, _ := d.getProduct("t1", "p3", map[string]string{"buyer_state": "KA"})
	d.check(p.Extensions["saas"] != nil && len(p.Degraded) == 0, "chaos off: saas is back (a success resets its breaker's failure count)")
}

// Criteria 4 and 13.
func (d *demo) saasDown() {
	d.begin(4, `Killing the saas process makes t1 reads succeed with degraded: ["saas"]`)
	ctx := map[string]string{"buyer_state": "KA"}
	_, warm, _ := d.getProduct("t1", "p1", ctx)
	note("warmed the cache for t1/p1 (saas seats=%v); waiting %v for saas's TTL to lapse", warm.Extensions["saas"]["seats"], saasTTL)
	time.Sleep(saasTTL + 200*time.Millisecond)

	d.kill("module-saas")
	note("killed module-saas (SIGKILL)")

	status, raw := d.rpc("pim.v1.PIM/UpsertProduct", map[string]any{"product": map[string]any{
		"tenant_id": "t1", "id": "p-new", "sku": "SKU-NEW", "title": "Brand new", "price_minor": 99900, "currency": "INR",
	}})
	d.check(status == 200, "UpsertProduct t1/p-new succeeds with saas down: writes never touch modules")
	if status != 200 {
		note("%s", raw)
	}
	status, p, raw := d.getProduct("t1", "p-new", ctx)
	fmt.Println(indent(raw, "      "))
	d.check(status == 200 && p.Extensions["saas"] == nil && p.Extensions["taxes"] != nil && reflect.DeepEqual(p.Degraded, []string{"saas"}),
		"t1/p-new (never cached): HTTP %d, extensions {%s}, degraded %v", status, keysOf(p.Extensions), p.Degraded)
	status, p, _ = d.getProduct("t2", "p1", ctx)
	d.check(status == 200 && len(p.Degraded) == 0, "t2 (no saas installed) is unaffected: degraded %v", p.Degraded)

	d.begin(13, `With saas down and a warm cache, FAIL_OPEN serves the stale fragment and still reports degraded: ["saas"]`)
	mark := d.logSize("pim")
	status, p, raw = d.getProduct("t1", "p1", ctx)
	fmt.Println(indent(raw, "      "))
	d.check(status == 200 && p.Extensions["saas"] != nil && p.Extensions["saas"]["seats"] == warm.Extensions["saas"]["seats"] &&
		reflect.DeepEqual(p.Degraded, []string{"saas"}),
		"t1/p1: stale saas served (seats=%v) and degraded %v", p.Extensions["saas"]["seats"], p.Degraded)
	for _, l := range d.logSince("pim", mark, "stale") {
		note("%s", trimLog(l))
	}
}

// Criterion 5.
func (d *demo) failClosed() {
	d.begin(5, "Flipping saas to FAIL_CLOSED makes the same read return CodeUnavailable naming saas")
	status, _ := d.rpc("registry.v1.Registry/UpdateModulePolicy", map[string]any{"tenant_id": "t1", "module_id": "saas", "failure_mode": "FAIL_CLOSED"})
	d.check(status == 200, "UpdateModulePolicy(t1, saas, FAIL_CLOSED) — no handshake, so it works with saas down")

	status, _, raw := d.getProduct("t1", "p1", map[string]string{"buyer_state": "KA"})
	var cerr struct{ Code, Message string }
	_ = json.Unmarshal(raw, &cerr)
	note("HTTP %d %s", status, raw)
	d.check(status == 503 && cerr.Code == "unavailable" && strings.Contains(cerr.Message, `"saas"`),
		"HTTP 503, code %q, message names saas; the stale entry is not served under FAIL_CLOSED", cerr.Code)

	d.rpc("registry.v1.Registry/UpdateModulePolicy", map[string]any{"tenant_id": "t1", "module_id": "saas", "failure_mode": "FAIL_OPEN"})
	if err := d.start("module-saas", "-cache-ttl", fmt.Sprint(int(saasTTL.Seconds()))); err == nil {
		_ = d.waitHealthy(saasURL)
	}
	_, p, _ := d.getProduct("t1", "p-new", map[string]string{"buyer_state": "KA"})
	d.check(p.Extensions["saas"] != nil && len(p.Degraded) == 0, "back to FAIL_OPEN and saas restarted: t1/p-new has saas again")
	note("breakers: %s", d.getRaw(pimURL+"/debug/breakers"))
}

// Criterion 10.
func (d *demo) openAPI() {
	d.begin(10, "The two tenants' OpenAPI specs differ in exactly the expected way")
	specs := map[string]map[string]any{}
	for _, tenant := range []string{"t1", "t2"} {
		raw := d.getRaw(pimURL + "/openapi/" + tenant + ".json")
		var doc map[string]any
		err := json.Unmarshal(raw, &doc)
		d.check(err == nil, "GET /openapi/%s.json: %d bytes", tenant, len(raw))
		specs[tenant] = doc
		ext := dig(doc, "components", "schemas", "Product", "properties", "extensions", "properties")
		note("%s Product.extensions properties: {%s}", tenant, keysOf(ext))
	}

	var diffs []string
	jsonDiff("", specs["t2"], specs["t1"], &diffs)
	note("t2 -> t1 differences:")
	for _, df := range diffs {
		note("  %s", df)
	}
	want := []string{
		"+ components.schemas.Product.properties.extensions.properties.saas",
		"~ components.schemas.Product.properties.degraded.items.enum",
		"~ components.schemas.Product.properties.extensions.description",
		"~ info.description",
		"~ info.title",
	}
	d.check(reflect.DeepEqual(diffs, want), "exactly: saas added under extensions, saas added to the degraded enum, tenant named in titles and descriptions")

	taxes1 := dig(specs["t1"], "components", "schemas", "Product", "properties", "extensions", "properties", "taxes")
	taxes2 := dig(specs["t2"], "components", "schemas", "Product", "properties", "extensions", "properties", "taxes")
	d.check(taxes1 != nil && reflect.DeepEqual(taxes1, taxes2), "taxes is described identically for both tenants")
	saasSchema, _ := json.MarshalIndent(dig(specs["t1"], "components", "schemas", "Product", "properties", "extensions", "properties", "saas"), "      ", "  ")
	fmt.Printf("      extensions.saas in t1's spec, generated without PIM importing saas:\n      %s\n", saasSchema)
}

// Criterion 9.
func (d *demo) drain() {
	d.begin(9, "Unregistering taxes mid-flight drains without erroring in-flight reads")
	d.resetStats()

	const n = 20
	type result struct {
		status   int
		p        product
		finished time.Time
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A distinct buyer_state per read misses the cache, so every
			// read is a live 40ms call to taxes.
			status, p, _ := d.getProduct("t2", "p1", map[string]string{"buyer_state": fmt.Sprintf("D%02d", i)})
			results[i] = result{status, p, time.Now()}
		}()
	}

	// Wait until taxes has received all 20 calls: they are now inside its
	// 40ms sleep.
	deadline := time.Now().Add(2 * time.Second)
	for d.stats(taxesURL).EnrichCalls < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	inFlight := d.stats(taxesURL).EnrichCalls
	status, raw := d.rpc("registry.v1.Registry/UnregisterModule", map[string]any{"tenant_id": "t2", "module_id": "taxes"})
	unregistered := time.Now()
	var ur struct {
		DeleteAfter time.Time `json:"deleteAfter"`
	}
	_ = json.Unmarshal(raw, &ur)
	d.check(status == 200 && inFlight == n, "UnregisterModule(t2, taxes) issued with %d reads inside taxes -> delete after %s",
		inFlight, ur.DeleteAfter.Local().Format("15:04:05.000"))
	wg.Wait()

	ok, after, withTaxes := 0, 0, 0
	for _, r := range results {
		if r.status == 200 && len(r.p.Degraded) == 0 {
			ok++
		}
		if r.finished.After(unregistered) {
			after++
		}
		if r.p.Extensions["taxes"] != nil {
			withTaxes++
		}
	}
	d.check(ok == n && withTaxes == n, "%d/%d in-flight reads succeeded with taxes and no degraded marker", ok, n)
	d.check(after > 0, "%d of them completed after UnregisterModule returned", after)

	rows := d.listModules("t2")
	states := map[string]bool{}
	for _, r := range rows {
		states[r.State] = true
	}
	d.check(setString(states) == "DRAINING", "t2's taxes rows are DRAINING")
	status, p, _ := d.getProduct("t2", "p1", map[string]string{"buyer_state": "KA"})
	d.check(status == 200 && p.Extensions == nil && len(p.Degraded) == 0, "a new read no longer fans out to taxes, and is not degraded: extensions {%s}", keysOf(p.Extensions))

	time.Sleep(time.Until(ur.DeleteAfter) + 300*time.Millisecond)
	d.check(len(d.listModules("t2")) == 0, "after the 5s grace period the rows are deleted")
	d.check(keysOf(d.listModulesByID("t1")) == "saas,taxes", "t1's installation is untouched")
}

// -----------------------------------------------------------------------------
// -register-only

func (d *demo) registerOnly() int {
	code := 0
	for _, r := range []struct{ tenant, url string }{{"t1", taxesURL}, {"t1", saasURL}, {"t2", taxesURL}} {
		status, raw := d.rpc("registry.v1.Registry/RegisterModule", map[string]any{"tenant_id": r.tenant, "base_url": r.url})
		switch {
		case status == 200:
			fmt.Printf("registered %s for %s\n", r.url, r.tenant)
		case strings.Contains(string(raw), "already_exists"):
			fmt.Printf("already installed: %s for %s\n", r.url, r.tenant)
		default:
			fmt.Printf("failed: %s for %s: HTTP %d %s\n", r.url, r.tenant, status, raw)
			code = 1
		}
	}
	return code
}

// -----------------------------------------------------------------------------
// processes

func (d *demo) start(name string, args ...string) error {
	log, err := os.OpenFile(filepath.Join(d.dir, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(filepath.Join(d.bin, name), args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		return fmt.Errorf("start %s: %w (run `make build` first)", name, err)
	}
	p := &proc{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		log.Close()
		close(p.done)
	}()
	d.mu.Lock()
	d.procs[name] = p
	d.mu.Unlock()
	return nil
}

func (d *demo) kill(name string) {
	d.mu.Lock()
	p := d.procs[name]
	delete(d.procs, name)
	d.mu.Unlock()
	if p != nil {
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (d *demo) stopAll() {
	d.mu.Lock()
	procs := d.procs
	d.procs = map[string]*proc{}
	d.mu.Unlock()
	for _, p := range procs {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	for _, p := range procs {
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}
}

func (d *demo) waitHealthy(base string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if res, err := d.http.Get(base + "/healthz"); err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become healthy", base)
}

func (d *demo) logSize(name string) int64 {
	fi, err := os.Stat(filepath.Join(d.dir, name+".log"))
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (d *demo) logSince(name string, offset int64, substr string) []string {
	f, err := os.Open(filepath.Join(d.dir, name+".log"))
	if err != nil {
		return nil
	}
	defer f.Close()
	_, _ = f.Seek(offset, io.SeekStart)
	b, _ := io.ReadAll(f)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// HTTP

type product struct {
	Core       map[string]any            `json:"core"`
	Extensions map[string]map[string]any `json:"extensions"`
	Degraded   []string                  `json:"degraded"`
}

type registration struct {
	ModuleID        string   `json:"moduleId"`
	ExtensionPoint  string   `json:"extensionPoint"`
	State           string   `json:"state"`
	CacheTTLSeconds int      `json:"cacheTtlSeconds"`
	ContextKeys     []string `json:"contextKeys"`
}

type moduleStats struct {
	EnrichCalls    int64 `json:"enrich_calls"`
	EnrichProducts int64 `json:"enrich_products"`
}

func (d *demo) post(url string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	res, err := d.http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func (d *demo) rpc(method string, body any) (int, []byte) {
	return d.post(pimURL+"/"+method, body)
}

func (d *demo) getRaw(url string) []byte {
	res, err := d.http.Get(url)
	if err != nil {
		return []byte(err.Error())
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return bytes.TrimSpace(raw)
}

func (d *demo) getProduct(tenant, id string, ctx map[string]string) (int, product, []byte) {
	status, raw := d.rpc("pim.v1.PIM/GetProduct", map[string]any{"tenant_id": tenant, "id": id, "context": ctx})
	var res struct {
		Product product `json:"product"`
	}
	_ = json.Unmarshal(raw, &res)
	return status, res.Product, raw
}

func (d *demo) listModules(tenant string) []registration {
	_, raw := d.rpc("registry.v1.Registry/ListModules", map[string]any{"tenant_id": tenant})
	var res struct {
		Registrations []registration `json:"registrations"`
	}
	_ = json.Unmarshal(raw, &res)
	return res.Registrations
}

func (d *demo) listModulesByID(tenant string) map[string]any {
	out := map[string]any{}
	for _, r := range d.listModules(tenant) {
		out[r.ModuleID] = true
	}
	return out
}

func (d *demo) stats(base string) moduleStats {
	var s moduleStats
	_ = json.Unmarshal(d.getRaw(base+"/stats"), &s)
	return s
}

func (d *demo) resetStats() {
	d.post(taxesURL+"/stats/reset", nil)
	d.post(saasURL+"/stats/reset", nil)
}

func (d *demo) chaos(mode string) {
	status, _ := d.post(saasURL+"/chaos", map[string]string{"mode": mode})
	note("saas chaos mode -> %s (HTTP %d)", mode, status)
}

// invalidateOnBehalfOf stands in for the module telling PIM its data changed,
// so the next read actually calls it instead of serving a cache hit.
func (d *demo) invalidateOnBehalfOf(module string) {
	status, raw := d.rpc("registry.v1.Registry/InvalidateFragments", map[string]any{"tenant_id": "t1", "module_id": module})
	note("InvalidateFragments(t1, %s) -> HTTP %d %s", module, status, raw)
}

// -----------------------------------------------------------------------------
// output

func title(s string) {
	fmt.Printf("\n== %s\n", s)
}

func (d *demo) begin(criterion int, s string) {
	d.step = fmt.Sprintf("criterion %d", criterion)
	fmt.Printf("\n== Criterion %d: %s\n", criterion, s)
}

func note(format string, args ...any) {
	fmt.Printf("    %s\n", fmt.Sprintf(format, args...))
}

func (d *demo) check(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ok {
		d.passes++
		fmt.Printf("    ✔ %s\n", msg)
		return
	}
	d.failures = append(d.failures, d.step+": "+msg)
	fmt.Printf("    ✘ %s\n", msg)
}

func fatal(err error) int {
	fmt.Fprintln(os.Stderr, "demo:", err)
	return 2
}

func indent(raw []byte, prefix string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, prefix, "  "); err != nil {
		return prefix + string(raw)
	}
	return prefix + buf.String()
}

func trimLog(l string) string {
	if i := strings.Index(l, "level="); i >= 0 {
		l = l[i:]
	}
	if len(l) > 220 {
		l = l[:220] + "…"
	}
	return l
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func keysOf[V any](m map[string]V) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func setString(m map[string]bool) string {
	return keysOf(m)
}

func dig(v any, path ...string) map[string]any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	m, _ := v.(map[string]any)
	return m
}

// jsonDiff lists the paths at which b differs from a: "+" added, "-" removed,
// "~" changed. Objects are compared key by key; anything else as a whole.
func jsonDiff(path string, a, b any, out *[]string) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		if !reflect.DeepEqual(a, b) {
			*out = append(*out, "~ "+path)
		}
		return
	}
	keys := map[string]bool{}
	for k := range am {
		keys[k] = true
	}
	for k := range bm {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		child := k
		if path != "" {
			child = path + "." + k
		}
		av, inA := am[k]
		bv, inB := bm[k]
		switch {
		case !inA:
			*out = append(*out, "+ "+child)
		case !inB:
			*out = append(*out, "- "+child)
		default:
			jsonDiff(child, av, bv, out)
		}
	}
	sort.Strings(*out)
}
