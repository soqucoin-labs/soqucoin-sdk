# Soqucoin SDK developer targets.

.PHONY: test race fuzz integration docs

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
# withdrawal flows, six scenarios, about 30 seconds. Needs a soqucoind build
# (v2.3.0 or later) on PATH or in SOQUCOIND.
SOQUCOIND ?= $(shell command -v soqucoind 2>/dev/null)
integration:
	@test -n "$(SOQUCOIND)" || { echo "set SOQUCOIND=/path/to/soqucoind (regtest-capable, v2.3.0+)"; exit 1; }
	go test -tags integration ./integration -soqucoind "$(SOQUCOIND)" -count=1 -v

docs:
	python3 scripts/check-docs.py
