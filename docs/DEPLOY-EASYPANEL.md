# Deploy ke Easypanel

Panduan deploy `bit-multi-brain-rag` ke Easypanel (self-hosted PaaS).

## Pilih 1 dari 2 opsi compose

| | **All-in-one** | **Split** |
|---|---|---|
| File | `docker-compose.all.yml` | `docker-compose.qdrant.yml` + `docker-compose.yml` |
| Services | dashboard + embedder + qdrant (1 deploy) | qdrant (deploy 1) + bit-rag stack (deploy 2) |
| Easypanel project | 1 project | 2 project |
| Shared network | tidak butuh | butuh `bit-rag-external` |
| Upgrade Qdrant | restart seluruh stack | restart Qdrant aja |
| Share Qdrant | tidak bisa | bisa multi-project |
| **Cocok untuk** | single-tenant, deploy paling simpel | multi-tenant, ops mature |

Kalau ragu → pakai **all-in-one** (`docker-compose.all.yml`). Lebih simpel.

## Arsitektur

```
                                                  ┌──────────────────┐
                                                  │  Easypanel host  │
                                                  └────────┬─────────┘
                                                           │
              ┌────────────────────────────────────────────┼─────────────────────────────────────────┐
              │                                            │                                          │
              │  ┌──────────────────────────────┐          │  ┌──────────────────────────────────┐  │
              │  │ docker-compose.qdrant.yml    │          │  │ docker-compose.yml               │  │
              │  │                              │          │  │                                  │  │
              │  │   ┌────────────────┐         │          │  │   ┌──────────────────┐           │  │
              │  │   │ qdrant         │         │          │  │   │ dashboard :8081 ◄─┼───── Easypanel reverse proxy
              │  │   │ (no host port) │         │          │  │   │                  │           │   (user domain)
              │  │   └──────┬─────────┘         │          │  │   └────────┬─────────┘           │  │
              │  │          │                   │          │  │            │                      │  │
              │  └──────────┼───────────────────┘          │  │            │                      │  │
              │             │                              │  │   ┌────────▼─────────┐            │  │
              │             │                              │  │   │ embedder :8080   │            │  │
              │             │                              │  │   │ (no host port)   │            │  │
              │             │                              │  │   └──────────────────┘            │  │
              │             │                              │  │                                   │  │
              │             │   bit-rag-external network   │  └───────────────────────────────────┘  │
              │             └─────────────────────────────────────────┘                              │
              └──────────────────────────────────────────────────────────────────────────────────────┘

Port exposure:
  ✅ dashboard :8081  → di-publish (Easypanel reverse-proxy ke domain user)
  ❌ embedder  :8080  → INTERNAL ONLY (cuma dashboard yang akses)
  ❌ qdrant    :6333  → INTERNAL ONLY (cross-compose via bit-rag-external network)
  ❌ qdrant    :6334  → INTERNAL ONLY (gRPC, tidak dipakai dashboard sekarang)
```

## Langkah deploy

### 1. Generate secrets

Sebelum deploy, generate API keys (jangan pakai placeholder!):

```bash
# Dashboard API key (akan dipakai user untuk akses /api/v1/*)
openssl rand -hex 32

# Qdrant API key
openssl rand -hex 32

# Embedding server API key
openssl rand -hex 32
```

Simpan ketiganya, akan di-set di Easypanel env.

---

## Opsi A: All-in-one (`docker-compose.all.yml`) — RECOMMENDED untuk single-tenant

### A.1. Deploy

1. Buat project baru di Easypanel: `bit-rag`
2. Connect Git repo (atau upload source) → pilih "Compose" service type
3. Point ke `docker-compose.all.yml`
4. Set env vars di Easypanel UI (lihat `.env.example`):
   - `DASHBOARD_API_KEYS` = secret 1 (comma-separated multi-key)
   - `LLAMA_API_KEY` = secret 3
   - `QDRANT_API_KEY` = secret 2
5. Deploy (~5-10 menit first build: download model GGUF, build Go binary)
6. Easypanel auto-detect port `8081` → tawarkan domain mapping

### A.2. Verifikasi port exposure

