LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN): ## Location to install dependencies into.
	mkdir -p $(LOCALBIN)

GOFUMPT ?= $(LOCALBIN)/gofumpt
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

.PHONY: gofumpt
gofumpt: $(GOFUMPT) ## Download gofumpt if necessary.
$(GOFUMPT): $(LOCALBIN)
	test -s $(LOCALBIN)/gofumpt || GOBIN=$(LOCALBIN) go install mvdan.cc/gofumpt@latest

.PHONY: fmt
fmt: gofumpt ## Run go mod tidy and gofumpt against the code.
	go mod tidy -compat=1.24
	$(GOFUMPT) -w -extra .

.PHONY: lint
lint: ## Download golangci-lint if necessary and run it against the code.
	test -s $(GOLANGCI_LINT) || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(LOCALBIN)
	CGO_ENABLED=0 $(GOLANGCI_LINT) run --timeout 5m

.PHONY: build
build: ## Build the node-event-listener binary.
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -tags timetzdata -tags=nomsgpack -o $(LOCALBIN)/node-event-listener-goroutines .
	#GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o $(LOCALBIN)/node-event-listener .
	#go build -o $(LOCALBIN)/node-event-listener .

.PHONY: vendor
vendor: ## Run go mod vendor and vendor dependencies.
	go mod vendor
