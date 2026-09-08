# doc-link — decoupled document linking POC
#
# Two ways to run it:
#   make dev       everything on the host against a podman Postgres  (fast loop)
#   make cluster   kind + CNPG + ingress, one origin per component   (the real thing)

SHELL := /bin/bash
.DEFAULT_GOAL := help

PG_PORT      ?= 5433
KIND_CLUSTER ?= doclink
NAMESPACE    ?= doclink
GO_SERVICES  := doclink pim subscriptions shipping
WEB_APPS     := pim-web subscriptions-web shipping-web
TAG          ?= dev

dsn = postgres://$(1):doclink-dev@localhost:$(PG_PORT)/doclink?sslmode=disable

## ---------------------------------------------------------------- codegen ---

.PHONY: generate
generate: ## Regenerate Go and TypeScript from the protos
	GOBIN=$(PWD)/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	GOBIN=$(PWD)/bin go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
	PATH="$(PWD)/bin:$$PATH" buf generate
	buf generate --template buf.gen.web.yaml

.PHONY: lint
lint: ## buf lint + go vet + gofmt check
	buf lint
	go vet ./...
	@test -z "$$(gofmt -l cmd internal services bench)" || \
		(echo "gofmt needed:"; gofmt -l cmd internal services bench; exit 1)

.PHONY: test
test: ## Run the Go tests
	go test ./...

.PHONY: verify
verify: ## Assert the architectural rules (run this in CI)
	@./scripts/verify-decoupling.sh

## ------------------------------------------------------------ local (dev) ---

.PHONY: pg
pg: ## Start the local Postgres (podman) on $(PG_PORT)
	-podman rm -f doclink-pg 2>/dev/null
	podman run -d --name doclink-pg \
		-e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=doclink \
		-p $(PG_PORT):5432 docker.io/library/postgres:17-alpine
	@echo "waiting for postgres…"
	@for i in $$(seq 1 40); do \
		podman exec doclink-pg pg_isready -U postgres >/dev/null 2>&1 && break; sleep 1; done
	MIGRATION_DSN="postgres://postgres:postgres@localhost:$(PG_PORT)/doclink?sslmode=disable" \
		go run ./cmd/migrate

.PHONY: seed
seed: ## Seed all three representations with identical data
	BENCH_DSN="$(call dsn,svc_bench)" go run ./cmd/seed -items $${ITEMS:-1000}

.PHONY: build
build: ## Build every Go binary into bin/
	@mkdir -p bin
	@for s in $(GO_SERVICES); do go build -o bin/$$s ./services/$$s; done
	go build -o bin/migrate ./cmd/migrate
	go build -o bin/seed ./cmd/seed
	go build -o bin/bench ./bench
	@echo "built: $$(ls bin | tr '\n' ' ')"

.PHONY: dev
dev: build ## Run all four services and all three frontends locally
	@./scripts/dev.sh

.PHONY: dev-stop
dev-stop: ## Stop everything started by `make dev`
	-pkill -f 'bin/(doclink|pim|subscriptions|shipping)' 2>/dev/null
	-pkill -f 'vite' 2>/dev/null
	@echo "stopped"

.PHONY: bench
bench: ## Run the strategy comparison against the local stack
	BENCH_DSN="$(call dsn,svc_bench)" ./bin/bench \
		-duration $${DURATION:-5s} -satellites $${SATS:-0,2,4,8,16}

# SATS defaults to 0 here, unlike `make bench`. Synthetic satellites listen on
# 127.0.0.1 in the harness process, which a registry inside the cluster cannot
# reach; the harness now detects that and refuses rather than reporting a wrong
# number. Sweeping satellite count is a local-stack exercise.
.PHONY: bench-cluster
bench-cluster: ## Real-satellite measurement against the kind cluster
	@echo "port-forwarding doclink-pg-rw:5432 -> localhost:15432"
	@kubectl --context kind-$(KIND_CLUSTER) -n $(NAMESPACE) port-forward svc/doclink-pg-rw 15432:5432 & \
	 PF=$$!; sleep 3; \
	 PW=$$(kubectl --context kind-$(KIND_CLUSTER) -n $(NAMESPACE) get secret service-roles \
	       -o jsonpath='{.data.password}' | base64 -d); \
	 PGPASSWORD="$$PW" BENCH_DSN="postgres://svc_bench@localhost:15432/doclink?sslmode=disable" \
	   ./bin/bench -registry http://doclink-api.doclink.localhost:18080 \
	     -duration $${DURATION:-5s} -satellites $${SATS:-0} \
	     -out bench/results/cluster.json; \
	 kill $$PF

