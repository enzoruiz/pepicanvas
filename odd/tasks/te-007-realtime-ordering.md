# TE-007 realtime ordering and deduplication

Issue #9 (TE-007). Branch `feat/te-007-realtime-ordering`, worktree `/home/enzo/projects/pepicanvas-te-007`, base `268988b`.
Scope approved in-session: core actor plus early primitives (drain contract, correlation passthrough); standard library only under `backend/internal/realtime`.

## Specs

- **S1 — Stable acceptance order.** Issue: "Mutaciones y comandos reciben un orden de aceptación estable por canvas y una identidad que evita aplicar duplicados." One actor per canvas assigns a gapless, monotonically increasing sequence to accepted commands only.
- **S2 — Fault convergence.** Issue: "Duplicación, reordenamiento, retraso y entrega perdida." Every authorized replica converges to the actor state under injected duplication, reordering, delay, and loss. Loss recovery replays retained events by sequence; a gap older than retention reports a resync requirement (snapshot is a non-goal).
- **S3 — Last accepted wins.** Issue: "Comandos concurrentes: el último comando aceptado prevalece."
- **S4 — Delete wins.** Issue: "Delete-wins frente a comandos posteriores."
- **S5 — Tenant isolation.** Issue: "Dos tenants con actividad simultánea." and "Todos los clientes autorizados convergen bajo fallas inyectadas y ningún evento cruza tenants." Actors are keyed by tenant and canvas; equal canvas IDs in two tenants never share order, dedup, or events. Authorization is re-evaluated immediately before acceptance (TE-001), fail-closed.
- **S6 — Idempotency (CTRL-IDEM-01, owned).** Issue: "reintentos de TE-007 (#9) antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento."
- **S7 — Drain primitive (early CTRL-SHUTDOWN-01).** Issue: "una prueba de señal con trabajo de TE-007 (#9) en curso mide el drenaje, rechaza trabajo nuevo y demuestra ausencia de commits parciales, conexiones tardías, temporizadores o procesos huérfanos." This slice proves the actor-level part: closing rejects new work, drains queued work, and leaves no goroutines or timers.
- **S8 — Correlation primitive (early CTRL-TRACE-01).** Issue: "conserva una correlación consultable desde aceptación por HTTP/WebSocket hasta dominio, PostgreSQL, outbox, actor". This slice carries a bounded correlation ID from command to result and event.

### Deferred to their owners (retroactive proof)

- CTRL-CONS-01 → TE-012 (outbox needs PostgreSQL; "persistencia física" is a TE-007 non-goal).
- CTRL-WEB-01 → TE-014 (no HTTP/WebSocket ingress exists yet).
- CTRL-READY-01 and process-level CTRL-SHUTDOWN-01 → TE-016 (no server process exists yet).
- Operational CTRL-TRACE-01 → TE-015 (no telemetry pipeline exists yet).

### Non-goals

"Instantánea de resincronización, bloqueos o persistencia física." Also: HTTP/WebSocket transport, canvas domain types, idle actor eviction.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1, S3, S4, S5, S8 | inline, test-first | done | see Log L4 |
| T2 | S6 | inline, test-first | pending | — |
| T3 | S2, S5 | inline, test-first | pending | — |
| T4 | S7 | inline, test-first | pending | — |
| T5 | all | independent verify (Risk item 4, concurrency) + RDD assess | pending | — |

## Log

- **L1** (user, 2026-10-17): "empieza el desarrollo del issue 9 en su respectivo worktree"
- **L2** (user decision): selected "Núcleo + primitivas (recomendado)": actor core in `internal/realtime` stdlib-only, plus Start/Stop drain and correlation ID primitives; CONS, WEB, process READY, and operational TRACE documented for owners TE-012/014/015/016, as in TE-001 and TE-005.
- **L3** (parallel fence from roadmap analysis): TE-007 stays stdlib-only in `backend/internal/realtime` with no canvas persistence types, so it can run beside TE-003/TE-004.
- **L4** (T1 evidence): RED `go test -race ./internal/realtime` failed to build (`undefined: Command`, `Config`, `Hub`, `Key`). GREEN `ok .../internal/realtime 1.010s`; full `go test -race ./...` passed; `git diff --check` clean. Known gap handed to T4: `Close` drops queued commands and a waiting `Submit` with a non-expiring context blocks.
