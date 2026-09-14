SHELL := /bin/bash
.DEFAULT_GOAL := help

BUF  ?= buf
BINS := pim module-taxes module-saas demo

.PHONY: proto
proto: ## Lint the protos and generate Go into gen/
	$(BUF) lint
	$(BUF) generate

.PHONY: build
build: proto ## Build every binary into bin/
	@mkdir -p bin
	@for b in $(BINS); do go build -o bin/$$b ./cmd/$$b || exit 1; done
	@echo "built: $(BINS)"

.PHONY: test
test: proto ## go vet + tests, including the no-module-imports boundary check
	go vet ./...
	go test ./...

.PHONY: dev
dev: build ## Run PIM and both modules, output prefixed per service
	@mkdir -p data
	@# Process substitution rather than a pipe, so $$! is the service, not awk,
	@# and the trap stops exactly these three rather than the whole process group
	@# (which, when make is run from a script, includes the script).
	@pids=""; \
	trap 'kill $$pids 2>/dev/null; wait' INT TERM EXIT; \
	./bin/pim -db data/pim.db > >(awk '{print "[pim]   " $$0; fflush()}') 2>&1 & pids="$$pids $$!"; \
	./bin/module-taxes        > >(awk '{print "[taxes] " $$0; fflush()}') 2>&1 & pids="$$pids $$!"; \
	./bin/module-saas         > >(awk '{print "[saas]  " $$0; fflush()}') 2>&1 & pids="$$pids $$!"; \
	wait

.PHONY: register
register: ## Against `make dev`: install taxes+saas for t1 and taxes for t2
	./bin/demo -register-only

.PHONY: demo
demo: build ## Start all three services and run the acceptance walkthrough
	./bin/demo

.PHONY: clean
clean: ## Remove binaries, generated code and local databases
	rm -rf bin gen data

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
