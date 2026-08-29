.PHONY: build test vet fmt lint cover clean run image docs notebook dashboard

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
	pdoc -t docs/pdoc-template runledger -o docs/python

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
	@# nbconvert warns "Alternative text is missing" and fills in a hardcoded
	@# placeholder that tells a screen-reader user nothing. Its own warning is
	@# therefore expected above; this step is what actually resolves it, and
	@# fails the build if any image lacks a description.
	python3 scripts/apply_alt_text.py \
		python/examples/reproducibility.ipynb docs/reproducibility.html
	@# nbconvert has no dark theme or toggle of its own; this adds the same
	@# light/dark switch docs/index.html and the pdoc output already have.
	python3 scripts/apply_theme_toggle.py docs/reproducibility.html


# Runs the local dashboard (dashboard/README.md) against a ledger you have
# already started -- `make build && ./bin/runledger &`, same as the
# notebook. Not something CI runs or the Pages deploy builds: see
# dashboard/README.md for why this stays a local-only tool.
#
# Needs the dashboard's own dependencies: pip install -e ./python -e ./dashboard
dashboard:
	@command -v marimo >/dev/null 2>&1 || \
		{ echo "marimo not found: pip install -e ./python -e ./dashboard"; exit 1; }
	marimo run dashboard/app.py

clean:
	rm -rf bin coverage.out docs/reproducibility.html docs/python
