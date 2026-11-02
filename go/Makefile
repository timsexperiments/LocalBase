# Reliability tools; docs in docs/reliability.md. Fake backends only, ports in
# 29000-29499, and each run stops itself.
.PHONY: soak soak-smoke soak-race

soak: ## 30 minutes against fake backends; writes a result table
	tools/soak.sh -duration 30m

soak-smoke: ## One cycle, about 2 minutes
	tools/soak.sh -smoke

soak-race: ## One cycle against a race-detector gateway
	tools/soak.sh -smoke -race
