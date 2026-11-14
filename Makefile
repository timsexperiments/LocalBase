# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
.PHONY: help bootstrap build run test race contract ui lint fmt check hooks rehearse docs \
	soak soak-smoke soak-race chaos chaos-all known-bugs bench bench-compare e2e e2e-manifest-check e2e-kill-check

PORT ?= 26400

# Tools from `make bootstrap` come first.
export PATH := $(CURDIR)/.cache/tools/bin:$(PATH)

help: ## List the commands
	@grep -E '^[a-z0-9-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/' | expand -t 20

bootstrap: ## Install the pinned tools into .cache/tools, verified by checksum
	scripts/bootstrap.sh

build: ## Build go/local-base, as the release builds it
	go/scripts/build.sh go/local-base

run: build ## Run a gateway on a throwaway root in .scratch (PORT=26400, runtimes on 26410-26414)
	mkdir -p .scratch
	go/local-base --root "$(CURDIR)/.scratch/root" --non-interactive serve --host 127.0.0.1 --port $(PORT) \
	  --llm-port 26410 --stt-port 26411 --image-port 26412 --video-port 26413 --tts-port 26414

test: ## Unit tests; needs no models or GPU
	cd go && go test ./internal/... ./cmd/...

race: ## Unit tests with the race detector, as CI runs them
	cd go && go test -race ./internal/... ./cmd/...

contract: ## HTTP contract suite against the Go gateway
	cd go && scripts/contract.sh -args -impl=go

ui: ## Compiled CSS is current, and the browser JavaScript tests pass (needs Bun)
	bun install --frozen-lockfile
	cd go && scripts/tailwind.sh --check
	bun test go/tools/jstest

lint: ## golangci-lint, as CI runs it. Needs make bootstrap
	cd go && golangci-lint run --config=.golangci.yml

fmt: ## Format Go code
	cd go && "$$(go env GOROOT)/bin/gofmt" -w .

check: ## What CI checks first: format, vet, repo guards and unit tests
	@cd go && unformatted="$$("$$(go env GOROOT)/bin/gofmt" -l .)" && { [ -z "$$unformatted" ] || { echo "run make fmt:"; echo "$$unformatted"; exit 1; }; }
	cd go && go vet ./...
	scripts/check-repo-hygiene.sh
	$(MAKE) test

hooks: ## Optional: format and repo guard checks before each commit
	git config core.hooksPath .githooks

rehearse: ## Rehearse a release from local artifacts; publishes nothing. Needs make bootstrap
	scripts/release/rehearse.sh

docs: ## Build the documentation site into .cache/docs-site/book. Needs make bootstrap
	scripts/docs-site.sh

# Reliability tools (go/docs/reliability.md, go/docs/performance.md): fake backends only, ports in
# 29000-29499, and each run stops itself.

soak: ## 30 minutes against fake backends; writes a result table
	cd go && scripts/soak.sh -duration 30m

soak-smoke: ## One soak cycle, about 2 minutes
	cd go && scripts/soak.sh -smoke

soak-race: ## One soak cycle against a race-detector gateway
	cd go && scripts/soak.sh -smoke -race

chaos: ## Break a real gateway on purpose; skips tests naming an open bug
	@cd go && bash -c '. scripts/disk-guard.sh && disk_guard chaos && exec env GOMAXPROCS=2 nice -n 10 go test ./test/chaos -count=1 -timeout 10m'

chaos-all: ## The same, including the tests of open bugs
	@cd go && bash -c '. scripts/disk-guard.sh && disk_guard chaos-all && exec env GOMAXPROCS=2 LOCALBASE_CHAOS_ALL=1 nice -n 10 go test ./test/chaos -count=1 -timeout 10m'

known-bugs: ## Fail if a chaos skip names a merged pull request, or none
	cd go && scripts/known-bugs-check.sh

bench: ## What the gateway itself costs, against fake backends; about 2 minutes
	cd go && scripts/bench.sh

bench-compare: ## The same, then compare with the recorded baseline for this platform
	cd go && scripts/bench.sh -compare tools/bench/baseline-$$(go env GOOS)-$$(go env GOARCH).json

e2e: ## The browser suite in headless Chrome, against fake backends; about a minute
	cd go && test/browser/e2e.sh

e2e-manifest-check: ## The browser suite's table fails when a section doesn't run, and a broken run exits 5; about two minutes
	cd go && test/browser/e2e-manifest-check.sh

e2e-kill-check: ## The browser suite leaves nothing running however it ends; about a minute
	cd go && test/browser/e2e-kill-check.sh