.PHONY: bench-refs
bench-refs: ## Same sweep with refs_only, which is where INDEXED actually wins
	BENCH_DSN="$(call dsn,svc_bench)" ./bin/bench \
		-duration $${DURATION:-5s} -satellites $${SATS:-0,4,16} \
		-refs-only -out bench/results/refs-only.json

## ------------------------------------------------------------------ kind ----

.PHONY: images
images: ## Build all seven container images with podman
	@for s in $(GO_SERVICES); do \
		echo "==> doclink/$$s:$(TAG)"; \
		podman build -f Containerfile.backend --build-arg SERVICE=services/$$s \
			-t doclink/$$s:$(TAG) . || exit 1; done
	@for c in migrate seed; do \
		echo "==> doclink/$$c:$(TAG)"; \
		podman build -f Containerfile.backend --build-arg SERVICE=cmd/$$c \
			-t doclink/$$c:$(TAG) . || exit 1; done
	@for a in $(WEB_APPS); do \
		echo "==> doclink/$$a:$(TAG)"; \
		podman build -f Containerfile.frontend --build-arg APP=$$a \
			-t doclink/$$a:$(TAG) . || exit 1; done

.PHONY: kind-up
kind-up: ## Create the kind cluster and install ingress + CNPG
	kind create cluster --config deploy/kind/cluster.yaml
	kubectl --context kind-$(KIND_CLUSTER) apply -f \
		https://raw.githubusercontent.com/kubernetes/ingress-nginx/main/deploy/static/provider/kind/deploy.yaml
	kubectl --context kind-$(KIND_CLUSTER) apply --server-side -f \
		https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-1.25/releases/cnpg-1.25.0.yaml
	kubectl --context kind-$(KIND_CLUSTER) -n ingress-nginx wait --for=condition=Ready pod \
		-l app.kubernetes.io/component=controller --timeout=300s
	kubectl --context kind-$(KIND_CLUSTER) -n cnpg-system wait --for=condition=Available deploy/cnpg-controller-manager --timeout=300s

.PHONY: load
load: ## Push the locally built images into the kind nodes
	@mkdir -p .images
	@for i in $(GO_SERVICES) migrate seed $(WEB_APPS); do \
		echo "==> loading doclink/$$i:$(TAG)"; \
		podman save -o .images/$$i.tar localhost/doclink/$$i:$(TAG) && \
		kind load image-archive .images/$$i.tar --name $(KIND_CLUSTER) || exit 1; done
	@rm -rf .images

.PHONY: deploy
deploy: ## Apply the manifests and wait for everything to come up
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/00-namespace.yaml
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/01-secrets.yaml
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/cnpg/cluster.yaml
	kubectl --context kind-$(KIND_CLUSTER) -n $(NAMESPACE) wait --for=condition=Ready cluster/doclink-pg --timeout=600s
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/02-migrate-job.yaml
	kubectl --context kind-$(KIND_CLUSTER) -n $(NAMESPACE) wait --for=condition=Complete job/doclink-migrate --timeout=300s
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/03-services.yaml
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/04-web.yaml
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/05-ingress.yaml
	kubectl --context kind-$(KIND_CLUSTER) -n $(NAMESPACE) rollout status deploy/doclink --timeout=180s
	kubectl --context kind-$(KIND_CLUSTER) -n $(NAMESPACE) rollout status deploy/pim --timeout=180s
	kubectl --context kind-$(KIND_CLUSTER) apply -f deploy/manifests/06-seed-job.yaml
	@echo
	@echo "  open http://pim.doclink.localhost:18080/items"

.PHONY: cluster
cluster: images kind-up load deploy ## Full cluster bring-up from nothing

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: clean
clean: dev-stop ## Stop local processes and remove the local Postgres
	-podman rm -f doclink-pg 2>/dev/null
	rm -rf bin .images

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
