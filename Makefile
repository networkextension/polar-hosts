.PHONY: test build vet fmt tidy ci darwin-arm64

test:
	CGO_ENABLED=0 go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

build:
	CGO_ENABLED=0 go build ./...

darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o bin/hosts-svc ./cmd/hosts-svc

ci: vet test build
