# US-020 temporary object locks during manipulation

Issue #20 (US-020). Branch `feat/us-020-object-locking`, worktree `/home/enzo/projects/pepicanvas-us-020`, base `f8615b1`.
Scope (L2): HTTP lock API (open lock session, heartbeat, acquire, release, list holders) behind the TE-014 ingress, lock sessions bound to TE-002 registry leases, a commit gate that US-008/US-009 call before `CanvasStore.Apply`, and a frontend lock client with the visible owner. No migration, no `go.mod` change. Read every section up to `## Log`.

## Specs

- **S1 — Protected operations.** Issue: "Mover, redimensionar, rotar, editar texto y eliminar requieren bloqueo." `SetTransform` (move, resize, rotate), `SetContent` (edit text), and `DeleteObject` commit only through the gate while the caller's lock session holds the object's lock.
- **S2 — Unprotected operations.** Issue: "La selección y la reproducción multimedia no requieren bloqueo." and "Seleccionar, reproducir/pausar/reiniciar video y reproducir audio funcionan sin adquirir bloqueo." `CreateObject`, `SetDimensions`, and every non-canvas-mutation command pass the gate without a lock; selection has no server effect.
- **S3 — One visible holder.** Issue: "El primer solicitante aceptado obtiene el bloqueo y se muestra su propietario.", "Dos solicitudes simultáneas producen un solo titular visible." and "No existen dos titulares efectivos." A protected commit in flight keeps the object unavailable to other sessions until it returns, even if the lease ends meanwhile.
- **S4 — Other participants.** Issue: "Otro participante no confirma operaciones protegidas sobre ese objeto, pero puede editar objetos distintos."
- **S5 — Finish and cancel.** Issue: "Se libera al completar o cancelar" and "Completar o cancelar libera inmediatamente."
- **S6 — Heartbeat.** Issue: "o 5 segundos después de la última señal de vida (heartbeat)." and "Perder señal de vida (heartbeat) libera exactamente al alcanzar 5 segundos sin señal de vida (heartbeat) aceptada."
- **S7 — Reconnect.** Issue: "Reconectar no restaura bloqueo." and "Desconexión no permite recuperar el bloqueo al reconectar." A lock session silent for 5 s is disconnected and its ID is rejected afterwards; a new session never gets the old locks.
- **S8 — Revocation and deactivation.** Issue: "Revocación o desactivación libera el bloqueo y rechaza confirmaciones posteriores."
- **S9 — Tenants.** Issue: "Un bloqueo de tenant A no es visible ni afecta tenant B."
- **S10 — Reauthorization and name.** Issue: "Cada acción de adquirir, renovar o liberar reautoriza al actor. El nombre mostrado es el de la cuenta autorizada y no contiene secretos."
- **S11 — Automatic release.** Issue: "El bloqueo desaparece sin intervención manual en todos los casos definidos."
- **S12 — CTRL-IDEM-01 (owner TE-007 closed: prove complete).** Issue: "CTRL-IDEM-01 — reintentos de US-020 (#20) antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento." Applies to acquire and release over HTTP; opening a lock session and the heartbeat are ephemeral presence and are excluded.
- **S13 — CTRL-WEB-01 (owner TE-014 closed: prove complete).** Issue: "CTRL-WEB-01 — pruebas de US-020 (#20) cubren CSRF válido, faltante e inválido y Origin permitido, ausente y hostil, con rechazo antes del dominio, almacenamiento o upgrade WebSocket."
- **S14 — CTRL-TRACE-01 (owner TE-015 open: defer with primitive).** Issue: "CTRL-TRACE-01 — recorrido instrumentado de US-020 (#20) conserva una correlación consultable desde aceptación por HTTP/WebSocket hasta dominio, PostgreSQL, outbox, actor, S3 o subproceso aplicable, con redacción verificada." Primitive: the request `correlation_id` reaches the lock log records, holder IDs redacted.
- **S15 — CTRL-READY-01 (owner TE-016 open: defer).** Issue: "CTRL-READY-01 — pruebas de proceso de US-020 (#20) distinguen vida y preparación durante inicio, caída de dependencias y cierre, y confirman que no se acepta trabajo cuando no está preparado." Primitive: a closed lock service answers a retryable unavailable error.
- **S16 — CTRL-SHUTDOWN-01 (owner TE-016 open: defer).** Issue: "CTRL-SHUTDOWN-01 — una prueba de señal con trabajo de US-020 (#20) en curso mide el drenaje, rechaza trabajo nuevo y demuestra ausencia de commits parciales, conexiones tardías, temporizadores o procesos huérfanos." Primitive: the lock-session directory and manager close inside the server shutdown, drain in-flight protected commits, and leave no timers.

### Non-goals

"Bloqueo persistente, bloqueo de canvas completo y reserva manual." Also: WebSocket transport (US-017), object mutation routes (US-008, US-009), playback commands (US-013).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1–S4, S9, S16 | inline, test-first: `locking` canvas listing, commit gate with in-flight fence, mutation classification | pending | |
| T2 | S6–S8, S10, S11, S16 | inline, test-first: lock-session directory bound to TE-002 leases, idle disconnect, per-account bound | pending | |
| T3 | S3–S10, S12–S15 | inline, test-first: member grant read, HTTP lock routes, `cmd/server` composition | pending | |
| T4 | S3, S5–S7 | inline, test-first: frontend lock client and visible owner | pending | |
| T5 | all | docs, independent verify, RDD assess | pending | |

## Log

- **L1** (user, 2026-10-28): "Start developing GitHub issue #20 [US-020] Bloquear temporalmente un objeto durante su manipulación in this worktree (/home/enzo/projects/pepicanvas-us-020, branch feat/us-020-object-locking, cut from origin/main f8615b1). Read it with gh issue view 20, plus docs/stack.md and docs/roadmap.md (stage 6). Prior groundwork: TE-009 lock leases is merged (odd/tasks/te-009-lock-leases.md); verify what it already covers before designing. Follow ODD: create the feature tracker in odd/tasks/ plus its Engram mirror, implement task by task with test-first and work-unit commits on this branch. Do not push, open a PR or merge until the user asks. Other gentle-shell sessions are developing #43 (TE-015), #24 (US-011), #20 (US-020) and #16 (US-006) in parallel in sibling worktrees: keep changes inside your issue scope, avoid touching shared files unnecessarily, and if you add a database migration warn that its number may collide with a parallel branch. Talk to the user in Spanish."
- **L2** (user decision, 2026-10-28): no WebSocket exists until US-017 (stage 9) and no object mutation route until US-008/US-009 (stages 7–8, both depend on US-020). The user chose "API HTTP de bloqueos + compuerta de commit": HTTP routes (acquire, heartbeat, release, holder lookup) with ingress `Session` and reauthorization; lock session bound to the TE-002 lease; a gate US-008/US-009 use before `CanvasStore.Apply`, proven with real mutations; frontend lock client and holder view. US-017 moves it to WebSocket.
- **L3** (TE-009 coverage, verified in `backend/internal/locking`): exclusive lease per tenant/canvas/object, denied acquire names the holder, exact 5 s expiry (timer plus lazy check), heartbeat renews every lease of a session and reports losses, connection-context detach, reauthorization on acquire and heartbeat, acquire/release idempotency per account with bounded retention, `Close`. Missing: listing a canvas's holders, a commit gate, transport, a display name, reauthorization on release.
- **L4** (design decisions): the displayed name is the account email (the only account name; shown only to members who pass TE-001 on that canvas); lock sessions are opaque random IDs, never tokens; the registry grant names the auth session by the hex SHA-256 of its token (the `sessions.token_hash` key), never the token.
