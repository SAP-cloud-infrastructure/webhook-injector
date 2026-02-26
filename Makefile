.PHONY: build
build:
	go build -o bin/webhook-injector ./cmd/webhook-injector

.PHONY: run
run:
	go run ./cmd/webhook-injector/main.go

.PHONY: test
test:
	go test -v ./...

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...
