# INSTALL-MCP-LOCAL.md — Install the bit-rag MCP client locally

This guide shows how to install the `bit-rag-mcp` binary on your local machine
and connect it to a remote bit-multi-brain-rag dashboard (e.g. deployed on
Easypanel) so that any MCP-capable AI agent can run semantic code search.

---

## Architecture

```
AI Agent (Claude / Factory / OpenCode / Codex / Cursor / Continue / Windsurf)
    │  MCP over stdio (JSON-RPC 2.0)
    ▼
bit-rag-mcp  ← runs LOCALLY (this guide installs it)
    │  HTTPS POST /api/v1/search
    │  Authorization: Bearer <DASHBOARD_API_KEY>
    ▼
bit-rag dashboard (Easypanel) :8081
    │  internal Docker network (NOT exposed publicly)
    ▼
embedder + Qdrant
```

**Only ONE port is exposed publicly: the dashboard.** Qdrant and the embedder
stay on the internal network. Your local MCP binary speaks HTTPS to the
dashboard and the dashboard proxies search to the backends.

**Source code never leaves your machine.** Only the query text + project name
travel over the wire.

---

## Prerequisites

On your local machine:
- **Go 1.24+** (https://go.dev/dl/) — needed to build the binary
- **C toolchain**:
  - Windows: included with Go; or install MSYS2 / TDM-GCC if missing
  - macOS: `xcode-select --install`
  - Linux: `sudo apt install build-essential` (Debian/Ubuntu) or equivalent

On the server (already deployed via [DEPLOY-EASYPANEL.md](./DEPLOY-EASYPANEL.md)):
- The dashboard is reachable at a public URL (e.g. `https://bit-rag.example.com`)
- You know one of the values from the server's `DASHBOARD_API_KEYS` env

---

## Step 1 — Build & install the MCP binary

### Windows (PowerShell)

```powershell
# From the repo root
cd D:\path\to\bit-multi-brain-rag

# Build + install to %LOCALAPPDATA%\Programs\bit-rag\bit-rag-mcp.exe
.\scripts\install-mcp.ps1

# Or build + connectivity test in one shot
.\scripts\install-mcp.ps1 `
  -DashboardUrl "https://bit-rag.your-domain.com" `
  -ApiKey "your-strong-key" `
  -Test
```

To uninstall:
```powershell
.\scripts\install-mcp.ps1 -Uninstall
```

### Linux / macOS (Bash)

```bash
# From the repo root
cd /path/to/bit-multi-brain-rag

# Build + install to ~/.local/bin/bit-rag-mcp
chmod +x ./scripts/install-mcp.sh
./scripts/install-mcp.sh

# Build + connectivity test
DASHBOARD_URL="https://bit-rag.your-domain.com" \
DASHBOARD_API_KEY="your-strong-key" \
  ./scripts/install-mcp.sh --test

# Make sure ~/.local/bin is on PATH:
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

To uninstall:
```bash
./scripts/install-mcp.sh --uninstall
```

---

## Step 2 — Register MCP in your AI client

Choose your client below. The recipe is always:

> Add an `mcpServers.bit-rag` entry pointing to the installed binary, with two
> env vars: `DASHBOARD_URL` and `DASHBOARD_API_KEY`.

### Claude Desktop

Config file:
- **macOS:** `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Linux:** `~/.config/Claude/claude_desktop_config.json`
- **Windows:** `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/home/USER/.local/bin/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
        "DASHBOARD_API_KEY": "your-strong-key"
      }
    }
  }
}
```

Restart Claude Desktop. Look for the 🔌 icon → `bit-rag` should be listed.

### Factory

Config file: `~/.factory/config.json`

```json
{
  "mcp": {
    "servers": {
      "bit-rag": {
        "command": "/home/USER/.local/bin/bit-rag-mcp",
        "env": {
          "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
          "DASHBOARD_API_KEY": "your-strong-key"
        }
      }
    }
  }
}
```

Also copy the skill file so Factory auto-loads guidance:
```bash
mkdir -p ~/.factory/skills
cp skills/factory/bit-rag.md ~/.factory/skills/
```

### OpenCode

Config file: `~/.opencode/config.json`

```json
{
  "mcp": {
    "servers": {
      "bit-rag": {
        "command": "/home/USER/.local/bin/bit-rag-mcp",
        "env": {
          "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
          "DASHBOARD_API_KEY": "your-strong-key"
        }
      }
    }
  }
}
```

Also copy the skill file:
```bash
mkdir -p ~/.opencode/skills
cp skills/opencode/bit-rag.md ~/.opencode/skills/
```

### Codex CLI

Codex MCP config (location varies — check `codex --help`). Typical block:

```toml
[mcp.bit-rag]
command = "/home/USER/.local/bin/bit-rag-mcp"
env.DASHBOARD_URL     = "https://bit-rag.your-domain.com"
env.DASHBOARD_API_KEY = "your-strong-key"
```

### Cursor

Config file: `~/.cursor/mcp.json` (or via Cursor Settings → MCP)

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/home/USER/.local/bin/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
        "DASHBOARD_API_KEY": "your-strong-key"
      }
    }
  }
}
```

