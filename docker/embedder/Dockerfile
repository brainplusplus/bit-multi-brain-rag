# =============================================================================
# Dockerfile untuk llama.cpp embedding server (voyage-4-nano Q8).
#
# Source: poc-cocoindex/docker-voyage-nano/Dockerfile (sudah diverifikasi:
#   --pooling mean fix, LD_LIBRARY_PATH fix, bake-in model).
#
# Build:  docker build -f docker/embedder/Dockerfile -t bit-rag-embedder:latest docker/embedder
# Run:    docker run --rm -p 8080:8080 -e LLAMA_API_KEY=secret bit-rag-embedder:latest
#
# Notes:
#   - Model di-bake ke image (372MB extra, image total ~1GB). Trade-off:
#     deploy sekali langsung jalan, tanpa volume mount untuk model.
#   - --pooling mean WAJIB untuk voyage-4-nano (default cls bikin recall salah).
# =============================================================================

FROM ghcr.io/ggml-org/llama.cpp:server

# FIX: libllama-server-impl.so ada di /app, bukan di ld.so default search path.
# Tanpa ini, llama-server gagal load: "cannot open shared object file".
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

# Hanya expose port internal embedding. Di docker-compose tidak di-publish
# ke host (akses cuma dari dashboard via network internal).
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=120s --retries=3 \
  CMD curl -f http://localhost:8080/health || exit 1

# Flags voyage-4-nano (lihat poc-cocoindex/docker-voyage-nano/Dockerfile untuk
# justifikasi tiap flag).
ENTRYPOINT ["/app/llama-server"]
CMD ["--model", "/models/voyage-4-nano-q8_0.gguf", \
     "--embedding", \
     "--no-webui", \
     "--pooling", "mean", \
     "--port", "8080", \
     "--ctx-size", "8192", \
     "--threads", "-1", \
     "--parallel", "4", \
     "--cont-batching"]
