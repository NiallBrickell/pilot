.PHONY: build start stop dev clean dashboard dashboard-dev dashboard-build replay release

build:
	go build -o pilot .

# Replay real escalated tool calls through the live evaluator (~40 Anthropic
# calls, a few cents). This is the only check that catches a prompt regression:
# the code still compiles and the unit tests still pass while the evaluator
# quietly denies routine work. Deliberately not in CI — it needs a key and costs
# money per run, so it gates releases instead, where a bad prompt would reach
# users. PILOT_REPLAY_REQUIRE_KEY makes a missing key fail rather than skip.
replay:
	PILOT_REPLAY_REQUIRE_KEY=1 go test -tags=replay ./internal/config \
		-run TestReplayAgainstEvaluator -v -count=1 -timeout 10m

# make release VERSION=v0.1.29 — verify, then tag and push so release.yml
# publishes the binaries that `pilot upgrade` pulls.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=v0.1.29"; exit 1; }
	@git diff-index --quiet HEAD -- || { echo "working tree is dirty — commit first"; exit 1; }
	@git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null && { echo "tag $(VERSION) already exists"; exit 1; } || true
	go test ./...
	$(MAKE) replay
	git tag $(VERSION)
	git push origin main
	git push origin $(VERSION)
	@echo "Released $(VERSION) — watch: gh run list --workflow=Release --limit 1"

start: build
	PILOT_SKIP_AUTO_UPGRADE=1 ./pilot start

stop:
	./pilot stop

dev:
	go run . serve

clean:
	rm -f pilot

dashboard: build
	./pilot dashboard

dashboard-dev:
	cd dashboard && wails dev

dashboard-build:
	cd dashboard && wails build
