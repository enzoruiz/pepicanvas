# TE-010 remote render latency

Issue #36, branch `feat/te-010-remote-render-latency`, worktree `/home/enzo/projects/pepicanvas-te-010`, base `dbee2ac` (origin/main).
Scope chosen by the user (L2): measurement instrument plus a live harness on the real TE-007 hub and the real Chrome/Edge matrix, with a harness-only stdlib transport (SSE + fetch) and no new dependencies.
Stage 5 rules: no new dependency in `backend/go.mod` or `frontend/package.json` without asking; do not merge; commit and push the branch allowed; no PR until asked.

## Specs

- **S1 — Correlated samples per transmission.** "Cada transmisión obtiene al menos 100 mediciones correlacionadas desde la aceptación del servidor hasta el renderizado remoto de crear, mover, redimensionar, rotar, eliminar y ejecutar comandos multimedia."
- **S2 — Sample validity.** "Cada muestra registra un cliente compatible, cero desconexiones del cliente durante la muestra y mediciones de RTT de red cuyo p95 para esa muestra es ≤100 ms."
- **S3 — Upload excluded.** "Se excluye el procesamiento inicial de la carga."
- **S4 — Per-transmission p95.** "El p95 se calcula por transmisión sobre ≥100 muestras."
- **S5 — Inconclusive.** "La evidencia insuficiente se marca como inconclusa, nunca como aprobada."
- **S6 — Recalculable without secrets.** "El conjunto de evidencia permite recalcular la cantidad, el RTT y el p95 de cada transmisión sin datos secretos."
- **S7 — Non-goals.** "SLA contractual o medición de carga inicial de archivos."
- **S8 — CTRL-TRACE-01 (deferred).** "CTRL-TRACE-01 — Diferido a TE-015 (TE-015 depende de TE-010): cada muestra aceptación→render conserva el `correlation_id` de TE-003 y la correlación que TE-007 transporta del comando al evento; TE-015 la vuelve consultable a través de persistencia, publicación, cliente remoto y render."
- **S9 — CTRL-EVID-01 (deferred).** "CTRL-EVID-01 — Diferido a TE-015 (TE-015 depende de TE-010): cada muestra emite release, escenario, marcas de tiempo y correlación suficientes para recalcular la latencia; TE-015 la conserva de forma durable, íntegra y relegible."
- **S10 — CTRL-VERS-01 (owner TE-011 closed: full).** "CTRL-VERS-01 — el manifiesto de TE-010 (#36) fija release, Go, módulos, Node.js, gestor de paquetes, dependencias frontend, PostgreSQL, almacenamiento S3, ffmpeg/ffprobe, navegadores y OBS exactos; cualquier diferencia obliga a repetir la evidencia afectada."

## Design decisions (implementation, see Log L3)

- Acceptance instant: `realtime.Hub.Submit` returned `accepted` on the harness server (server monotonic clock, ns since process start).
- Render instant: the remote page applies the event to `CanvasSurface` (or the media element) and takes the second `requestAnimationFrame` after the DOM commit (or after the media `playing`/`pause` event), on `performance.now()`.
- Clock mapping: each sample carries ≥5 pings `{clientSentMs, serverAtNs, clientReceivedMs}` (3 before the command, 2 after the render); the offset comes from the minimum-RTT ping; latency = `renderedAtMs + offset − acceptedAtNs/1e6`.
- p95 is nearest-rank (`ceil(0.95·n)`-th smallest), per sample over its RTTs and per transmission over valid latencies.
- Results are `MEASURED` or `INCONCLUSIVE`; there is no latency threshold (S7).
- Media commands play/pause one audio element whose WAV source is generated in the page and loaded before measurement starts; samples accepted before `measurementStartNs` are invalid (S3).
- The TE-010 manifest (evidence `versions`) records release, Go, Node.js, npm, the Go modules from `go.mod`, the frontend packages from `package-lock.json`, PostgreSQL, S3, ffmpeg/ffprobe, the six browser/OBS clients, and the SHA-256 of the release manifest; `latencycheck` compares it with the current release manifest and repository files, and any difference makes the transmission `INCONCLUSIVE`.

## Tasks

- [x] T0 — Read issue, roadmap, stack; review control lines (S8-S10). Route: inline. Commit: this document.
- [ ] T1 — `backend/internal/latency`: strict evidence decode and evaluator (S1-S6, S8 primitive, S10 comparison). Route: inline, test-first. Commit: —
- [ ] T2 — `backend/cmd/latencycheck`: CLI that loads the release manifest and repository pins, evaluates evidence files, prints per-transmission results, exit codes (S5, S6, S10). Route: inline, test-first. Commit: —
- [ ] T3 — `backend/cmd/latencyprobe`: loopback harness server with the real hub, SSE events, ping, commands, static probe page (S1, S3, S8). Route: inline, test-first. Commit: —
- [ ] T4 — Frontend probe page and latency primitives (render mark, pings, event stream disconnects), separate Vite config (S1-S3). Route: inline, test-first (Vitest). Commit: —
- [ ] T5 — Playwright latency runner over the browser matrix writing evidence and validating it with `latencycheck`; real run (S1-S6, S10). Route: inline. Commit: —
- [ ] T6 — Docs (`docs/stack.md`), close-out checks, native review per commit if due. Route: inline. Commit: —

## Log

- **L1 (user request, verbatim):** "Empezá el desarrollo del issue #36 [TE-010] Medir latencia desde aceptación hasta render remoto, en la rama feat/te-010-remote-render-latency. Estás en un worktree dedicado sobre origin/main; trabajá solo acá y en tu rama. Leé el issue completo con gh issue view, docs/roadmap.md y docs/stack.md antes de tocar código. Reglas de la etapa 5 (en paralelo están US-005 #15, US-016 #19 y TE-010 #36): (1) solo un issue de la etapa puede agregar dependencias a backend/go.mod o frontend/package.json; si necesitás agregar una, frená y preguntame antes. (2) Revisá las líneas de controles del issue según las reglas 6 a 8 de docs/stack.md: control con propietario cerrado se demuestra completo; propietario abierto se declara Diferido a. La etapa 4 todavía tiene issues abiertos (US-003, US-007, TE-013, TE-014): no mergees nada; podés commitear y pushear tu rama, pero no abras PR hasta que te lo pida."
- **L2 (user decision):** "Instrumento + arnés en vivo (recomendado)": sample format, Go evaluator (count, per-sample RTT p95, per-transmission p95, INCONCLUSO) with a recalculation CLI, and a measurement harness using the real TE-007 hub and real matrix Chrome/Edge through Playwright; harness-only transport on stdlib (SSE + fetch), no new dependencies; media commands on a local fixture without upload; US-017/US-013 replace the transport later.
- **L3 (evidence):** No WebSocket route, no `coder/websocket` in `go.mod`, `CanvasSurface` renders only boxes and text, no playback protocol (US-013). Control review: TRACE and EVID owner TE-015 is open and depends on TE-010, so `Diferido a TE-015` is mandatory and correct (rule 7); VERS owner TE-011 is closed, so it is demonstrated in full (rule 7). The release manifest pins Go `1.27.2` while the host has `go1.27.3` and CI installs `go.mod`'s `1.26.0`; the live run uses `GOTOOLCHAIN=go1.27.2` so its evidence matches the manifest. The issue control lines already follow rules 6-8; no edit needed now.
