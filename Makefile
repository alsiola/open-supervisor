.PHONY: build test vet install snapshot

build:
	go build -o bin/open-supervisor ./cmd/open-supervisor

test:
	go test -race ./...

vet:
	go vet ./...

install:
	go install ./cmd/open-supervisor

# A local release build of every platform into dist/, without publishing.
snapshot:
	goreleaser release --snapshot --clean