```bash
docker ps --format "table {{.Names}}\t{{.Ports}}"

# EXPECTED:
# NAMES              PORTS
# bit-rag-dashboard  0.0.0.0:8081->8081/tcp     ← OK
# bit-rag-embedder   8080/tcp                    ← INTERNAL only
# bit-rag-qdrant     6333/tcp, 6334/tcp          ← INTERNAL only
```

---

## Opsi B: Split compose (Qdrant terpisah) — untuk multi-tenant

### B.2. Buat shared network (sekali aja)

Network `bit-rag-external` dipakai bersama Qdrant + dashboard untuk discovery.

**Di Easypanel:**
- Buka Settings → Networks → Create network
- Name: `bit-rag-external`
- Driver: bridge

**Atau via SSH ke host Easypanel:**
```bash
docker network create bit-rag-external
```

### 3. Deploy Qdrant dulu

1. Buat project baru di Easypanel: `bit-rag-qdrant`
2. Pilih "Compose" service type
3. Upload / paste `docker-compose.qdrant.yml`
4. Set env vars di Easypanel UI:
   - `QDRANT_API_KEY` = secret dari langkah 1
5. Deploy
6. Verify: `docker logs bit-rag-qdrant` → `[INFO] Listening on 0.0.0.0:6333`

**Tidak ada port yang di-expose ke publik.** Qdrant cuma dapat diakses dari container lain di network `bit-rag-external`.

### 4. Deploy bit-rag (dashboard + embedder)

1. Buat project baru di Easypanel: `bit-rag-app`
2. Connect Git repo (atau upload source)
3. Pilih "Compose" service type, point ke `docker-compose.yml`
4. Set env vars di Easypanel UI (lihat `.env.example`):
   - `DASHBOARD_API_KEYS` = secret 1 dari langkah 1 (comma-separated kalau multi-key)
   - `LLAMA_API_KEY` = secret 3
   - `EMBEDDING_API_KEY` = sama dengan `LLAMA_API_KEY`
   - `QDRANT_URL` = `http://qdrant:6333` (default sudah benar untuk same-network)
   - `QDRANT_API_KEY` = secret 2 dari langkah 1
5. Deploy
6. Tunggu first build (~5 menit: download model GGUF ~370 MB, build Go binary)
7. Easypanel akan auto-detect port `8081` dan tawarkan domain mapping

### 5. Verifikasi

```bash
# Healthz (public, no auth)
curl https://your-domain.example.com/healthz
# {"service":"bit-multi-brain-rag-dashboard","status":"ok"}

# Auth check (no key → 401)
curl -i https://your-domain.example.com/api/v1/projects
# HTTP/2 401

# Auth ok
curl -H "Authorization: Bearer $DASHBOARD_API_KEY" \
     https://your-domain.example.com/api/v1/projects
# {"projects": null}
```

## Port exposure detail (audit checklist)

Verifikasi tidak ada port internal yang bocor:

```bash
# Di host Easypanel:
docker ps --format "table {{.Names}}\t{{.Ports}}"

# EXPECTED:
# NAMES              PORTS
# bit-rag-dashboard  0.0.0.0:8081->8081/tcp        ← OK, public via Easypanel
# bit-rag-embedder   8080/tcp                       ← INTERNAL only (no host bind)
# bit-rag-qdrant     6333/tcp, 6334/tcp             ← INTERNAL only
```

Kalau ada `0.0.0.0:6333` atau `0.0.0.0:8080`, BERHENTI — ada port leak. Cek `docker-compose.yml`, pastikan `ports:` block tidak di-uncomment untuk embedder/qdrant.

## Backup / restore

- **SQLite (dashboard data):** volume `bit-rag-data` di-mount ke `/app/data`. Backup dengan `docker run --rm -v bit-rag-data:/data -v $(pwd):/backup alpine tar czf /backup/dashboard-$(date +%F).tar.gz /data`.
- **Qdrant (vectors):** volume `qdrant-storage`. Gunakan Qdrant snapshot API (`POST /collections/{name}/snapshots`) atau backup volume langsung.

## Indexing (async, ADR-0005)

