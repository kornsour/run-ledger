.PHONY: build test vet fmt lint cover clean run image

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

clean:
	rm -rf bin coverage.out
