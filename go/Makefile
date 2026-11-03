# Reliability tools; docs in docs/reliability.md. Fake backends only, ports in
# 29000-29499, and each run stops itself.
.PHONY: soak soak-smoke soak-race chaos chaos-all known-bugs bench bench-compare

soak: ## 30 minutes against fake backends; writes a result table
	tools/soak.sh -duration 30m

soak-smoke: ## One cycle, about 2 minutes
	tools/soak.sh -smoke

soak-race: ## One cycle against a race-detector gateway
	tools/soak.sh -smoke -race

chaos: ## Break a real gateway on purpose; skips tests naming an open bug
	@bash -c '. tools/disk-guard.sh && disk_guard chaos && exec env GOMAXPROCS=2 nice -n 10 go test ./tools/chaos -count=1 -timeout 10m'

chaos-all: ## The same, including the tests of open bugs
	@bash -c '. tools/disk-guard.sh && disk_guard chaos-all && exec env GOMAXPROCS=2 LOCALBASE_CHAOS_ALL=1 nice -n 10 go test ./tools/chaos -count=1 -timeout 10m'

known-bugs: ## Fail if a chaos skip names a merged pull request, or none
	tools/known-bugs-check.sh

bench: ## What the gateway itself costs, against fake backends; about 2 minutes
	tools/bench.sh

bench-compare: ## The same, then compare with the recorded baseline for this platform
	tools/bench.sh -compare tools/bench/baseline-$$(go env GOOS)-$$(go env GOARCH).json
