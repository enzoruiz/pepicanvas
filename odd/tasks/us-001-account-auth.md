# US-001 account registration and sign-in

Issue #5 (US-001). Branch `feat/us-001-account-auth`, worktree `/home/enzo/projects/pepicanvas-us-001`, base `c5feef1`.
Scope: accounts, server-side sessions, the first HTTP server (`cmd/server`), the canvas-request access check, and minimal editor sign-up/sign-in UI.

## Specs

- **S1 — Registration.** Issue: "Dada una identidad no registrada y datos válidos, cuando completa el registro, entonces puede iniciar sesión." Identity is email + password (L2).
- **S2 — No duplicate account.** Issue: "Una identidad ya registrada no produce una segunda cuenta."
- **S3 — Invalid credentials.** Issue: "Credenciales inválidas no crean sesión ni revelan si existe un canvas asociado."
- **S4 — Reconnection and expiry.** Issue: "Una sesión válida sobrevive a una reconexión; una sesión vencida no se restaura desde caché." Business rule: "Una sesión vencida exige nueva autenticación." Expiry: 24 h idle, 7 days absolute (L2).
- **S5 — Authentication grants no canvas.** Issue: "Una cuenta autenticada sin suscripción activa ni moderación aceptada recibe rechazo al solicitar un canvas." Business rule: "La autenticación no concede acceso a ningún canvas por sí sola."
- **S6 — Safe errors.** Issue: "Los errores muestran un motivo seguro y no incluyen tokens, enlaces de invitación, credenciales OBS ni datos de otro usuario."
- **S7 — Two-tenant isolation and secret hygiene.** Issue: "Las pruebas usan dos tenants y confirman que iniciar sesión en uno no permite enumerar ni leer recursos del otro. Credenciales y tokens no aparecen en registros accesibles, errores visibles ni URLs de terceros."
- **S8 — Definition of Done.** "Registro, duplicado, login válido, login inválido, vencimiento, reconexión y resistencia a caché tienen pruebas automatizadas." "Las pruebas de dos tenants confirman ausencia de enumeración y datos cruzados." "Una revisión de registros y errores confirma ausencia de secretos."
- **S9 — CTRL-IDEM-01.** "reintentos de US-001 (#5) antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento."
- **S10 — CTRL-WEB-01 (early primitive, deferred to TE-014).** "pruebas de US-001 (#5) cubren CSRF válido, faltante e inválido y Origin permitido, ausente y hostil, con rechazo antes del dominio, almacenamiento o upgrade WebSocket."
- **S11 — CTRL-RATE-01 (early primitive, deferred to TE-014).** "pruebas deterministas de US-001 (#5) alcanzan el límite por cuenta, sesión, streamer o IP aplicable, observan 429 o cierre documentado, verifican recuperación e independencia entre streamers."
- **S12 — CTRL-TRACE-01 (deferred to TE-015).** "recorrido instrumentado de US-001 (#5) conserva una correlación consultable desde aceptación por HTTP/WebSocket hasta dominio, PostgreSQL, outbox, actor, S3 o subproceso aplicable, con redacción verificada." Own part: every request carries the TE-003 `correlation_id` into logs and the stored registration operation.
- **S13 — CTRL-RESTORE-01 (deferred to TE-017).** Own part: accounts and sessions live only in PostgreSQL, with no state outside the database.

### Non-goals

