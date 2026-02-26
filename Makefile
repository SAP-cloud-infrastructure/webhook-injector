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

.PHONY: test-integration
test-integration:
	KUBEBUILDER_ASSETS="$(shell setup-envtest use -p path 1.29.x)" go test -v -tags=integration ./test/integration/...

.PHONY: docker-build
docker-build:
	docker build -t webhook-injector:latest .

.PHONY: docker-push
docker-push: docker-build
	docker tag webhook-injector:latest $(REGISTRY)/webhook-injector:$(VERSION)
	docker push $(REGISTRY)/webhook-injector:$(VERSION)
