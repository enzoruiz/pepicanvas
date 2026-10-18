# TE-004 atomic canvas persistence

Issue #6 (TE-004). Branch `feat/te-004-canvas-persistence`, worktree `/home/enzo/projects/pepicanvas-te-004`, base `beee2b1`.
Scope: PostgreSQL infrastructure (pgx, goose, sqlc, testcontainers, CI) plus the canvas persistence adapter; domain in `internal/canvas`, adapter in `internal/platform/postgres`.

## Specs

- **S1 — Durable mutations survive restart.** Issue: "Las mutaciones confirmadas de dimensiones, contenido, transformaciones, orden y eliminación sobreviven reinicio sin estados parciales." Evidence: "Reinicio después de cada tipo de mutación." After each mutation kind, the PostgreSQL server restarts and a new store reads the identical committed state.
- **S2 — Failure before and after commit.** Issue: "Fallo antes y después de confirmación." A failure before commit leaves no trace of the mutation; a failure after commit (lost answer) keeps the committed effect, and the retry returns the same result without a second effect.
- **S3 — Concurrency.** Issue: "Concurrencia de dimensiones y z-order." Concurrent dimension changes serialize per canvas and the last committed one defines the dimensions; concurrent order changes never assign the same z value twice and follow commit order.
- **S4 — Deletion.** Issue: "Eliminación sin recuperación visible." A committed delete removes the object row; no tombstone or recovery state is readable, and later mutations of that object are rejected as not found.
- **S5 — Identity and isolation.** Issue (DoD): "Cada estado confirmado reaparece idéntico y ningún fallo produce una mutación parcial o recurso cruzado." Every row is keyed by tenant and canvas; equal canvas and object IDs in two tenants never read or mutate each other.
- **S6 — CTRL-IDEM-01 (owned for TE-004).** Issue: "reintentos de TE-004 (#6) antes y después de timeout o reconexión producen un único efecto durable y la misma respuesta, con la semántica de identificador de operación de TE-007; un payload distinto con el mismo identificador se rechaza y la retención acotada se prueba en su vencimiento."
- **S7 — CTRL-CONS-01 early primitive.** Issue: "cada mutación de dimensiones, contenido, transformación, orden y eliminación se confirma en una única transacción PostgreSQL, a la que TE-012 incorpora el evento de outbox."
- **S8 — Linked story invariants.** US-002: "Un streamer activo tiene exactamente un canvas con dimensiones iniciales 1920×1080." US-007: "Ancho y alto enteros; mínimo 320×180 y máximo 3840×2160, inclusivos." and "Redimensionar no altera coordenadas, tamaño, rotación ni orden de objetos." US-009: "Persisten posición, dimensiones, rotación y orden." and "La última manipulación confirmada coloca el objeto arriba."

### Deferred to their owners (retroactive proof)

- CTRL-CONS-01 outbox event → TE-012 (TE-012 depends on TE-004).
- CTRL-TRACE-01 → TE-015. Primitive here: each operation row stores the `correlation_id` of `platform/correlation`.
- CTRL-RESTORE-01 → TE-017 (no own primitive).

### Non-goals

"Transporte en tiempo real, inspección de medios o bloqueos." Also: HTTP routes, the US-002 activation flow, US-008 text style rules, asset foreign keys (TE-006), and updating the release manifest `goModules` (TE-011 release scope).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | infra for S1 | inline, test-first: pgx/goose/testcontainers, embedded migrations, `platform/postgres` open+migrate, restartable test harness, CI job | done | see L3 |
| T2 | S8, S4 | inline, test-first: `internal/canvas` domain types, validation, `Store` port | pending | |
| T3 | S1, S3, S4, S5, S7, S8 | inline, test-first: canvas schema, sqlc queries, one transaction per mutation, z-order | pending | |
| T4 | S2, S6 | inline, test-first: durable operation IDs, fingerprint conflict, retention expiry, fault injection before/after commit | pending | |
| T5 | all | docs, review assess, verification, per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-18): "empieza el desarrollo del issue 6 en su respectivo worktree"
- **L2** (decision): PostgreSQL pinned to `postgres:17.4-alpine@sha256:7062a2109c4b51f3c792c7ea01e83ed12ef9a980886e3b3d380a7d2e5f6ce3f5`, matching `toolchain.postgresql` 17.4 in `releases/client-support-release.json` (CTRL-VERS-01). sqlc runs from a pinned container image instead of a `go.mod` tool directive, to keep its cgo parser out of the module graph.
- **L3** (T1 evidence): RED build failure (`no non-test Go files in .../internal/platform/postgres`). GREEN with `PEPICANVAS_REQUIRE_POSTGRES=1 go test -race -count=1 -v ./internal/platform/postgres/`: migrate twice, restart the server, reopen, migrate again, same schema version; an unreachable DSN fails without echoing its password. Full race suite passed. The new modules require `go 1.26.0` in `go.mod` (was 1.25); CI reads the version from `go.mod`. Migration 00001 creates `canvases` and `canvas_objects` with the S8 dimension checks and a unique z per canvas. CI sets `PEPICANVAS_REQUIRE_POSTGRES=1`, so a missing container runtime fails there instead of skipping.