Sejak ADR-0005, indexing **TIDAK** lagi blocking HTTP call. `POST /api/v1/index`
return `202 Accepted` segera + job descriptor JSON; pekerjaan jalan di
goroutine. UI HTMX dashboard poll status tiap 2 detik secara otomatis.

### Endpoint summary

| Endpoint | Method | Return | Tujuan |
|---|---|---|---|
| `/api/v1/index` | POST | **202** + `{id, status, project, ...}` | Enqueue indexing (idempotent per project) |
| `/api/v1/index/status?project=X` | GET | 200 + live Job JSON | Poll progress / final state |
| `/api/v1/index/cancel` | POST | 200 + `{status: cancel signalled}` | Stop running job |
| `/ui/index` | POST (form) | 202 + HTMX partial | UI button (self-polling) |
| `/ui/index/status?project=X` | GET | HTMX partial | UI poll target (every 2s) |
| `/ui/index/cancel` | POST (form) | 200 + HTMX partial | UI Cancel button |

### Polling flow (curl example)

```bash
# 1. enqueue
curl -X POST https://dashboard.example.com/api/v1/index \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"project":"my-repo"}'
# → 202 {"id":"...","status":"queued",...}

# 2. poll every few seconds
while true; do
  curl -s "https://dashboard.example.com/api/v1/index/status?project=my-repo" \
       -H "Authorization: Bearer $KEY" | jq '{status, files_done, files_total, indexed_done}'
  sleep 5
done
# → {"status":"running","files_done":3,"files_total":20,"indexed_done":40}
# → {"status":"running","files_done":11,...}
# → {"status":"succeeded","files_done":20,"files_total":20,"indexed_done":236}

# 3. (optional) cancel mid-flight
curl -X POST https://dashboard.example.com/api/v1/index/cancel \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"project":"my-repo"}'
```

### Concurrency

- **Per-project lock**: 2× POST /api/v1/index untuk project sama → same job ID
  dikembalikan (idempotent), tidak ada duplikasi work.
- Project berbeda → jalan paralel (sharing embedder, throughput tergantung
  beban llama.cpp).

### Restart recovery

Kalau container dashboard restart saat indexing berjalan, job orphan otomatis
di-flip ke status `interrupted` di startup. UI menampilkan pesan "Click
Re-index to retry" daripada spinner palsu.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| dashboard exit 1: "config validate: production must have DASHBOARD_API_KEYS" | env not set | set `DASHBOARD_API_KEYS` di Easypanel |
| dashboard log: "qdrant unreachable" | network/DNS issue | pastikan dashboard + qdrant di network `bit-rag-external` yang sama |
| embedder takes >2 min to start | model load (372 MB) saat first boot | normal, lihat `start_period: 120s` di healthcheck |
| recall sangat rendah (<10%) | pooling salah (default cls, harus mean) | confirm `--pooling mean` di embedder Dockerfile (sudah benar di repo ini) |
| dashboard:8081 timeout dari Easypanel | container healthcheck failing | cek `docker logs bit-rag-dashboard` |
| status="interrupted" di UI setelah deploy baru | container restart sementara job aktif | normal recovery behavior; klik Re-index untuk retry |
| POST /api/v1/index returns 503 "indexer unavailable" | embedder atau qdrant offline | cek health kedua service: `docker ps`, `curl :8080/healthz` |

## MCP server

MCP binary (`cmd/mcp`) **tidak** di-deploy sebagai Easypanel service. MCP runs over stdio, bukan HTTP. Untuk pakai MCP:

1. Build binary lokal: `go build -o mcp ./cmd/mcp`
2. Konfig di MCP client (Claude Desktop, dll):
   ```json
   {
     "mcpServers": {
       "bit-rag": {
         "command": "/path/to/mcp",
         "env": {
           "QDRANT_URL": "https://qdrant.your-domain.example.com",
           "QDRANT_API_KEY": "...",
           "EMBEDDING_ENDPOINT": "https://embedder.your-domain.example.com",
           "EMBEDDING_API_KEY": "..."
         }
       }
     }
   }
   ```

Note: untuk akses MCP ke Qdrant/embedder di Easypanel dari luar, perlu expose port via Easypanel (separate sub-domain dengan TLS + auth). Itu di luar scope deploy dashboard ini.
