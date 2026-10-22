# US-007 canvas dimensions

Issue #17 (US-007). Branch `feat/us-007-canvas-dimensions`, worktree `/home/enzo/projects/pepicanvas-us-007`, base `4b173a8`.
Scope: owner-only resize of the canvas over an authenticated HTTP API, a dimensions form in /editor, and a shared DOM canvas surface sized by the confirmed dimensions (the OBS URL that will use it is US-021).

## Specs

- **S1 — Initial and inclusive limits.** Business rules: "Valor inicial 1920×1080." "Ancho y alto enteros; mínimo 320×180 y máximo 3840×2160, inclusivos." Criterion: "Valores en ambos límites y dentro del rango se aceptan y persisten en editor y OBS."
- **S2 — Invalid input.** Criterion: "Valores vacíos, fraccionarios, no numéricos, ancho <320, alto <180, ancho >3840 o alto >2160 se rechazan sin cambiar estado."
- **S3 — Absolute preservation.** Business rule: "Los objetos conservan posición y tamaño absolutos y pueden quedar fuera del área." Criteria: "Redimensionar no altera coordenadas, tamaño, rotación ni orden de objetos." "Objetos fuera del nuevo límite permanecen almacenados y vuelven a verse al ampliar." DoD: "Se verifica preservación absoluta con objetos dentro, parcial y totalmente fuera."
- **S4 — Owner only.** Business rule: "Solo el propietario cambia dimensiones." Criterion: "Moderadores y usuarios ajenos reciben rechazo seguro."
- **S5 — Concurrency.** Criterion: "Dos solicitudes concurrentes se aplican por orden confirmado del servidor; la última confirmada define las dimensiones."
- **S6 — Reconnection.** Criterion: "Una reconexión obtiene las dimensiones confirmadas, no valores almacenados en caché."
- **S7 — Isolation.** "Las pruebas usan dos canvas y confirman que cambiar uno no modifica ni revela el otro."
- **S8 — Definition of Done.** "Límites inclusivos, entradas inválidas, concurrencia, reconexión y autorización tienen pruebas." "OBS y editor muestran el mismo tamaño confirmado."
- **S9 — CTRL-CONS-01 (owner TE-012 closed: full).** "prueba de integración de US-007 (#17) confirma dominio y outbox en una transacción, fuerza una caída antes de publicar y verifica entrega posterior sin pérdida ni confirmación WebSocket fantasma."
- **S10 — CTRL-IDEM-01 (owner TE-007 closed: full).** "reintentos de US-007 (#17) antes y después de timeout o reconexión producen un único efecto y la misma respuesta; un payload distinto con el mismo operation ID se rechaza y la retención acotada se prueba en su vencimiento."
- **S11 — CTRL-WEB-01 (deferred to TE-014, route primitive).** "pruebas de US-007 (#17) cubren CSRF válido, faltante e inválido y Origin permitido, ausente y hostil, con rechazo antes del dominio, almacenamiento o upgrade WebSocket."
- **S12 — CTRL-TRACE-01 (deferred to TE-015).** "recorrido instrumentado de US-007 (#17) conserva una correlación consultable desde aceptación por HTTP/WebSocket hasta dominio, PostgreSQL, outbox, actor, S3 o subproceso aplicable, con redacción verificada." Own part: the request `correlation_id` reaches the log line, the retained operation, the outbox record, and the delivered hub event.
- **S13 — CTRL-RESTORE-01 (deferred to TE-017).** "una restauración de copias reales vinculadas a US-007 (#17) en entorno aislado verifica integridad PostgreSQL/assets, reconciliación y RPO/RTO observados sin exponer secretos." Own part: dimensions live only in the PostgreSQL `canvases` row.
- **S14 — CTRL-READY-01 (deferred to TE-016).** "pruebas de proceso de US-007 (#17) distinguen vida y preparación durante inicio, caída de dependencias y cierre, y confirman que no se acepta trabajo cuando no está preparado." Own part: with PostgreSQL down the resize answers 500 and changes nothing.
- **S15 — CTRL-SHUTDOWN-01 (deferred to TE-016).** "una prueba de señal con trabajo de US-007 (#17) en curso mide el drenaje, rechaza trabajo nuevo y demuestra ausencia de commits parciales, conexiones tardías, temporizadores o procesos huérfanos." Own part: each resize is one transaction, so an interrupted request commits all or nothing.
- **S16 — Surfaces (L2).** Owner changes dimensions through an authenticated HTTP API with CSRF and an operation ID, and a width/height form in the editor. The editor and the OBS overlay share one DOM canvas surface sized by the confirmed dimensions; the private OBS URL that feeds the overlay is US-021.

### Non-goals

Issue: "Reflujo, escalado global y formatos alternativos por canvas." Also: WebSocket transport (US-017), OBS private URL and its data feed (US-021), general CSRF/Origin middleware (TE-014), wiring the outbox publisher into the server process (no consumer until US-017).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S4 | inline, test-first: TE-001 policy rule owner administers canvas; moderators and others denied | pending | |
| T2 | S1, S2, S4, S7, S10, S11, S12, S16 | inline, test-first: `PUT /api/tenants/{tenantID}/canvases/{canvasID}/dimensions`, `Canvases` port gains `Apply`, httpapi fake tests | pending | |
| T3 | S1-S7, S9, S10, S12-S15 | inline, test-first: real-PostgreSQL tests of the route: limits, preservation, concurrency, restart, isolation, retention, outbox crash and delivery to the hub | pending | |
| T4 | S1, S2, S6, S16 | inline, test-first: frontend API client, dimensions form, shared `CanvasSurface`, Vitest | pending | |
| T5 | S1, S4, S6, S8, S16 | inline: Playwright end-to-end over the real server | pending | |
| T6 | all | docs (`docs/stack.md`), review assess, verification, per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-21): "empieza el desarrollo del issue 17 en su respectivo worktree"
- **L2** (user decisions, 2026-10-21): surface "API HTTP + formulario en editor"; OBS "Renderer compartido + diferir URL".
- **L3** (design, parent): (a) The domain already has `canvas.SetDimensions` with the inclusive bounds, and `postgres.CanvasStore.Apply` locks the canvas row, retains the operation ID for 10 minutes, and writes the outbox record in the same transaction (TE-004, TE-012). (b) Owner-only uses the TE-001 vocabulary: new rule `owner administer canvas`; moderators keep `edit` for objects. (c) Route order: Origin (middleware) -> session 401 -> CSRF 403 -> authorization from the path (403, before the body is read, so non-owners always get the same answer) -> decode 400 (empty, fractional, non-numeric, unknown fields) -> domain validation 400 -> `access.GuardMutation` reauthorizes right before `Apply` -> 200 `{canvas_id, width, height, revision}`; `ErrOperationConflict` 409; missing canvas 403. A replay returns the same body. (d) Reads stay `Cache-Control: no-store`, so a reconnecting editor gets the confirmed dimensions.
