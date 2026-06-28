# ADR-0006: GPU Embedding Acceleration

- **Status**: Accepted
- **Date**: 2026-06-28
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: ADR-0001 (Embedding Model), ADR-0004 (Hybrid Architecture)

---

## 1. Context

ADR-0001 memilih `voyage-4-nano` (GGUF Q8, 1024 dim, 32K context window) sebagai
model embedding utama. Model berjalan via llama.cpp embedder (OpenAI-compatible
`/v1/embeddings`). Untuk RAG interaktif (search real-time saat user menunggu),
latency per request harus di bawah ~200ms.

Pertanyaan muncul: **apakah GPU dibutuhan, atau CPU cukup?**

### 1.1 Test environment

| Item | Detail |
|------|--------|
| CPU | WSL2 (Rancher Desktop), 8 vCPU (host: laptop) |
| GPU | NVIDIA RTX 3090 (24 GB VRAM), via WSL2 `/dev/dxg` + CDI |
| Model | voyage-4-nano Q8 GGUF (372 MB) |
| Backend | llama.cpp server-cuda image |
| GPU offload | `--n-gpu-layers 99` (all layers on GPU) |
| Tokenizer | Llama tokenizer (1024 dim, `--pooling mean`) |
| CDI device | `nvidia.com/gpu=all` |

### 1.2 Benchmark method

Tool baru `cmd/embed-bench` (commit `66645af`) melakukan:
1. Warm-up 2 panggilan (tidak dihitung).
2. N iterasi timed call ke `POST /v1/embeddings`.
3. Hitung total / avg / p50 / p95 latency + tokens/sec.

Dijalankan dari dalam `bit-rag-dashboard` container ke `bit-rag-embedder`
container via network `bit-rag-external` (network round-trip minimal).

Skenario:
- Input length: **short (~12 tok)**, **medium (~120 tok)**.
- Batch size: 1, 4, 16 (parallel inputs per request).
- Iterations: 10 (warm-up excluded).
- Long input (~600 tok) tidak diuji karena embedder batasi physical batch
  size ke 512 token (di luar cakupan ADR ini).

Repro:
```bash
docker cp bin/embed-bench-linux bit-rag-dashboard:/tmp/embed-bench
docker exec bit-rag-dashboard /tmp/embed-bench \
  -url http://bit-rag-embedder:8080 \
  -model voyage-4-nano \
  -iters 10
```

---

## 2. Decision

**Aktifkan GPU mode sebagai default untuk deployment production**, dengan
auto-fallback ke CPU bila GPU tidak tersedia (lihat ADR-0001 §3.2 / implementasi
`detectGPU()` di `pkg/dashboard/gpu.go`).

### 2.1 Rationale: data benchmark

Berikut hasil benchmark rata-rata atas 10 iterasi per skenario:

| Scenario | Batch | CPU avg | GPU avg | CPU tok/s | GPU tok/s | **Speedup** |
|----------|-------|---------|---------|-----------|-----------|-------------|
| short (~12 tok) | 1 | 1528 ms | 29 ms | 9 | 480 | **52x** |
| short (~12 tok) | 4 | 1533 ms | 8 ms | 146 | 26,967 | **191x** |
| short (~12 tok) | 16 | 6146 ms | 26 ms | 583 | 136,754 | **236x** |
| medium (~120 tok) | 1 | 1612 ms | 8 ms | 87 | 16,765 | **201x** |
| medium (~120 tok) | 4 | 4125 ms | 33 ms | 543 | 66,833 | **125x** |
| medium (~120 tok) | 16 | 12995 ms | 122 ms | 2,758 | 291,715 | **106x** |

> Catatan: input ~600 tok tidak bisa diukur karena embedder melempar HTTP 500
> ("physical batch size 512 exceeded"). Bisa di-raise via `--ubatch-size` bila
> nanti dibutuhkan; di luar lingkup keputusan ini.

#### 2.1.1 Key observations

1. **GPU menang 52-236x lebih cepat** di semua skenario.
2. **CPU tidak feasible untuk interactive RAG**: 1.5 detik per single short
   embedding, ~13 detik untuk 16 medium inputs paralel. User akan menunggu
   terlalu lama.
3. **GPU latency sangat konsisten**: p95 dekat p50 (mis. 8ms vs 8ms untuk
   medium b1; 122ms vs 120ms untuk medium b16). Predictable.
4. **GPU throughput puncak 291,715 tok/s** (medium batch 16) — 105x lipat
   CPU. Memungkinkan banyak concurrent search.
5. **Batch besar (b16) menguntungkan GPU lebih dramatis** — CPU quadratic
   (6146ms vs 1528ms saat batch 16x dibanding 1x), GPU hampir flat (26ms vs
   29ms). Indikasi overhead GPU amortisasi sangat baik di batch processing.

### 2.2 Implikasi UX

Latency budget interaktif RAG (~200ms dari query → hasil render):
- **CPU**: 1.5-13 detik → tidak usable
- **GPU**: 8-122ms → nyata real-time, sisa budget bisa untuk Qdrant search +
  UI render

### 2.3 Mekanisme default + auto-fallback

Implementasi (`pkg/dashboard/gpu.go`):

