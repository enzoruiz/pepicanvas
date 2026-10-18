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
| T1 | S1, S3, S4, S5, S8 | inline, test-first | done | `cb98881` |
| T2 | S6 | inline, test-first | done | `76151d0` |
| T3 | S2, S5 | inline, test-first | done | `ec73c03` |
| T4 | S7 | inline, test-first | done | `94ad40d` |
| T5 | all | inline adversarial review (verifier not admitted) + RDD assess | done | see Log L8 |

## Log

- **L1** (user, 2026-10-17): "empieza el desarrollo del issue 9 en su respectivo worktree"
- **L2** (user decision): selected "Núcleo + primitivas (recomendado)": actor core in `internal/realtime` stdlib-only, plus Start/Stop drain and correlation ID primitives; CONS, WEB, process READY, and operational TRACE documented for owners TE-012/014/015/016, as in TE-001 and TE-005.
- **L3** (parallel fence from roadmap analysis): TE-007 stays stdlib-only in `backend/internal/realtime` with no canvas persistence types, so it can run beside TE-003/TE-004.
- **L4** (T1 evidence): RED `go test -race ./internal/realtime` failed to build (`undefined: Command`, `Config`, `Hub`, `Key`). GREEN `ok .../internal/realtime 1.010s`; full `go test -race ./...` passed; `git diff --check` clean. Known gap handed to T4: `Close` drops queued commands and a waiting `Submit` with a non-expiring context blocks.
- **L5** (T2 evidence): RED build failure (`undefined: ErrOperationConflict`, unknown `Config.Now`/`Config.Retention`). GREEN `ok .../internal/realtime 1.034s`; full race suite passed. Decisions: the fingerprint covers kind, object, and payload but not the correlation ID; denials are not retained; domain rejections are; retention is valid while `now < accepted+TTL` and is also capped by count (defaults 10m, 4096). Slice note: T1 was 764 lines (about 300 implementation, the rest tests and tracker), above the ~400 advisory, kept as one behavior unit.
- **L6** (T3 evidence): RED build failure (`undefined: Fetcher`, `NewReplica`, `ErrForeignEvent`, `ErrInvalidEvent`). GREEN `ok .../internal/realtime 1.075s`; full race suite passed. Harness: 20 seeds, two tenants with the same canvas ID, three concurrent clients each with retries and deletes, three replicas per tenant with injected loss (20%), duplication (20%), delay past catch-up (10%), and shuffled order; every replica equals the authoritative state and rejects every foreign-tenant event. Mutation check: applying events without sequence buffering made the ordering and convergence tests fail, then the restored code passed. Tail loss is recovered by `CatchUp`, which transports call on reconnect or heartbeat; a cursor older than the log returns `ErrResyncRequired` (snapshot belongs to TE-008).
- **L7** (T4 evidence): RED under `testing/synctest`: "drain took 300ms, want 400ms" and "deadlock: main bubble goroutine has exited but blocked goroutines remain" (the old `Close` dropped queued work and orphaned waiting `Submit` calls). GREEN `ok .../internal/realtime 1.099s`; `-race -count=30` passed in 3.8s; full race suite passed. Contract: `Close` rejects new work with `ErrClosed` immediately, waits for admitted sends, drains queued commands (measured exactly 400ms for four 100ms commands), and on deadline cancels the authorizer context, answers queued work with `ErrClosed` without applying it (zero events), and returns only after every actor exits. Retained events stay readable after `Close`. No timers are used.
- **L8** (T5 evidence): `gentle_review assess` over `268988b..94ad40d` returned `risk: medium`, `reviewDue: false` (`under_budget`), `independentVerifier: false`; the `gentle-ai-verify` launch was refused by the admission gate and was not retried. Inline adversarial review of the concurrency paths: no send on a closed inbox (queues close only after `sending.Wait()`, and admission happens under the same lock that sets `closed`); one reply per request on a buffered channel; replay log and actor map read under their mutexes; dedup lookup runs after authorization; abort never applies after cancellation. Finding fixed: the `Authorizer` must honor ctx or `Close` cannot abort within its deadline; this is now documented on the port. Differential check: not applicable, because `internal/realtime` is a new package and no existing command or helper shares its code. Final: `go vet ./...` and `go test -race -count=1 ./...` passed; `git diff --check` clean.
- Per-S verdicts (self-verified): S1 PASS, S2 PASS, S3 PASS, S4 PASS, S5 PASS, S6 PASS, S7 PASS (actor scope), S8 PASS (passthrough scope). Deferred: CONS→TE-012, WEB→TE-014, READY and process SHUTDOWN→TE-016, operational TRACE→TE-015.
- Known limits: actors are never evicted (retained results and the log are bounded per canvas, not per hub); a replica cannot detect a lost tail without `CatchUp`; a replay cursor ahead of the hub (for example after a process restart) returns no events, and restart recovery belongs to persistence and resync.
