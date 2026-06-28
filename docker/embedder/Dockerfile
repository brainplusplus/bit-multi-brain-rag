# =============================================================================
# Dockerfile untuk llama.cpp embedding server (voyage-4-nano Q8).
#
# Source: poc-cocoindex/docker-voyage-nano/Dockerfile (sudah diverifikasi:
#   --pooling mean fix, LD_LIBRARY_PATH fix, bake-in model).
#
# Dual CPU/GPU build via ARG:
#   --build-arg BASE_IMAGE=ghcr.io/ggml-org/llama.cpp:server          (CPU)
#   --build-arg BASE_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda     (NVIDIA GPU)
#   --build-arg N_GPU_LAYERS=99   (only meaningful for GPU; 0 = pure CPU)
#
# Build CPU:  docker build -f docker/embedder/Dockerfile -t bit-rag-embedder:cpu docker/embedder
# Build GPU:  docker build -f docker/embedder/Dockerfile \
#               --build-arg BASE_IMAGE=ghcr.io/ggml-org/llama.cpp:server-cuda \
#               --build-arg N_GPU_LAYERS=99 \
#               -t bit-rag-embedder:gpu docker/embedder
#
# Run CPU:   docker run --rm -p 8080:8080 bit-rag-embedder:cpu
# Run GPU:   docker run --rm --gpus all -p 8080:8080 bit-rag-embedder:gpu
#
# Notes:
#   - Model di-bake ke image (372MB extra, image total ~1GB CPU / ~2.5GB GPU).
#   - --pooling mean WAJIB untuk voyage-4-nano (default cls bikin recall salah).
#   - --ctx-size 32768 cocok voyage-4-nano hard cap (effective chunk dipotong
#     di indexer pakai EffectiveChunkTokens()).
#   - --n-gpu-layers 0 di base CPU image → llama.cpp ignore; di CUDA image
#     dengan N_GPU_LAYERS=99 → offload semua layer ke GPU.
# =============================================================================

ARG BASE_IMAGE=ghcr.io/ggml-org/llama.cpp:server
FROM ${BASE_IMAGE}

# Default 0 = no GPU offload. Overridden via --build-arg for GPU image.
ARG N_GPU_LAYERS=0
ENV N_GPU_LAYERS=${N_GPU_LAYERS}

# FIX: libllama-server-impl.so ada di /app, bukan di ld.so default search path.
ENV LD_LIBRARY_PATH=/app

# Bake model ke image (saat build time).
RUN apt-get update \
    && apt-get install -y --no-install-recommends wget ca-certificates \
    && mkdir -p /models \
    && wget -q --show-progress -O /models/voyage-4-nano-q8_0.gguf \
        "https://huggingface.co/jsonMartin/voyage-4-nano-gguf/resolve/main/voyage-4-nano-q8_0.gguf?download=true" \
    && apt-get purge -y wget \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
  CMD curl -f http://localhost:8080/health || exit 1

# Use shell-form to expand $N_GPU_LAYERS. Flags voyage-4-nano:
#   --ubatch-size 2048 must be >= max chunk tokens (EffectiveChunkTokens = 2000)
#     to avoid "input too large to process" errors from llama.cpp.
#   --n-gpu-layers 99 offloads all layers to GPU (ignored on CPU image).
ENTRYPOINT ["/bin/sh", "-c", "exec /app/llama-server \
    --model /models/voyage-4-nano-q8_0.gguf \
    --embedding \
    --no-webui \
    --pooling mean \
    --port 8080 \
    --host 0.0.0.0 \
    --ctx-size 32768 \
    --ubatch-size 2048 \
    --threads -1 \
    --parallel 4 \
    --cont-batching \
    --n-gpu-layers ${N_GPU_LAYERS}"]