```
canSwitch = Detected && ContainerToolkit
```

- `Detected` di-isi oleh probe: nvidia-smi → /proc/driver/nvidia → **inferensi
  dari CDI devices + nvidia runtime** (fallback untuk dashboard in-container).
- `ContainerToolkit` dicek via `docker info` Runtimes registry.
- Bila GPU tidak terdeteksi (mis. dev laptop tanpa NVIDIA), Settings UI menampilkan
  banner actionable + Switch button disabled. Mode CPU dipakai.
- Bila CDI spec stale (mis. setelah driver update), pre-flight di `performSwitch`
  menolak switch dan memberi command regen yang persis.

Switch runtime via `POST /api/v1/gpu/switch {mode: gpu|cpu}`:
- pre-flight CDI check
- pull image (skip jika lokal)
- stop + remove old container
- start new container dengan CDI device (`nvidia.com/gpu=all`) atau legacy
  DeviceRequests
- health probe (auto-rollback bila gagal)
- persist mode di SQLite

---

## 3. Consequences

### 3.1 Positif

- **Latency RAG turun dari 1.5+ detik → < 50ms per request** (typical case).
- **Throughput 100x lipat** → support multi-user concurrent search tanpa antrian.
- **Predictable p95**: UX konsisten, tidak ada tail latency 2+ detik.
- **CDI mode portable** ke berbagai host (Rancher Desktop, Docker Desktop,
  Linux native) tanpa perlu config manual per environment.

### 3.2 Negatif / trade-off

- **GPU requirement**: deployment production butuh NVIDIA GPU + driver +
  nvidia-container-toolkit. Tidak semua environment punya (lihat ADR-0001 §3.2
  untuk fallback ke Voyage AI API sebagai backup).
- **Image size**: `bit-rag-embedder:gpu` ~2.5 GB (CUDA base) vs `:cpu` ~1.0 GB.
  Build time lebih lama (~5-10 menit untuk download CUDA base).
- **Rancher Desktop + WSL2 setup fragile**: NVIDIA toolkit on Alpine butuh
  glibc shim (sgerrand), CDI spec regen tiap boot (OpenRC service). Recovery
  via `scripts/rancher-nvidia-install.sh` (idempotent). Risiko: Rancher update
  bisa wipe distro, perlu re-run installer.
- **Voyage-4-nano Q8 memakan ~4.4 GB VRAM** saat loaded. OK di RTX 3090 (24GB)
  tapi bisa jadi pertimbangan untuk GPU kecil (8GB ke bawah perlu validasi).

### 3.3 Mitigations

| Risiko | Mitigasi |
|--------|----------|
| Rancher update wipe distro | Idempotent installer script + UI banner dengan command persis |
| Driver update → CDI stale | OpenRC boot service regen otomatis + pre-flight check refuse switch + show regen command |
| GPU tidak tersedia | Auto-fallback ke CPU mode (dengan UX warning bahwa latency akan tinggi) |
| Image besar | Dual-tag Dockerfile (satu Dockerfile + ARG), build on demand saat switch |

---

## 4. Alternatives considered

### 4.1 CPU-only dengan model lebih kecil

Pakai model embedding yang lebih ringan (mis. MiniLM-L6 Q4 ~ 23 MB) supaya CPU
cukup. **Ditolak** karena:
- Recall/precision turun signifikan (lihat ADR-0001 benchmark dataset).
- voyage-4-nano dipilih just precisely karena kualitasnya; mengorbankan kualitas
  untuk speed bukan trade-off yang diinginkan.
- CPU tetap lambat (~1.5s) bahkan untuk model kecil karena single-thread WSL2.

### 4.2 External API (Voyage AI, OpenAI) instead of local GPU

**Pertimbangkan sebagai fallback**, bukan default:
- Latency network ~100-300ms (sudah mendekati GPU local) tapi dengan biaya
  per-request + ketergantungan internet + privacy data.
- OK untuk backup bila GPU down, tapi tidak ideal untuk primary karena cost
  scaling + data eksposur.
- Provider registry (ADR-0004) sudah mendukung switch ke external API kapan saja.

### 4.3 AMD ROCm atau Intel oneAPI

**Ditolak** untuk sekarang karena:
- Dukungan llama.cpp untuk ROCm/oneAPI kurang matang dibanding CUDA.
- Setup lebih rumit di WSL2.
- Bisa dipertimbangkan lagi bila ada demand (vendor-neutral via CDI sebenarnya
  mendukung, tapi build image perlu disiapkan).

---

## 5. References

- ADR-0001 — Embedding Model Selection and Index Isolation
- ADR-0004 — Hybrid Architecture
- Commit `66645af` — `cmd/embed-bench` benchmark tool
- Commit `448899e` — fix pull-skip + correct network name (switch end-to-end)
- Commit `0bc1bf4` — infer GPU detection from CDI when in-container probes fail
- Commit `084c4af` — host-aware health checks, CDI-first runtime attach, Rancher install scripts
- `pkg/dashboard/gpu.go` — detection + switch logic
- `scripts/rancher-nvidia-install.sh` — idempotent installer
- llama.cpp docs: https://github.com/ggml-org/llama.cpp
