# TE-012 durable mutations through a PostgreSQL outbox

Issue #40 (TE-012). Branch `feat/te-012-outbox`, worktree `/home/enzo/projects/pepicanvas-te-012`, base `5756810`.
Scope: outbox table and sqlc queries, outbox event written inside `CanvasStore.Apply`, retrying publisher in `internal/outbox` delivering to the realtime actor, observability, startup recovery, retroactive CTRL-CONS-01 proof.

## Specs

- **S1 — Atomic domain and outbox.** Issue: "Las mutaciones durables confirmarán el estado de dominio y su evento de outbox en la misma transacción PostgreSQL" and DoD "Cada mutación aplicable escribe dominio y outbox atómicamente." Deliverables: "Migración y consultas sqlc para outbox y estado de entrega." and "Puerto transaccional que escribe dominio y evento juntos." Failure: "Falla antes de confirmar: no quedan dominio ni evento confirmados."
- **S2 — Retrying publisher to the actor.** Issue: "un publicador reintentable entregará luego al actor del canvas y expondrá pendientes y antigüedad." Deliverable: "Publicador reintentable al actor con espera acotada y observabilidad de pendientes."
- **S3 — Crash between commit and publish; startup recovery.** Issue: "Falla después de confirmar: el evento permanece pendiente y se reintenta tras reinicio." Deliverable: "Recuperación al iniciar sin Redis, Kafka ni otro broker." DoD: "La caída entre commit y publicación se recupera sin pérdida." CTRL-CONS-01 (owned): "una prueba de integración con PostgreSQL real fuerza una caída entre commit y publicación, reinicia el publicador y demuestra entrega posterior sin pérdida ni confirmación WebSocket fantasma; pendientes y antigüedad son consultables."
- **S4 — Observability.** DoD: "Pendientes, antigüedad y reintentos son observables."
- **S5 — CTRL-IDEM-01.** Issue: "la reentrega del publicador no repite efectos en el actor del canvas: un evento ya aplicado se reconoce por su identidad y produce la misma respuesta." Verification line: "reintentos de TE-012 antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento."
- **S6 — Isolation between streamers.** Issue: "la outbox conserva el streamer y canvas autorizados por la operación original; el publicador no reautoriza hacia otro tenant." and "dos cuentas generan eventos simultáneos y ninguna recibe ni consulta pendientes de la otra."
- **S7 — Cleanup and safe stop.** Issue: "Limpieza: entregas confirmadas se retienen o purgan con política acotada y observable." and "Reversión o desactivación segura: detener el publicador conserva pendientes y fuerza resincronización; no revierte commits confirmados."
- **S8 — Deferred primitives.** CTRL-TRACE-01 (→ TE-015): "cada evento de outbox conserva el `correlation_id` de la mutación que lo originó y lo registra al publicarse." CTRL-READY-01 (→ TE-016): "el publicador expone si está operativo y el tamaño de su cola pendiente, para que TE-016 lo use en la preparación." CTRL-SHUTDOWN-01 (→ TE-016): "el publicador expone un cierre con el contrato de `Hub.Close` y deja reintentable todo evento no entregado." CTRL-RESTORE-01 (→ TE-017): "sin primitiva propia".
- **S9 — Retroactive CTRL-CONS-01 proof.** DoD: "La prueba retroactiva de CTRL-CONS-01 cubre, o justifica como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-004, TE-005 y TE-007." Issue: "TE-005 y TE-007 (cerrados), TE-004 (cierra antes por dependencia) y todo consumidor cerrado con `CTRL-CONS-01 — Diferido a TE-012` antes de cerrar TE-012." Computed list at start (L3): TE-002, TE-004, TE-005, TE-007, TE-008.

### Non-goals

