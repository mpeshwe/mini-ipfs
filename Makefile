.PHONY: build run test lint clean docker-build compose-up compose-down help

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod

# Binary names
BINARY_NAME=mini-ipfs-node
BUILD_DIR=bin

# Docker parameters
DOCKER_IMAGE=mini-ipfs:dev
COMPOSE_FILE=build/compose.yml

help: ## Show this help message
	@echo "Available commands:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'

build: ## Build the Mini-IPFS node binary
	@echo "Building Mini-IPFS node..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/node

run: build ## Build and run the Mini-IPFS node locally
	@echo "Starting Mini-IPFS node..."
	./$(BUILD_DIR)/$(BINARY_NAME)

test: ## Run all tests
	@echo "Running tests..."
	$(GOTEST) -v ./...

clean: ## Clean build artifacts
	@echo "Cleaning..."
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)/
	rm -f coverage.out coverage.html

deps: ## Download and tidy dependencies
	@echo "Managing dependencies..."
	$(GOMOD) tidy
	$(GOMOD) download

docker-build: ## Build Docker image
	@echo "Building Docker image..."
	docker build -f build/Dockerfile -t $(DOCKER_IMAGE) .

compose-up: ## Start multi-node setup with Docker Compose
	@echo "Starting Mini-IPFS network..."
	docker-compose -f $(COMPOSE_FILE) up --build -d

compose-down: ## Stop Docker Compose network
	@echo "Stopping Mini-IPFS network..."
	docker-compose -f $(COMPOSE_FILE) down -v
compose-reup:
	docker compose -f build/compose.yml down --volumes --remove-orphans
	docker compose --progress=plain -f build/compose.yml build --no-cache
	docker compose -f build/compose.yml up -d
