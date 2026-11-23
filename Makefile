VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
KIND    ?= kind
GO_FILES = $(shell git ls-files '*.go')

.DEFAULT_GOAL := help
.PHONY: help build run test check fmt accuracy kind-test kind-down clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## build ./kwhy (static, version from git)
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=$(VERSION)" -o kwhy ./cmd/kwhy

run: ## run kwhy from source: make run ARGS="pod deploy/api -n shop"
	go run ./cmd/kwhy $(ARGS)

test: ## unit tests with the race detector
	go test -race ./...

check: ## what CI checks: gofmt, vet (all build tags), race tests
	@test -z "$$(gofmt -l $(GO_FILES))" || (gofmt -l $(GO_FILES) && exit 1)
	go vet ./...
	go vet -tags kind ./change/ ./pod/
	go vet -tags accuracy ./change/ ./value/
	go test -race ./...

fmt: ## gofmt every tracked Go file
	gofmt -w $(GO_FILES)

accuracy: ## public-chart and overlay accuracy (overlays need scripts/fetch-overlays.sh first)
	go test -tags accuracy -timeout 110m ./value/ ./change/

kind-test: ## kind suites for change and pod; creates the clusters if missing (switches your kube context)
	$(KIND) get clusters | grep -qx kwhy || $(KIND) create cluster --name kwhy --wait 120s
	$(KIND) get clusters | grep -qx kwhy-pod || $(KIND) create cluster --name kwhy-pod --config testdata/pod/kind.yaml --wait 180s
	go test -tags kind -run TestKind -p 1 -timeout 45m ./change/ ./pod/

kind-down: ## delete the kind test clusters
	$(KIND) delete cluster --name kwhy
	$(KIND) delete cluster --name kwhy-pod

clean: ## remove the built binary
	rm -f kwhy
