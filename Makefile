.PHONY: build test vet fmt lint cover clean run image docs notebook

IMAGE ?= run-ledger
TAG ?= dev

build:
	@mkdir -p bin
	go build -o bin/runledger ./cmd/runledger
	go build -o bin/rlctl ./cmd/rlctl

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

run: build
	./bin/runledger

# Builds the same binary as `build`, containerized, for the host platform --
# not the multi-arch image CI publishes on a tag (see .github/workflows/image.yml).
image:
	docker build -t $(IMAGE):$(TAG) .

DOCS_PORT ?= 8123

# Builds the generated half of the Pages site: the executed notebook and the
# Python client reference. Both are produced from source at deploy time and
# are gitignored -- committing them would reintroduce exactly the drift the
# OpenAPI spec test exists to prevent.
#
# Needs the docs tooling: pip install -e './python[docs]'
docs: build notebook
	pdoc runledger -o docs/python

# Executes the notebook against a real, ephemeral ledger and writes the HTML
# into docs/. A notebook that has stopped working fails here rather than
# rotting quietly in git.
notebook: build
	@command -v jupyter >/dev/null 2>&1 || \
		{ echo "jupyter not found: pip install -e './python[docs]'"; exit 1; }
	@./bin/runledger --addr :$(DOCS_PORT) >/tmp/runledger-docs.log 2>&1 & \
		echo $$! > /tmp/runledger-docs.pid; \
		sleep 1; \
		RUNLEDGER_ADDR=http://localhost:$(DOCS_PORT) jupyter nbconvert \
			--to html --execute --output-dir docs \
			--output reproducibility.html \
			python/examples/reproducibility.ipynb; \
		status=$$?; \
		kill $$(cat /tmp/runledger-docs.pid) 2>/dev/null; \
		rm -f /tmp/runledger-docs.pid; \
		exit $$status

clean:
	rm -rf bin coverage.out docs/reproducibility.html docs/python
