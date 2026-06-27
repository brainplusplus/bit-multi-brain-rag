# ADR-0003: Dashboard Authentication via API Key

- **Status**: Accepted
- **Date**: 2026-06-27
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: ADR-0002 (Dashboard and Index Isolation)

---

## 1. Context

The bit-multi-brain-rag dashboard (ADR-0002) is a multi-project RAG explorer exposed
over HTTP. It serves:

- A web UI (project sidebar + embedding result viewer + query explorer).
- HTTP API endpoints (search, index status, model switch, benchmark runner).

Both the UI and the API must be protected from anonymous access before the dashboard
is exposed beyond localhost (Easypanel deploy, Cloudflare Tunnel).

### 1.1 Threat model

| Threat | Source | Impact |
| --- | --- | --- |
| Anonymous reads source code via search API | Internet (Cloudflare Tunnel) | Source code leak across all projects |
| Anonymous triggers re-index / model switch | Internet | DoS (CPU burn), index corruption |
| Anonymous runs benchmark | Internet | Resource exhaustion, cost |
| Shared password leak | Single secret in `.env` | Full access, no per-user audit |

### 1.2 Constraints

- Secrets stored in `.env` (per user request). No external secret manager (Vault, AWS SM)
  in current infra.
- No GPU/auth infra. Must work on CPU-only Easypanel VPS.
- Must be consistent with existing `LLAMA_API_KEY` pattern already in use for the
  embedding backend (ADR-0002 §3.2).
- Global scope: 1 valid key grants access to all projects (no per-project ACL in MVP).

### 1.3 Options considered

| Option | Complexity | Security | Audit | Fit |
| --- | --- | --- | --- | --- |
| **A. Single password gate via `.env`** | Low | Low | None (shared) | Dev only |
| **B. API key multi-key via `.env`** | Low–Med | Medium | Per-key | **Chosen** |
| C. OIDC / OAuth (Google / GitHub login) | High | High | Per-user | Phase 2 |

#### Why not Option A (password gate)
- 1 shared password → no individual identity → no audit log of who did what.
- Password in `.env` → manual rotation (edit file + restart), no revoke per-user.
- If leaked, full access with no forensics.
- Inconsistent with `LLAMA_API_KEY` API-key pattern already deployed.

#### Why not Option C (OIDC/OAuth) now
- Requires external identity provider (Google/GitHub OAuth app) configuration.
- Requires session store (DB or Redis) for OAuth flow.
- Over-engineered for current team size and CPU-only VPS.
- Deferred to a future ADR when team grows or SSO is mandated.

#### Why Option B (API key multi-key)
- Consistent with `LLAMA_API_KEY` already in production (one mental model).
- Per-key identity enables audit logging (which key triggered re-index, benchmark, etc.).
- Keys are individually revocable (remove from `.env` + restart) without affecting others.
- `.env` remains the secret store (per user request) — no new infra.
- Upgrade path to Option C is additive: OIDC can coexist with API keys (keys for
  service-to-service, OIDC for human users).

---

## 2. Decision

### 2.1 Authentication mechanism

The dashboard enforces **bearer API key authentication** on every HTTP route (UI and
API). Requests without a valid key return `401 Unauthorized`.

- Header: `Authorization: Bearer <API_KEY>`
- Keys are defined in `.env` as a comma-separated list (multiple keys supported).
- Key matching is **constant-time** (hmac.compare_digest) to prevent timing attacks.
- Key format: opaque random string, min 32 chars, generated via `secrets.token_urlsafe(32)`.

### 2.2 Configuration

`.env` example:

```dotenv
# Dashboard API keys (comma-separated, multiple users supported)
# Generate new keys with: python -c "import secrets; print(secrets.token_urlsafe(32))"
DASHBOARD_API_KEYS=key_a_abc123...,key_b_def456...,key_c_ghi789...

# Optional: key labels for audit log readability (maps key prefix -> human label)
# Format: <first8chars>=<label>, comma-separated
DASHBOARD_KEY_LABELS=key_a_abc=alice,key_b_def=bob,key_c_ghi=ci
```

### 2.3 Scope (MVP)

- **Global access**: any valid key grants access to all projects, all endpoints.
- **No per-project ACL**: project-level isolation is via index directory structure
  (ADR-0002), not via authz. Authn is global; authz is "valid key = full access".
- **Audit log**: every mutating action (re-index, model switch, benchmark run) records
  the key label + timestamp + action + target project. Read-only actions (search,
  view) are logged at DEBUG level only (to avoid log spam).

### 2.4 Key rotation

- To add a key: append to `DASHBOARD_API_KEYS` in `.env` → restart dashboard.
- To revoke a key: remove from `DASHBOARD_API_KEYS` in `.env` → restart dashboard.
- Rotation window: brief downtime during restart (acceptable for MVP; can be made
  hot-reloadable in Phase 2 via SIGHUP or file watcher).

### 2.5 Frontend (web UI) integration

The web UI is also protected. Flow:

1. User opens dashboard URL → server returns login page (no key in URL/cookie yet).
2. User pastes API key into login form → frontend stores it in `sessionStorage`
   (cleared on tab close, not persisted to disk).