Issue: "Facturación, inicio social, MFA y recuperación de contraseña." Also: subscription activation and canvas creation (US-002), invitations (US-005), the general CSRF/Origin/rate-limit middleware and metrics (TE-014), WebSocket.

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1-S4, S9 | inline, test-first: `internal/access` account and session service with ports and injectable clock; argon2id adapter `platform/password` | done | see L4 |
| T2 | S1, S2, S4, S5, S7, S9, S13 | inline, test-first: migration 00003 (accounts, sessions, account registrations, tenants, memberships), sqlc queries, postgres adapters, restart and concurrency tests | pending | |
| T3 | S3, S5-S7, S10-S12 | inline, test-first: `platform/httpapi` routes, cookie, CSRF, Origin, login/registration rate limits, canvas access check, log redaction | pending | |
| T4 | S4, S8 | inline, test-first: `cmd/server` composition, env configuration, static frontend serving, process test | pending | |
| T5 | S1, S3, S4, S6, S8 | inline, test-first: editor sign-up/sign-in UI, API client, Vitest, Playwright end-to-end over the real server | pending | |
| T6 | all | docs (`docs/stack.md`), review assess, verification, per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-18): "empieza el desarrollo del issue 5 en su respectivo worktree"
- **L2** (user decisions, 2026-10-18): identity "Email + contraseña"; session expiry "Inactividad 24h + absoluto 7d"; CTRL-WEB-01/CTRL-RATE-01 "Primitiva + diferir" (US-001 implements a session-bound CSRF token, Origin check, and in-memory login limits for its own routes, and declares "Diferido a TE-014" for the general middleware and metrics); frontend "Sí, login/registro mínimos" (sign-up and sign-in screens in /editor plus a Playwright end-to-end test).
- **L3** (design, parent): (a) emails are trimmed and lower-cased, at most 254 bytes; passwords 12-128 characters; argon2id hashes in PHC string form. (b) Unknown email and wrong password produce the same `unauthorized` answer, and an unknown email still runs one argon2id verification against a dummy hash. (c) Registration is idempotent by `operation_id`: the retained row stores the account ID and the SHA-256 of the normalized email only; a replay is equivalent when the email matches and the password verifies against the account hash, so no fast password hash is ever stored. Retention is 10 minutes. A duplicate email under a new operation ID returns `conflict` (no email verification exists, so hiding it is not possible without breaking S1). (d) Sign-in is excluded from operation-ID replay: replaying would require retaining the session token, which TE-003 forbids (tokens are stored only as SHA-256); a retried sign-in creates an independent session that expires on its own. Sign-out is naturally idempotent. (e) Session cookie `__Host-pcs` (`HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`); the CSRF token is SHA-256 over a label and the session token, so it needs no storage and cannot be forged without the cookie. (f) Access facts for S5 come from minimal `tenants` and `memberships` tables; US-002 owns writing them, tests seed them. A denied canvas request and a missing canvas return the same `forbidden` answer.
- **L4** (T1 evidence): RED vet failures `undefined: access.AccountID`, `undefined: secret.KindPassword`, and `no non-test Go files in .../platform/password`. GREEN `go test -race -count=1 ./internal/access/ ./internal/secret/ ./internal/platform/password/`. Covered with fakes: S1 register then sign in with a normalized email; invalid email, password, and operation ID shapes leave no account; boundaries 254-byte email, 12 and 128 characters, 16 and 128-byte operation IDs accepted. S2 duplicate email (any case) returns `ErrIdentityTaken` and keeps the first password. S3 wrong password, unknown, malformed email, empty and oversized password all return the same `ErrInvalidCredentials` text, create no session, and run exactly one password verification. S4 idle expiry at 24 h (renewed by use) and absolute expiry at 7 d, expired sessions deleted; sign-out idempotent and per session; malformed tokens rejected. CSRF token bound to the session, missing, other-session, tampered, and raw session token rejected. S9 replay at retention-1ns returns the same account and keeps the first correlation ID; different email or password under a used ID returns `ErrOperationConflict`; at exactly 10 min the ID starts a new operation; a store failure leaves nothing and the retry applies once; 16 concurrent duplicates make one account. Mutation check: dropping the replay password comparison made `TestReusedOperationIDWithADifferentPayloadIsRejected` fail; restored. argon2id uses m=19456 KiB, t=2, p=1 (OWASP minimum), PHC strings, verification honors stored parameters and bounds them (m ≤ 256 MiB, t ≤ 10, p ≤ 16). `secret` gains kinds `password` and `csrf_token`; its unknown-kind test now uses `not_a_kind`. `golang.org/x/crypto` became a direct dependency (same v0.58.0).