Exactly-once delivery and multi-instance coordination (docs/stack.md CTRL-CONS-01 exclusions: "no se promete entrega exactamente una vez ni coordinación entre múltiples instancias"); WebSocket transport and HTTP routes; outbox for members, subscriptions, or OBS credentials (their stories wire their own mutations into this port); process readiness and signal handling (TE-016); restore drills (TE-017); operational trace queries (TE-015).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1, S6, S8 (TRACE) | inline, test-first: migration `00005_canvas_outbox.sql`, sqlc queries, event codec, `Apply` writes the event in its transaction, commit hook | done | see L4 |
| T2 | S2, S4, S7, S8 (READY, SHUTDOWN) | inline, test-first: `internal/outbox` publisher (per-canvas head-of-line, bounded backoff, attempt timeout, purge, stats, `Close`), PostgreSQL outbox store | pending | |
| T3 | S3, S5, S6 | inline, test-first: hub sink adapter, crash-between-commit-and-publish and restart test, redelivery idempotence, two-tenant isolation | pending | |
| T4 | S9, all | retroactive proof, docs, assess, verification, per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-18): "empieza el desarrollo del issue 40 en su respectivo worktree"
- **L2** (design, parent decision):
  (a) Table `canvas_outbox`: identity `id`, `(tenant_id, canvas_id)` FK to `canvases` ON DELETE CASCADE, `revision` unique per canvas, `operation_id`, nullable `correlation_id`, `kind`, `object_id`, `z`, `mutation jsonb`, `created_at`, `attempts`, `next_attempt_at`, `last_error`, `settled_at`, `outcome` (`delivered` | `rejected`). Inserted inside the `Apply` transaction after the mutation; a replayed operation inserts nothing.
  (b) Event shape is TE-008's `resync.Event` (mutation, result, correlation ID, numbered by revision). Per canvas, rows are inserted under the canvas row lock, so `id` order equals revision order.
  (c) Publisher: each pass reads the oldest pending event of every canvas (`DISTINCT ON`), delivers those due, and repeats while it makes progress; a failing event blocks only later events of its canvas. Retry with exponential backoff capped at a maximum delay, and a timeout per attempt. Wake-up through a commit hook on `CanvasStore` plus a poll interval, which also covers startup recovery.
  (d) Sink contract: nil settles `delivered`; an error wrapping `outbox.ErrUndeliverable` settles `rejected` (never retried, counted); any other error is retried. Hub adapter: operation ID `canvas-revision-<n>` (stable per canvas, so the hub's dedup recognizes redelivery even when the original operation ID is later reused); `ErrInvalidCommand`, `ErrOperationConflict`, and `access.ErrAccessDenied` are undeliverable; an actor `rejected` outcome is a delivered answer.
  (e) Purge of settled rows older than a retention (default 24h) in bounded batches; purge count observable.
  (f) Migration number 00005: US-002 (in progress, `feat/us-002-manual-activation`) already uses 00004.
- **L3** (S9 list): closed issues with a CTRL-CONS-01 line at start: TE-002 (#7, deferred), TE-004 (#6, deferred), TE-005 (#2), TE-007 (#9), TE-008 (#10) (rule 9 / pre-rule lines). Recomputed at close.
- **L4** (T1 evidence): RED build failure (`no non-test Go files in .../internal/outbox`); codec RED `cannot set embedded pointer to unexported struct type` fixed by a named `transform` field. GREEN `PEPICANVAS_REQUIRE_POSTGRES=1 go test -race -count=1 ./...` (all packages ok). Coverage: S1 `TestApplyWritesOneOutboxEventPerAcceptedMutation` (all five kinds; each record equals `resync.Event{Key, Mutation, Result, CorrelationID}`, revision 1..5, increasing IDs, `created_at` = acceptance clock, attempts 0, commit hook once per record), `TestOutboxIgnoresReplaysConflictsAndRejectedMutations` (replay, conflict, not found, invalid write nothing and do not run the hook), `TestFailureBeforeCommitLeavesNoOutboxEvent` (no record, no domain change, no hook; retry writes one); S6 `TestOutboxPendingIsScopedToTheCanvasKey` (two tenants, same canvas ID, concurrent writers; each sees only its five records in revision order; malformed key `ErrInvalid`; limit); S8 TRACE primitive: `correlation_id` stored per record. Codec tests: round trip of every kind (rotation `12.345678901234567` exact through `jsonb`), stable kind names, `ErrMalformed` for nil, pointer, unknown kind, bad JSON, unknown field, invalid mutation, trailing data. Mutation check: forcing `replay=false` made `TestOutboxIgnores...` fail (`commit hook ran 2 times, want 1`); restored. `scripts/sqlc.sh diff` clean.
