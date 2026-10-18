# TE-008 client resynchronization from authorized state

Issue #10 (TE-008). Branch `feat/te-008-client-resync`, worktree `/home/enzo/projects/pepicanvas-te-008`, base `c5feef1`.
Scope (L2): snapshot from PostgreSQL (`canvas.Store.Load`) with `revision` as cursor, a pure revision fold in `internal/canvas`, and `internal/resync` with the server service and a Go reference client replica.

## Specs

- **S1 — Resync to current confirmed state.** Issue: "Un cliente que detecta pérdida o discontinuidad obtiene el estado confirmado actual después de reautorizarse, sin restaurar estado efímero ni reproducir comandos históricos." The replica detects a revision gap and requests a snapshot; the snapshot is the committed PostgreSQL state at one revision, replaces the replica state, and is never rebuilt by replaying commands.
- **S2 — Event loss.** Issue: "Pérdida de uno y varios eventos." Losing one event or several consecutive or scattered events is detected as a discontinuity, and after resync the replica equals the committed state.
- **S3 — Authorized and revoked reconnection.** Issue: "Reconexión autorizada y revocada." and "un actor revocado no recibe ningún estado." Access is re-evaluated (TE-001) before reading and again immediately before delivery, under a live TE-002 lease; a revoked, deactivated, cut, or shut-down actor receives no state, and the store is not read when access is already denied.
- **S4 — Snapshot concurrent with mutations.** Issue: "Instantánea concurrente con mutaciones." A snapshot taken while mutations commit is one consistent revision; events at or below it are ignored, events after it are applied, and every replica converges.
- **S5 — OBS media stopped at zero.** Issue: "OBS con medios detenidos en cero." Every video and audio object in a snapshot is stopped at position zero; no playback state is restored.
- **S6 — No ephemeral state.** Issue non-goal: "Edición sin conexión o persistencia de reproducción, bloqueos, cursores y presencia." The snapshot carries no locks, cursors, presence, or playback beyond S5, and the replica drops buffered state on adoption.
- **S7 — Tenant isolation and convergence (DoD).** Issue: "Clientes convergen bajo todas las discontinuidades y un actor revocado no recibe ningún estado." Equal canvas IDs in two tenants never deliver each other's state or events.
- **S8 — CTRL-IDEM-01 (owner TE-007 closed: full proof).** Issue: "reintentos de TE-008 (#10) antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento." Resync has no durable effect: retries before and after timeout or reconnection return the same snapshot for the same revision and change no stored data; adopting a repeated or stale snapshot leaves the replica unchanged. Operation-ID collision and retention: `No aplica` with justification (a read carries no operation ID; the catalog excludes actions without durable effect), recorded in the issue closure.

### Deferred to their owners (rule 7, owners open)

- CTRL-CONS-01 → TE-012. Primitive: the snapshot reads only committed rows in one `RepeatableRead` transaction, and the event shape (`resync.Event`, numbered by revision) is the one TE-012 publishes.
- CTRL-WEB-01 → TE-014 (no HTTP/WebSocket ingress exists yet).
- CTRL-TRACE-01 → TE-015. Primitive: the snapshot carries the request's correlation ID from `platform/correlation`.
- CTRL-READY-01 → TE-016 (no process exists yet).
- CTRL-SHUTDOWN-01 → TE-016. Primitive: resync runs under the lease context, so a registry shutdown rejects new resyncs and cancels in-flight ones without delivering state.

### Non-goals

