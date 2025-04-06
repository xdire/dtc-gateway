.PHONY: all build clean test gen-certs run-local load-config

# Binary name
BINARY=dtc-gateway

# Build directory
BUILD_DIR=./bin

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOGET=$(GOCMD) get

# Source files
SRC=$(shell find . -type f -name "*.go")

# Version flags
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildTime=$(BUILD_TIME)"

# Config file path
CONFIG_FILE ?= config.yaml

# Certificate generation
CERT_DIR=./certs
CERT_BITS=4096
CERT_DAYS=1825  # 5 years default

all: clean test build

# Build the application
build: $(SRC)
	@echo "Building $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) $(LDFLAGS) -o $(BINARY) .

# Run tests
test:
	@echo "Running tests..."
	$(GOTEST) -v ./...

# Clean up
clean:
	@echo "Cleaning..."
	$(GOCLEAN)
	@rm -rf $(BUILD_DIR) $(BINARY)

# Update dependencies
deps:
	@echo "Updating dependencies..."
	$(GOMOD) tidy

# Install dependencies
install-deps:
	@echo "Installing dependencies..."
	$(GOMOD) download

# Generate protobuf files
generate-proto:
	@echo "Generating protobuf files..."
	protoc --go_out=. --go-grpc_out=. gol/gateway/gateway.proto

# Install the binary
install: build
	@echo "Installing $(BINARY)..."
	cp $(BINARY) /usr/local/bin/

# Load configuration from YAML
load-config:
	@echo "Loading configuration from $(CONFIG_FILE)..."
	$(eval PUBLIC_HOST=$(shell yq '.gateway.public_host' $(CONFIG_FILE) 2>/dev/null || echo "0.0.0.0"))
	$(eval PUBLIC_PORT=$(shell yq '.gateway.public_port' $(CONFIG_FILE) 2>/dev/null || echo "50053"))
	$(eval CONTROL_HOST=$(shell yq '.gateway.control_host' $(CONFIG_FILE) 2>/dev/null || echo "0.0.0.0"))
	$(eval CONTROL_PORT=$(shell yq '.gateway.control_port' $(CONFIG_FILE) 2>/dev/null || echo "50054"))
	$(eval TUNNEL_PORT_BASE=$(shell yq '.gateway.tunnel_port_base' $(CONFIG_FILE) 2>/dev/null || echo "60000"))
	$(eval TLS_ENABLED=$(shell yq '.gateway.tls.enabled' $(CONFIG_FILE) 2>/dev/null || echo "false"))
	$(eval TLS_CERT=$(shell yq '.gateway.tls.cert_path' $(CONFIG_FILE) 2>/dev/null || echo "./certs/server.crt"))
	$(eval TLS_KEY=$(shell yq '.gateway.tls.key_path' $(CONFIG_FILE) 2>/dev/null || echo "./certs/server.key"))
	$(eval CERT_YEARS=$(shell yq '.gateway.tls.longevity_years' $(CONFIG_FILE) 2>/dev/null || echo "5"))
	$(eval CERT_DAYS=$(shell echo $(($(CERT_YEARS) * 365))))
	$(eval HTTP_ENABLED=$(shell yq '.gateway.http_cert_server.enabled' $(CONFIG_FILE) 2>/dev/null || echo "true"))
	$(eval HTTP_PORT=$(shell yq '.gateway.http_cert_server.port' $(CONFIG_FILE) 2>/dev/null || echo "8443"))
	$(eval LOG_LEVEL=$(shell yq '.gateway.log_level' $(CONFIG_FILE) 2>/dev/null || echo "info"))
	$(eval AUTH_TOKEN=$(shell yq '.gateway.auth_token' $(CONFIG_FILE) 2>/dev/null || echo ""))
	@if [ -z "$(AUTH_TOKEN)" ]; then \
		AUTH_TOKEN=$$(openssl rand -hex 32); \
		echo "Generated AUTH_TOKEN: $$AUTH_TOKEN"; \
	fi

# Generate TLS certificates
gen-certs:
	@echo "Generating TLS certificates with $(CERT_DAYS) days validity..."
	@mkdir -p $(CERT_DIR)
	openssl req -x509 -nodes -newkey rsa:$(CERT_BITS) -keyout $(CERT_DIR)/server.key -out $(CERT_DIR)/server.crt -days $(CERT_DAYS) -subj "/CN=dtc-gateway/O=DTC Chat/C=US"
	@echo "Certificates generated at $(CERT_DIR)/server.crt and $(CERT_DIR)/server.key"

# Run locally with configuration
run-local: load-config
	@echo "Running gateway locally with configuration from $(CONFIG_FILE)..."
	@if [ "$(TLS_ENABLED)" = "true" ]; then \
		if [ ! -f "$(TLS_CERT)" ] || [ ! -f "$(TLS_KEY)" ]; then \
			make gen-certs CERT_DAYS=$(CERT_DAYS); \
		fi; \
		$(GOCMD) run main.go \
			--public-host=$(PUBLIC_HOST) \
			--public-port=$(PUBLIC_PORT) \
			--control-host=$(CONTROL_HOST) \
			--control-port=$(CONTROL_PORT) \
			--tunnel-port-base=$(TUNNEL_PORT_BASE) \
			--auth-token=$(AUTH_TOKEN) \
			--log-level=$(LOG_LEVEL) \
			--tls \
			--tls-cert=$(TLS_CERT) \
			--tls-key=$(TLS_KEY); \
	else \
		$(GOCMD) run main.go \
			--public-host=$(PUBLIC_HOST) \
			--public-port=$(PUBLIC_PORT) \
			--control-host=$(CONTROL_HOST) \
			--control-port=$(CONTROL_PORT) \
			--tunnel-port-base=$(TUNNEL_PORT_BASE) \
			--auth-token=$(AUTH_TOKEN) \
			--log-level=$(LOG_LEVEL); \
	fi

# Help command
help:
	@echo "Available commands:"
	@echo "  make build         - Build the application"
	@echo "  make test          - Run tests"
	@echo "  make clean         - Clean up build artifacts"
	@echo "  make deps          - Update dependencies"
	@echo "  make install-deps  - Install dependencies"
	@echo "  make generate-proto - Generate protobuf files"
	@echo "  make install       - Install the binary to /usr/local/bin"
	@echo "  make gen-certs     - Generate TLS certificates"
	@echo "  make run-local     - Run locally with config from config.yaml"
	@echo "  make all           - Clean, test, and build"
	@echo "  make help          - Show this help"