### Continue (VS Code extension)

Config file: `~/.continue/config.json`

```json
{
  "experimental": {
    "modelContextProtocolServers": [
      {
        "transport": {
          "type": "stdio",
          "command": "/home/USER/.local/bin/bit-rag-mcp",
          "env": {
            "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
            "DASHBOARD_API_KEY": "your-strong-key"
          }
        }
      }
    ]
  }
}
```

### Windsurf

Config file: `~/.codeium/windsurf/mcp_config.json`

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/home/USER/.local/bin/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
        "DASHBOARD_API_KEY": "your-strong-key"
      }
    }
  }
}
```

---

## Step 3 — Index your projects (server-side, one-time per repo)

The MCP only **queries** the index — it doesn't build it. Open the dashboard
in your browser (`https://bit-rag.your-domain.com`), create or select a
project, point it at a Git URL or upload a folder, and run "Index".

Indexing happens server-side using the dashboard's API key (different keyset
from MCP if you so choose). See [DEPLOY-EASYPANEL.md](./DEPLOY-EASYPANEL.md)
for indexing details.

---

## Step 4 — Verify

In your AI agent, ask something like:

> Use bit-rag to find the JWT validation middleware in project `my-api`.

The agent should call `rag_search_code` with `{project: "my-api", query:
"JWT validation middleware"}` and return file/line citations.

---

## Troubleshooting

### MCP fails to start (`config error`)
- Verify env vars are set in the MCP client config block (not your shell env —
  the agent spawns MCP as a subprocess and only forwards the `env` block).

### `dashboard healthz failed at boot`
- `curl https://bit-rag.your-domain.com/healthz` should return 200.
- Check Easypanel deployment status; ensure `dashboard` service is up.
- Check that the URL has no trailing path other than the root (just the host).

### `401 unauthorized`
- The `DASHBOARD_API_KEY` value does not match any of the
  `DASHBOARD_API_KEYS` (comma-separated list) on the server. Re-check.

### `503 backend unavailable`
- The dashboard is up but Qdrant or the embedder is unreachable from it.
- Check Easypanel logs for the `qdrant` and `embedder` services.

### Tool calls "no matches" but you're sure there's code
- Project name is wrong (it's case-sensitive — match the dashboard exactly).
- Project has not been indexed yet — go to the dashboard and run Index.
- Embedder is using wrong pooling and producing zero-vectors. See
  `infra/easypanel/.env.example` — `embedder` must pass `--pooling mean`
  for the voyage-code-3 backbone (this is set in the compose files).

### Tool calls are slow
- Default timeout is 30s. Increase via `MCP_TIMEOUT_S=60` in the env block.
- Check Easypanel resource limits — embedder may be CPU-starved.

---

## Security recommendations

1. **Use a unique API key per developer.** `DASHBOARD_API_KEYS` is
   comma-separated. Rotate by removing one entry.
2. **HTTPS only.** Configure Easypanel's Caddy/Traefik to terminate TLS;
   never expose plain HTTP.
3. **Network ACL.** If your dashboard is internal-only, put it behind
   Tailscale / Cloudflare Tunnel and point `DASHBOARD_URL` at the private
   hostname.
4. **No secrets in queries.** The query text is logged server-side for
   debugging. Don't paste passwords / tokens into RAG queries.

---

## Updating

Pull latest, re-run installer:

```bash
git pull
./scripts/install-mcp.sh        # or scripts\install-mcp.ps1 on Windows
# Restart your AI agent so it re-spawns MCP with the new binary.
```