"Edición sin conexión o persistencia de reproducción, bloqueos, cursores y presencia." Also: transport (HTTP/WebSocket), frontend client code (US-018, US-022), outbox publication (TE-012).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1, S4 | inline, test-first: `canvas.Fold` applies one committed mutation result to a canvas | done | `3e91528` |
| T2 | S1, S2, S4, S6, S7, S8 | inline, test-first: `resync.Replica` (gap detection, buffering, duplicates, adoption, stale snapshots, latest-revision notice) | done | see L4 |
| T3 | S3, S5, S6, S7, S8 | inline, test-first: `resync.Service` (double authorization, lease guard, revocation, shutdown, correlation, media stopped at zero) | done | see L6 |
| T4 | S1-S8 | inline, test-first: PostgreSQL integration (concurrent snapshots under loss, two tenants, uncommitted invisible, retries after timeout and restart) | pending | |
| T5 | all | docs, review assess, verification, per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-18): "empieza el desarrollo del issue 10 en su respectivo worktree"
- **L2** (user decision): selected "PostgreSQL + revision (recomendado)": snapshot = `canvas.Store.Load` (RepeatableRead) with revision as cursor; Go reference client detects revision gaps, requests resync, and drops events at or below the snapshot. TE-012 later emits events with that revision.
- **L3** (T1 evidence): RED build failure (`undefined: canvas.Fold`). GREEN `go test -race -count=1 ./internal/canvas/`: each mutation kind folds to the expected state (dimensions keep objects, create on top, content keeps order, transform raises to the top, delete removes) without modifying the input; a revision gap or repeat, missing, pointer or invalid mutation, unknown object, missing or reused object ID, and missing or reused z return `ErrInvalid` and no state.
- **L4** (T2 evidence): RED build failure (`no non-test Go files in .../internal/resync`). First GREEN run failed on two test-fixture defects (generated rotation and dimensions left their bounds after step 360; the media fixture held no media at revision 6), fixed in the test. GREEN `go test -race -count=1 ./internal/resync/`: a new replica needs a snapshot; contiguous events follow every committed state; duplicates are ignored; reordered events wait for their gap; loss of one, several consecutive, several scattered, and everything after the head is reported at the revision before the first lost event and converges by adopting the current snapshot without replay; a lost tail is detected by `Notice`; snapshots concurrent with events (buffered before adoption, late at or below it, older than a buffered event) converge; repeated and stale snapshots change nothing; foreign events and snapshots return `ErrForeign` and change nothing; an inconsistent event leaves the replica needing resync; the buffer is capped at `DefaultWindow`; snapshots carry one stopped-at-zero playback entry per video and audio object only. Mutation checks: dropping `latest > revision` from `NeedsResync` failed 4 tests; dropping the stale-snapshot guard failed `TestRepeatedAndStaleSnapshotsChangeNothing`; both restored.
- **L5** (slice decision): the branch passed ~400 authored lines after T2. Delivery stays one PR reviewed commit by commit, one commit per task, as in TE-003, TE-004, and TE-007; each commit builds and passes on its own.
- **L6** (T3 evidence): RED build failure (`undefined: resync.Service`, `resync.NewService`; observed by removing the implementation file, since it was written before the first run). GREEN `go test -race -count=1 ./internal/resync/`: owner, moderator, and OBS get `NewSnapshot(committed, correlation ID)` with video and audio stopped at zero, after exactly 1 read between 2 access checks; no correlation in ctx gives none; inactive membership, inactive subscription, operator role, facts for another action, resource, or tenant, the equal canvas ID of another tenant, and a facts failure return `access.ErrAccessDenied` and read nothing; access revoked between read and delivery delivers nothing; a lease cut before the resync, during a read that honors cancellation, or during a read that ignores it returns `revocation.ErrAccessRevoked` and nothing; a stale-grant reconnection gets no lease and a fresh one resyncs; a released lease returns `ErrReleased`; registry shutdown rejects new resyncs and cancels one in flight with `ErrClosed`; an invalid key or nil lease reaches neither access nor the store; a missing canvas is `ErrNotFound`; a retry after a timeout and after reconnection returns the same snapshot. Every failure returns the zero Snapshot. Mutation checks: skipping the second authorization failed 2 tests; skipping the lease check before delivery failed the ignored-cancellation cut; skipping the facts-scope check failed 3 cases; all restored.
