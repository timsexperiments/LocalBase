# Reliability tools; docs in docs/reliability.md. Fake backends only, ports in
# 29000-29499, and each run stops itself.
.PHONY: soak soak-smoke soak-race bench bench-compare

soak: ## 30 minutes against fake backends; writes a result table
	tools/soak.sh -duration 30m

soak-smoke: ## One cycle, about 2 minutes
	tools/soak.sh -smoke

soak-race: ## One cycle against a race-detector gateway
	tools/soak.sh -smoke -race

bench: ## What the gateway itself costs, against fake backends; about 2 minutes
	tools/bench.sh

bench-compare: ## The same, then compare with the recorded baseline for this platform
	tools/bench.sh -compare tools/bench/baseline-$$(go env GOOS)-$$(go env GOARCH).json
