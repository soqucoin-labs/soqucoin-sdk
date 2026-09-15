# Soqucoin SDK developer targets.

.PHONY: test race fuzz integration docs lint mutants branch gates

test:
	go vet ./... && go test ./...

race:
	go test -race ./...

# Short fuzz campaigns; lengthen -fuzztime for a real run.
fuzz:
	go test ./address -run '^$$' -fuzz FuzzDecode -fuzztime 30s
	go test ./address -run '^$$' -fuzz FuzzEncode -fuzztime 30s
	go test ./tx -run '^$$' -fuzz FuzzWeightMatchesSerialization -fuzztime 30s
	go test ./tx -run '^$$' -fuzz FuzzBuildSendNeverPanicsAndNeverOverpays -fuzztime 30s

# The self-serve integration harness: a throwaway regtest node, real deposit and
# withdrawal flows, nine scenarios, about 45 seconds. Needs a soqucoind build
# (v2.3.0 or later) on PATH or in SOQUCOIND.
SOQUCOIND ?= $(shell command -v soqucoind 2>/dev/null)
integration:
	@test -n "$(SOQUCOIND)" || { echo "set SOQUCOIND=/path/to/soqucoind (regtest-capable, v2.3.0+)"; exit 1; }
	go test -tags integration ./integration -soqucoind "$(SOQUCOIND)" -count=1 -v

docs:
	python3 scripts/check-docs.py

# The static-analysis floor. Pinned; a ratchet against origin/main, so the
# existing tree is the baseline and new code is held to the bar (.golangci.yml).
GOLANGCI ?= github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
lint:
	go run $(GOLANGCI) run ./...

# Every declared mutant must be caught by the one test that pins it. A test
# that cannot fail is not coverage.
mutants:
	python3 scripts/check-mutants.py

# The branch is current with main and the change is one mechanism.
branch:
	python3 scripts/check-branch.py

# What a pull request passes before it opens. The integration harness is not
# here because it needs a soqucoind build; run `make integration` as well.
gates: test race docs lint mutants branch
