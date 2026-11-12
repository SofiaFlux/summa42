.PHONY: test test-race vet build verify-fast verify

# The default GOCACHE is not writable in every environment (see AGENTS.md).
# A GOCACHE that is already set in the environment wins.
export GOCACHE ?= /tmp/summa42-full-go-cache

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

build:
	go build ./cmd/summa42 ./cmd/summa42-box

# The check to run while working: formatting, vet, build and the Go tests.
verify-fast:
	@unformatted="$$(gofmt -l .)" || exit 1; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: these files need formatting:"; echo "$$unformatted"; exit 1; \
	fi
	go vet ./...
	go build ./cmd/...
	go test ./... -count=1

# The check to run before opening a pull request: verify-fast plus the Python
# tests, the documentation check, the race run and the SQLite spike script.
verify: verify-fast
	python3 -m unittest discover -s scripts -p 'test_*.py'
	git ls-files -z '*.md' | xargs -0 python3 scripts/verify_docs.py
	go test -race ./... -count=1 -timeout 45m
	python3 docs/superpowers/research/spikes/2026-09-14-sqlite-persistence-spike.py
