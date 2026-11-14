# Reliability tools; docs in docs/reliability.md. Fake backends only, ports in
# 29000-29499, and each run stops itself.
.PHONY: soak soak-smoke soak-race chaos chaos-all known-bugs bench bench-compare

soak: ## 30 minutes against fake backends; writes a result table
	scripts/soak.sh -duration 30m

soak-smoke: ## One cycle, about 2 minutes
	scripts/soak.sh -smoke

soak-race: ## One cycle against a race-detector gateway
	scripts/soak.sh -smoke -race

chaos: ## Break a real gateway on purpose; skips tests naming an open bug
	@bash -c '. scripts/disk-guard.sh && disk_guard chaos && exec env GOMAXPROCS=2 nice -n 10 go test ./test/chaos -count=1 -timeout 10m'

chaos-all: ## The same, including the tests of open bugs
	@bash -c '. scripts/disk-guard.sh && disk_guard chaos-all && exec env GOMAXPROCS=2 LOCALBASE_CHAOS_ALL=1 nice -n 10 go test ./test/chaos -count=1 -timeout 10m'

known-bugs: ## Fail if a chaos skip names a merged pull request, or none
	scripts/known-bugs-check.sh

bench: ## What the gateway itself costs, against fake backends; about 2 minutes
	scripts/bench.sh

bench-compare: ## The same, then compare with the recorded baseline for this platform
	scripts/bench.sh -compare tools/bench/baseline-$$(go env GOOS)-$$(go env GOARCH).json

.PHONY: e2e
e2e: ## The browser suite in headless Chrome, against fake backends; about a minute
	test/browser/e2e.sh

.PHONY: e2e-manifest-check
e2e-manifest-check: ## Show the browser suite's table of what ran fails when a section does not run, and a broken run exits 5; about two minutes
	test/browser/e2e-manifest-check.sh

.PHONY: e2e-kill-check
e2e-kill-check: ## Show the browser suite leaves nothing running however it ends; about a minute
	test/browser/e2e-kill-check.sh
