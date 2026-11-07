# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
.PHONY: help bootstrap build run test race contract ui lint fmt check hooks rehearse docs soak soak-smoke soak-race

PORT ?= 26400

# Tools from `make bootstrap` come first.
export PATH := $(CURDIR)/.cache/tools/bin:$(PATH)

need_go_makefile = [ -f go/Makefile ] || { echo "This target needs go/Makefile and go/tools/soak, which come from PR \#468 (branch rewrite/go-rel-soak). They aren't in this tree yet."; exit 1; }

help: ## List the commands
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/' | expand -t 12

bootstrap: ## Install the pinned tools into .cache/tools, verified by checksum
	scripts/bootstrap.sh

build: ## Build go/local-base, as the release builds it
	go/tools/build.sh go/local-base

run: build ## Run a gateway on a throwaway root in .scratch (PORT=26400, runtimes on 26410-26414)
	mkdir -p .scratch
	go/local-base --root "$(CURDIR)/.scratch/root" --non-interactive serve --host 127.0.0.1 --port $(PORT) \
	  --llm-port 26410 --stt-port 26411 --image-port 26412 --video-port 26413 --tts-port 26414

test: ## Unit tests; needs no models or GPU
	cd go && go test ./internal/... ./cmd/...

race: ## Unit tests with the race detector, as CI runs them
	cd go && go test -race ./internal/... ./cmd/...

contract: ## HTTP contract suite against the Go gateway
	cd go && tools/contract.sh -args -impl=go

ui: ## Compiled CSS is current, and the browser JavaScript tests pass (needs Bun)
	bun install --frozen-lockfile
	cd go && tools/tailwind.sh --check
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

soak: ## 30 minutes against fake backends. Needs go/Makefile
	@$(call need_go_makefile)
	$(MAKE) -C go soak

soak-smoke: ## One soak cycle, about 2 minutes. Needs go/Makefile
	@$(call need_go_makefile)
	$(MAKE) -C go soak-smoke

soak-race: ## One soak cycle with the race detector. Needs go/Makefile
	@$(call need_go_makefile)
	$(MAKE) -C go soak-race
