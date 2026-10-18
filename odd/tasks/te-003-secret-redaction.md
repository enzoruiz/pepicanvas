# TE-003 secret redaction

Issue #4 (TE-003): keep invitations, session tokens, media URLs, and OBS credentials out of every forbidden surface.
Branch `feat/te-003-secret-redaction`, worktree `/home/enzo/projects/pepicanvas-te-003`, base `268988b`.
Read every section up to `## Log`; specs quote the issue and stack contract verbatim.

## Specs

- S1 (deliverable): "Invitaciones, tokens de sesión, URLs de medios y credenciales OBS quedan excluidos de errores visibles, registros accesibles, analítica, encabezados Referer y URLs de terceros, con respuestas no cacheables donde existe acceso privado."
- S2 (evidence): "Escaneo automatizado de registros y errores con secretos señuelo."
- S3 (evidence): "Inspección de solicitudes salientes y cabeceras de caché."
- S4 (evidence): "Prueba de back/refresh/cache después de corte de acceso."
- S5 (evidence): "Rotación de señuelo sin aparición en evidencia accesible."
- S6 (definition of done): "Ningún secreto señuelo aparece en superficies prohibidas y el contenido privado no se recupera desde caché después del corte."
- S7 (non-goal): "Autorización de recursos o cierre de conexiones."
- S8 (stack contract, `docs/stack.md`): "Logs estructurados, con un handler que redacta secretos (TE-003)"; "Los medios se sirven a través del backend. No se exponen URLs firmadas del almacenamiento. Las respuestas privadas usan `Cache-Control: no-store` y `Referrer-Policy: no-referrer` (TE-003)."
- S9 (cross-cutting controls, user decision L2): TE-003 delivers the early primitive only: a correlation identifier carried through context into logs, with redaction verified on correlated records. CTRL-WEB-01 ("pruebas de TE-003 (#4) cubren CSRF válido, faltante e inválido y Origin permitido, ausente y hostil, con rechazo antes del dominio, almacenamiento o upgrade WebSocket") and the end-to-end path of CTRL-TRACE-01 ("recorrido instrumentado de TE-003 (#4) conserva una correlación consultable desde aceptación por HTTP/WebSocket hasta dominio, PostgreSQL, outbox, actor, S3 o subproceso aplicable, con redacción verificada") are proven later by their owners TE-014 and TE-015 on top of TE-003.

## Tasks

- [x] SEC-01 — Secret value types that redact on every formatting path (string, `%v`/`%+v`/`%#v`, JSON, text, `slog.LogValuer`) for the four secret kinds. S1, S8. Route: inline. Commit: see Log L4.
- [x] SEC-02 — Redacting `slog` handler plus correlation-ID primitive; decoy scan over log output, including secrets embedded in free text and error values. S1, S2, S8, S9. Route: inline. Commit: see Log L5.
- [x] SEC-03 — HTTP private-response middleware (`Cache-Control: no-store`, `Referrer-Policy: no-referrer`) and safe error responder that never reflects secrets. S1, S3, S6, S8. Route: inline. Commit: see Log L6.
- [ ] SEC-04 — Browser surfaces: referrer policy on editor and overlay documents, no third-party or analytics requests, and back/refresh/cache test after access cut. S1, S3, S4, S6. Route: inline. Commit: pending.
- [ ] SEC-05 — Decoy rotation harness: fresh decoys per run, scan of every collected surface, wired into CI, docs updated. S2, S5, S6. Route: inline. Commit: pending.

## Log

- L1 (user, 2026-10-17): "empieza el desarrollo del issue 4 en su respectivo worktree"
- L2 (user decision, 2026-10-17): for CTRL-WEB-01 and CTRL-TRACE-01 the user chose "Primitiva temprana (recomendado)": TE-003 creates the correlation ID and verifies redaction on correlated logs; CSRF/Origin tests and the full trace move to TE-014/TE-015, which validate them later on top of TE-003.
- L3: no HTTP server, session, or logging code exists at base `268988b`; TE-003 builds primitives that later product issues consume.
- L4 (SEC-01): RED was a build failure (no `secret` package); GREEN `go test -race ./internal/secret` ok, covering 22 formatting/serialization paths for all four kinds, including an unexported struct field (payload behind a pointer prints as an address). Full `go test -race ./...` ok. Commit: `feat(secret): add redacting secret value type`.
- L5 (SEC-02): design decision: generated tokens carry kind prefixes (`pci_`, `pcs_`, `pco_` + 32 random bytes base64url) so `secret.Scrub` can redact them inside free text; `Scrub` also strips URL userinfo, query, and fragment (signed storage URLs). The `logging` handler scrubs messages and string/any/error attributes, fully redacts keys containing authorization/cookie/credential/password/secret/token, and appends `correlation_id` from context (`platform/correlation`, canonical 32-hex IDs only). RED: undefined symbols/packages. GREEN: focused race tests ok; mutation (handler bypassed) reported 10 leaks; full `go test -race ./...` ok. Known limit: a plain string holding a non-prefixed secret under a non-sensitive key is not detectable; secrets must travel as `secret.Value`. Commit: `feat(logging): redact secrets and attach correlation IDs`.
- L6 (SEC-03): `platform/httpx` adds `Private` (reapplies `Cache-Control: no-store` and `Referrer-Policy: no-referrer` at WriteHeader so handlers cannot weaken them), `Correlate` (fresh ID per request in context and `X-Correlation-ID`; inbound IDs ignored until TE-015 defines trust), `Responder.Error` (closed `ErrorCode` vocabulary, fixed status, JSON body with code and correlation only, internal error logged through the redacting handler), and `Recover` (generic 500, re-panics `http.ErrAbortHandler`). RED: package missing. GREEN: focused race tests ok with decoys in error text, request path, and query; mutation removing the WriteHeader reapply failed the override and error cases; full `go test -race ./...` ok. Commit: `feat(httpx): add private response and safe error primitives`.