3. Frontend sends `Authorization: Bearer <key>` header on every API call.
4. Logout = clear `sessionStorage`.

> **Why `sessionStorage` not `localStorage`**: limits key exposure to the active tab
> session; not accessible to other tabs or persistent after browser close. Reduces
> XSS secret-exfil window vs. `localStorage`.
>
> **Why not HTTP-only cookie**: would require CSRF protection (double-submit token or
> SameSite=strict). API-key-in-header is stateless and CSRF-immune by design. For an
> internal/team tool, the header approach is simpler and sufficient.

### 2.6 Defense in depth

Even with API key auth, the dashboard should NOT be exposed directly to the internet
without a reverse proxy. Recommended deployment:

```
Internet → Cloudflare Tunnel → Easypanel → dashboard:port
                                  ↓
                          (API key auth at app layer)
```

- Cloudflare Tunnel provides TLS termination + IP allowlist (optional, Cloudflare Access).
- App-layer API key auth is the authoritative check (do not rely solely on network).
- Optional: Cloudflare Access (Zero Trust) in front for SSO — complements, not
  replaces, the API key (defense in depth).

---

## 3. Consequences

### 3.1 Positive

- **Consistent**: same `Authorization: Bearer` pattern as the embedding backend
  (`LLAMA_API_KEY`). One auth model for the whole system.
- **Auditable**: per-key identity enables "who triggered this re-index" forensics.
- **Revocable**: keys can be removed individually without rotating a shared secret.
- **Stateless**: no session DB required. Keys validated against `.env` list each request.
- **No new infra**: `.env` is the only secret store. No Redis, no DB, no OAuth app.
- **CSRF-immune**: API key in `Authorization` header (not cookie) → no CSRF surface.

### 3.2 Negative

- **Manual key management**: keys are generated/added/removed by editing `.env` +
  restart. No self-service portal (admin must do it). Acceptable for small team.
- **Restart required for rotation**: brief downtime (seconds). Hot-reload deferred.
- **Global scope only (MVP)**: cannot restrict a key to specific projects. Any valid
  key sees everything. Per-project ACL deferred to a future ADR if needed.
- **Key in `sessionStorage` (XSS risk)**: if the dashboard has an XSS vulnerability,
  the key can be exfiltrated from `sessionStorage`. Mitigated by: no user-generated
  HTML content in MVP, strict CSP headers, content-type validation on all inputs.
- **No brute-force protection at app layer**: rate limiting relies on reverse proxy
  (Cloudflare) or a middleware (e.g., `slowapi`). A `fail2ban`-style lockout after N
  failed attempts should be added before public exposure.

### 3.3 Risks

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| Key leaked (`.env` committed to git) | Medium | High | `.env` in `.gitignore`; pre-commit hook scanning; rotate on leak |
| Key leaked (shared in chat/logs) | Medium | High | Treat keys as secrets; never log full key (only label/prefix); rotate if exposed |
| Brute-force on key endpoint | Low (behind CF) | Medium | Cloudflare rate limit + app-layer lockout after N fails |
| XSS exfiltrates key from `sessionStorage` | Low | High | Strict CSP, no user HTML, input sanitization |
| `.env` file read by other container | Low | High | Easypanel secret injection (not shared volume); file perms 600 |

### 3.4 Operational checklist (pre-production)

- [ ] `.env` added to `.gitignore` (verify never committed).
- [ ] Generate initial keys via `secrets.token_urlsafe(32)` (NOT `bismillah123`).
- [ ] Add app-layer rate limiting / lockout (e.g., `slowapi` or reverse-proxy rule).
- [ ] Add strict CSP header (`default-src 'self'; ...`).
- [ ] Audit log table/file created (key_label, ts, action, project_id).
- [ ] Login page implemented (key input → `sessionStorage`).
- [ ] Logout implemented (clear `sessionStorage`).
- [ ] All routes return 401 without valid key (integration test).
- [ ] Cloudflare Tunnel configured (not direct port exposure).
- [ ] Document key generation + rotation runbook in dashboard README.

---

## 4. Open questions

1. **Rate limiting implementation**: app-layer (`slowapi`) vs. reverse-proxy (Cloudflare
   WAF rule) vs. both. Decide during implementation.
2. **Audit log storage**: file-based (JSONL) vs. SQLite vs. Easypanel-hosted DB.
   File-based is simplest for MVP; SQLite if queryable history is needed.
3. **Hot-reload of keys**: SIGHUP handler vs. file watcher vs. accept restart downtime.
   Defer to implementation; restart is acceptable for MVP.
4. **Upgrade to OIDC**: when does team size or compliance require it? Triggers a new
   ADR (ADR-00NN) that coexists with API keys (keys for service accounts, OIDC for humans).

---

## 5. References

- ADR-0001: Embedding Model Selection and Index Isolation (`LLAMA_API_KEY` pattern origin)
- ADR-0002: Dashboard and Index Isolation (dashboard scope, endpoints to protect)
- OWASP API Security Top 10 — API2:2023 Broken Authentication
- `hmac.compare_digest` — constant-time string comparison (Python stdlib)
- `secrets.token_urlsafe` — cryptographically secure random strings (Python stdlib)
- Cloudflare Tunnel + Cloudflare Access (Zero Trust) documentation
