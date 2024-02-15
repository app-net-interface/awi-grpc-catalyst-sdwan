SHELL := /bin/sh

APP_NAME ?= awi-grpc-catalyst-sdwan
BIN_NAME ?= ${APP_NAME}
NAMESPACE ?= awi-system
CLUSTER_NAME ?= awi
#
# Image Info
#
COMMIT ?= $(shell git rev-parse --verify --short=8 HEAD)
IMAGE_TAG ?= ${COMMIT}
IMAGE_REGISTRY ?= 229451923406.dkr.ecr.us-west-2.amazonaws.com
IMAGE_NAME ?= ${IMAGE_REGISTRY}/awi/awi-grpc-catalyst-sdwan
SSH_PRIVATE_KEY_LOCATION ?= ~/.ssh/id_rsa

OS ?= $(if $(GOOS),$(GOOS),$(shell go env GOOS))
ARCH ?= $(if $(GOARCH),$(GOARCH),$(shell go env GOARCH))

.PHONY: all
all: build

# help credits: https://github.com/kubernetes-sigs/kubebuilder/blob/master/Makefile

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

%.image.exists:
	@docker inspect $* >/dev/null 2>&1 || \
		$(MAKE) docker-image

.PHONY: tools
tools:
	go install golang.org/x/tools/cmd/goimports@latest
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b "$$(go env GOPATH)"/bin v1.51.0

.PHONY: run
run: ## Run awi-grpc-catalyst-sdwan server
ifndef VMANAGE_USERNAME
	@echo "Warning: VMANAGE_USERNAME is not set"
endif
ifndef VMANAGE_PASSWORD
	@echo "Warning: VMANAGE_PASSWORD is not set"
endif
	go run main.go

.PHONY: proxy
proxy: ## Run envoy proxy in Linux (required for grpc-web connectivity) (might not work in WSL)
	docker run -d -v "$(pwd)"/proxy/envoy-config-linux.yaml:/etc/envoy/envoy.yaml:ro \
        --network=host envoyproxy/envoy:v1.22.0

.PHONY: proxy-windows
proxy-windows: ## Run envoy proxy in Windows (might not work in WSL)
	cd proxy && docker build -t awi-envoy-proxy:v1 .
	docker run -p 8080:8080 -p 9901:9901 awi-envoy-proxy:v1

.PHONY: proxy-local
proxy-local: ## Run envoy proxy without container (recommended for WSL)
	envoy -c proxy/envoy-config-linux.yaml

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	golangci-lint run --fix

.PHONY: lint
lint: ## Run golangci-lint linter
	golangci-lint run

.PHONY: build
build: ## Build binary
	CGO_ENABLED=0 GOOS=${OS} GOARCH=${ARCH} go build -ldflags="-w -s" -o bin/${BIN_NAME}

.PHONY: unit-test
unit-test: ## Run unit tests
	go test ./...

.PHONY: race-unit-test
race-unit-test: ## Run unit tests with race flag to detect races
	go test -race ./...

.PHONY: test-fmt
test-fmt: ## Test fmt and imports formatting
	test -z $$(goimports -w -l pkg)

.PHONY: test
test: test-fmt lint race-unit-test ## Run fmt, linters and unit tests

.PHONY: docker-image
docker-image: ## Build docker image
	OS=linux ARCH=amd64 BIN_NAME=${BIN_NAME}_${OS}_${ARCH} $(MAKE) build
	docker build  --build-arg SSH_PRIVATE_KEY="$(cat ${SSH_PRIVATE_KEY_LOCATION})" -t ${IMAGE_NAME}:${IMAGE_TAG} .

.PHONY: run-container
run-container: ${IMAGE_NAME}\:${IMAGE_TAG}.image.exists ## run application in container
	docker run --rm -it --volume $(realpath .)/awi.db:/app/awi.db --volume ~/.aws/:/root/.aws \
 	--volume $(realpath .)/config.yaml:/app/config.yaml \
 	--env VMANAGE_USERNAME='$(VMANAGE_USERNAME)' --env VMANAGE_PASSWORD='$(VMANAGE_PASSWORD)' \
 	--name awi-grpc-catalyst-sdwan \
 	${IMAGE_NAME}:${IMAGE_TAG}

.PHONY: publish
publish: docker-image ## Push docker image
	docker push ${IMAGE_NAME}:${IMAGE_TAG};
