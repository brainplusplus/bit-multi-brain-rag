# Architecture Decision Records (ADR)

Project: **bit-multi-brain-rag**

## Index

| ADR | Title | Status | Date |
|---|---|---|---|
| [0001](0001-embedding-model-and-index-isolation.md) | Embedding Model Selection and Index Isolation | Accepted | 2026-06-27 |
| [0002](0002-dashboard-and-index-isolation.md) | Dashboard Scope and Multi-Project Index Isolation | Accepted | 2026-06-27 |
| [0003](0003-dashboard-auth-api-key.md) | Dashboard Authentication via API Key | Accepted | 2026-06-27 |
| [0004](0004-hybrid-architecture-best-of-both.md) | Hybrid Architecture (Best of cocoindex-code + enowx-rag) | Accepted | 2026-06-27 |
| [0005](0005-background-indexing-jobs.md) | Background indexing jobs (async API + HTMX polling) | Accepted | 2026-06-27 |
| [0006](0006-gpu-embedding-acceleration.md) | GPU Embedding Acceleration | Accepted | 2026-06-28 |
| [0007](0007-gap-analysis-and-improvement-roadmap.md) | Gap Analysis & Improvement Roadmap | Accepted | 2026-06-28 |

## Conventions

- Format: Michael Nygard ADR (Context → Decision → Consequences)
- Status: Proposed → Accepted → Deprecated / Superseded
- File name: `NNNN-kebab-case-title.md` (4-digit sequential)
- One decision per ADR. Do not bundle unrelated decisions.
- Supersede via new ADR referencing the old one (`Superseded by ADR-NNNN`).
