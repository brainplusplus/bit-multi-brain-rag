# bit-multi-brain-rag Makefile
# Targets for building dashboard + embedder images (CPU and GPU variants).

# --- Variables --------------------------------------------------------------
DASHBOARD_IMAGE ?= bit-rag-dashboard:latest
EMBEDDER_CPU_IMAGE ?= bit-rag-embedder:cpu
EMBEDDER_GPU_IMAGE ?= bit-rag-embedder:gpu

# llama.cpp base images (override if you need a pinned digest).
LLAMACPP_CPU_BASE ?= ghcr.io/ggml-org/llama.cpp:server
LLAMACPP_GPU_BASE ?= ghcr.io/ggml-org/llama.cpp:server-cuda

# GPU layer offload (99 = all layers on GPU; reduce if VRAM is tight).
N_GPU_LAYERS ?= 99

# --- Help -------------------------------------------------------------------
.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Dashboard --------------------------------------------------------------
.PHONY: dashboard
dashboard: ## Build dashboard image
	docker build -t $(DASHBOARD_IMAGE) .

# --- Embedder: CPU ---------------------------------------------------------
.PHONY: embedder-cpu
embedder-cpu: ## Build CPU embedder image (voyage-4-nano Q8, llama.cpp:server)
	docker build \
		-f docker/embedder/Dockerfile \
		--build-arg BASE_IMAGE=$(LLAMACPP_CPU_BASE) \
		--build-arg N_GPU_LAYERS=0 \
		-t $(EMBEDDER_CPU_IMAGE) \
		docker/embedder

# --- Embedder: GPU ---------------------------------------------------------
.PHONY: embedder-gpu
embedder-gpu: ## Build GPU embedder image (CUDA, requires NVIDIA Container Toolkit on host)
	docker build \
		-f docker/embedder/Dockerfile \
		--build-arg BASE_IMAGE=$(LLAMACPP_GPU_BASE) \
		--build-arg N_GPU_LAYERS=$(N_GPU_LAYERS) \
		-t $(EMBEDDER_GPU_IMAGE) \
		docker/embedder

.PHONY: embedder-all
embedder-all: embedder-cpu embedder-gpu ## Build both CPU and GPU embedder images

# --- Convenience runs -------------------------------------------------------
.PHONY: run-embedder-cpu
run-embedder-cpu: ## Run CPU embedder standalone on :8080
	docker run --rm -p 8080:8080 --name bit-rag-embedder-cpu $(EMBEDDER_CPU_IMAGE)

.PHONY: run-embedder-gpu
run-embedder-gpu: ## Run GPU embedder standalone on :8080 (needs --gpus all)
	docker run --rm --gpus all -p 8080:8080 --name bit-rag-embedder-gpu $(EMBEDDER_GPU_IMAGE)

# --- Go targets -------------------------------------------------------------
.PHONY: build
build: ## Build dashboard binary locally (CGO required for tree-sitter)
	go build -o bin/dashboard ./cmd/dashboard

.PHONY: test
test: ## Run all tests
	go test ./...

.PHONY: vet
vet: ## go vet ./...
	go vet ./...

.PHONY: clean
clean: ## Remove built binaries
	rm -rf bin
