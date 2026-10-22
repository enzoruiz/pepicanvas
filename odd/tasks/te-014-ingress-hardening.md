# TE-014 HTTP and WebSocket ingress hardening

Issue #42 (TE-014). Branch `feat/te-014-ingress-hardening`, worktree `/home/enzo/projects/pepicanvas-te-014`, base `4b173a8`.
Scope: `net/http` ingress middleware (exact Origin, session-bound CSRF before the domain, closed CORS, security headers), WebSocket upgrade guard, in-memory limiters per applicable dimension with bounded metrics, wiring into `httpapi` and `cmd/server`, retroactive CTRL-WEB-01/CTRL-RATE-01 proof.

## Specs

- **S1 — Ingress middleware.** Issue: "HTTP mutante validará CSRF ligado a sesión, WebSocket validará Origin exacto antes del upgrade y las superficies de abuso aplicarán límites locales deterministas y observables." Deliverable: "Middleware net/http de CSRF, origen, cabeceras y CORS cerrado."
- **S2 — WebSocket upgrade guard.** Deliverable: "Validación exacta de Origin antes de cada upgrade WebSocket."
- **S3 — Limiters and metrics.** Deliverable: "Limitadores en memoria por dimensiones aplicables con respuestas y métricas acotadas." DoD: "Límites, recuperación y métricas son deterministas."
- **S4 — Test matrix.** Deliverable: "Matriz de pruebas de origen, CSRF, abuso y aislamiento." DoD: "CSRF y Origin cubren casos válidos, faltantes, inválidos y hostiles." and "Rechazos ocurren antes de dominio, archivos o upgrade."
- **S5 — CTRL-WEB-01 (owned).** "CTRL-WEB-01 — Propio: pruebas de integración para Origin permitido, ausente y hostil y para token CSRF válido, faltante e inválido, con rechazo antes de ejecutar dominio, escribir archivos o abrir el WebSocket."
- **S6 — CTRL-RATE-01 (owned).** "CTRL-RATE-01 — Propio: pruebas deterministas del límite y su ventana, respuesta 429 o cierre documentado, recuperación al vencer la ventana, métricas sin dimensiones no acotadas e independencia entre streamers."
- **S7 — Isolation between streamers.** Issue: "límites y rechazos de una cuenta no autorizan ni consumen el cupo lógico de otra salvo la dimensión IP declarada." and "CSRF, Origin y rate limit se ejecutan antes del dominio sin sustituir la matriz TE-001." DoD: "Dos streamers demuestran aislamiento y redacción."
- **S8 — Failures, cleanup, safe disable.** "Falla antes de confirmar: solicitud o upgrade se rechaza sin efecto lateral." "Limpieza: conexiones rechazadas no dejan goroutines, archivos ni reservas." "Reversión o desactivación segura: la configuración inválida mantiene readiness negativa; no habilita CORS comodín ni omite controles."
- **S9 — CTRL-IDEM-01.** "CTRL-IDEM-01 — reintentos de TE-014 antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento."
- **S10 — Deferred controls.** "CTRL-TRACE-01 — Diferido a TE-015: cada rechazo registra motivo, ruta y `correlation_id` (TE-003), con cardinalidad controlada y redacción verificada." CTRL-READY-01 ("pruebas de proceso de TE-014 distinguen vida y preparación durante inicio, caída de dependencias y cierre, y confirman que no se acepta trabajo cuando no está preparado.") owner TE-016 is open → deferred with the primitive: invalid configuration makes the process exit before listening.
- **S11 — Retroactive proof.** DoD: "Las pruebas retroactivas de CTRL-WEB-01 y CTRL-RATE-01 cubren, o justifican como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-001, TE-003, TE-005 y TE-007." Computed at start (L3): WEB: TE-001, TE-002, TE-003, TE-007, TE-008, TE-009, US-001, US-002; RATE: TE-005, TE-006, US-001.

### Non-goals

Trusted proxy headers (client IP stays the TCP peer; a proxied deployment needs an explicit later decision); multi-origin deployments and non-browser clients (CTRL-WEB-01 exclusions); distributed limiter, IP reputation, CAPTCHA, DDoS (CTRL-RATE-01 exclusions); a WebSocket endpoint or `coder/websocket` dependency (no route exists; stage rule 1); metric export and trace queries (TE-015); readiness endpoint (TE-016).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1-S8, S10 | delegated worker (context backstop), test-first against the parent's RED `ingress_test.go`: `platform/ingress` exact origins, closed CORS, security headers, route classes, CSRF before the domain, rejection log, bounded counters, limiter dimensions (IP, session, account), full-table reason, WebSocket upgrade guard, rejected-upgrade goroutine test | in progress | |
| T3 | S4, S5, S6, S7, S9 | inline, test-first: wire `httpapi` and `cmd/server` (API and static headers), move CSRF before `Authenticate`, IDEM retry test | pending | |
| T4 | S11, S10 | inline: retroactive proof, `docs/stack.md` entry | pending | |
| T5 | all | assess, independent verify (high risk: security), per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-21): "empieza el desarrollo del issue 42 en su respectivo worktree"
- **L2** (design, parent decision): (a) New package `internal/platform/ingress`; `httpapi` registers every route with a class (`Read`, `Anonymous` unsafe with Origin only, `Session` unsafe with Origin plus CSRF), so an unsafe route cannot exist without a class. (b) For `Session` routes the CSRF token is checked against the session cookie before routing reaches `Authenticate` (which reads and may delete sessions); without a cookie there is no ambient credential and the handler answers 401. Consequence: a stale cookie with a bad CSRF token now gets 403 instead of 401/204. (c) CORS stays closed: no `Access-Control-*` header is ever written and preflights are 403. (d) Metrics are in-memory counters over closed enums (surface × reason) with a `Snapshot`; no new dependency. (e) Client IP stays the TCP peer; forwarding headers are ignored and tested as ignored. (f) No WebSocket route exists, so the upgrade guard is proven with a stub upgrade handler.
- **L4** (T1 RED, parent): T1 and T2 merged (one package, one test file). `backend/internal/platform/ingress/ingress_test.go` written first; `go vet ./internal/platform/ingress/` RED `no non-test Go files in .../platform/ingress`.
- **L3** (retroactive list, computed 2026-10-21 from closed issue bodies and trackers): WEB: TE-001 (#1), TE-002 (#7), TE-003 (#4), TE-007 (#9), TE-008 (#10), TE-009 (#11), US-001 (#5), US-002 (#12). RATE: TE-005 (#2), TE-006 (#8), US-001 (#5). Recompute before closing.
