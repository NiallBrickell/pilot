.PHONY: build start stop dev clean dashboard dashboard-dev dashboard-build replay live release

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

# Drive a real Claude Code session (isolated CLAUDE_CONFIG_DIR, scratch
# PILOT_HOME and serve) through the auto-mode classifier with a freshly built
# binary. The replay suite covers Pilot's own layers; this is the only check
# that sees which hooks Claude Code actually fires. Needs `claude` on PATH
# and an Anthropic key; costs a few cents per case.
live:
	PILOT_LIVE_REQUIRE=1 go test -tags=live ./internal/livetest -v -count=1 -timeout 20m

# make release VERSION=v0.1.29 — verify, then tag and push so release.yml
# publishes the binaries that `pilot upgrade` pulls.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=v0.1.29"; exit 1; }
	@git diff-index --quiet HEAD -- || { echo "working tree is dirty — commit first"; exit 1; }
	@git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null && { echo "tag $(VERSION) already exists"; exit 1; } || true
	go test ./...
	$(MAKE) replay
	$(MAKE) live
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
