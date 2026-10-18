# TE-002 access cut and connection closure

Issue #7 (TE-002). Branch `feat/te-002-access-cut`, worktree `/home/enzo/projects/pepicanvas-te-002`, base `beee2b1`.
Scope: stdlib-only core in `backend/internal/revocation` (roadmap stage 1, rule 1: TE-004 owns this stage's `go.mod` changes), with a connection port the future WebSocket adapter implements. Read every section up to `## Log`.

## Specs

- **S1 — Confirmed cut.** Issue: "Un mecanismo común aplica revocación, desactivación y rotación desde la confirmación del servidor y cierra conexiones afectadas en ≤2 segundos." One registry applies the three cut types; a cut is applied only when it carries a server confirmation (a nonzero confirmed version).
- **S2 — Close clock.** Issue: "Reloj de confirmación y cierre para editor y OBS." Every affected editor and OBS connection is closed within 2 s of the confirmed cut reaching the registry; a connection that does not close gracefully in time is aborted; the cut reports each close latency.
- **S3 — Confirmation order.** Issue: "Carreras ordenadas por confirmación." A cut affects only grants observed at an earlier confirmed version, regardless of arrival order.
- **S4 — Rejection after cut.** Issue: "Rechazo de lecturas, escrituras y comandos no confirmados." Once a cut is applied, every read, write, and command through an affected connection is rejected, even before its transport finishes closing; an unconfirmed cut is rejected as invalid.
- **S5 — No restore.** Issue: "Reconexión y caché no restauran acceso." Registering again with a grant observed before the cut is rejected; a grant older than the retained cut history is rejected as stale.
- **S6 — Two tenants.** Issue: "Todos los tipos de corte cumplen ≤2 segundos bajo prueba concurrente de dos tenants; cualquier exceso falla." Concurrent cuts of every type in two tenants close within 2 s, never touch the other tenant, and any excess fails the test.
- **S7 — Idempotency (CTRL-IDEM-01).** Issue: "reintentos de TE-002 (#7) de una misma revocación, desactivación o rotación producen un único corte y la misma respuesta; un payload distinto con el mismo identificador de operación se rechaza."
- **S8 — Shutdown primitive (CTRL-SHUTDOWN-01, deferred to TE-016).** Issue: "el registro de conexiones expone un cierre con el contrato de `Hub.Close` de TE-007: rechaza conexiones nuevas, cierra las existentes con motivo reintentable dentro del presupuesto y no deja goroutines ni temporizadores."
- **S9 — Correlation primitive (CTRL-TRACE-01, deferred to TE-015).** Issue: "la confirmación y el cierre de cada conexión afectada registran el mismo `correlation_id` (TE-003), con redacción verificada."

### Deferred to their owners

- CTRL-CONS-01 → TE-012: this slice enforces only "the cut applies from a confirmation"; the outbox and commit-to-registry delivery belong to TE-012.
- CTRL-WEB-01 → TE-014; CTRL-RESTORE-01 → TE-017: no own primitive.

### Non-goals

"Definir la matriz de permisos o generar credenciales." Also: WebSocket transport, HTTP routes, persistence of cuts.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1–S6 | inline, test-first | done | see L3 |
| T2 | S7 | inline, test-first | pending | |
| T3 | S8, S9 | inline, test-first | pending | |
| T4 | all | docs, independent verify, RDD assess | pending | |

## Log

- **L1** (user, 2026-10-18): "empieza el desarrollo del issue 7 en su respectivo worktree"
- **L2** (scope from roadmap): stage 1 marks TE-002 "Paralelo seguro" beside TE-004, and rule 1 allows only one issue per stage to add dependencies, so TE-002 stays stdlib-only, like TE-007.
- **L3** (T1 evidence): RED `go test -race ./internal/revocation` failed to build (`undefined: Reason`, `Registry`, `Grant`, `Conn`, `Lease`). GREEN `ok .../internal/revocation 1.010s`; `-count=3` passed; full `go test -race ./...` passed. Mutation: dropping the `grant.Version < cut.Version` check failed `TestCutsApplyInConfirmationOrderNotArrivalOrder`. Decisions: the clock starts when the confirmed cut reaches `Apply` (commit-to-registry delay belongs to TE-012); graceful close gets 3/4 of the 2 s budget, then `Abort`; any close over budget returns `ErrBudgetExceeded` with the full report; cut history is per tenant (default 4096) and eviction raises a per-tenant floor that makes older grants `ErrStaleGrant`; every cut is tenant-scoped, so a session revocation names its tenant.
