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

## Evidence schema (contract shared by T1-T5)

One JSON file per transmission, decoded strictly (unknown fields, duplicate keys, trailing data and missing fields are structural errors):

```json
{
  "schemaVersion": 1,
  "transmission": {
    "id": "chrome-current-<timestamp>",
    "scenarioId": "remote-render-latency",
    "correlationId": "<32 lowercase hex>",
    "startedAt": "RFC3339",
    "completedAt": "RFC3339",
    "measurementStartNs": 123,
    "client": { "family": "chrome|edge|obs", "slot": "current|previous", "version": "155.0.8059.79" }
  },
  "versions": {
    "releaseManifestSha256": "<64 hex of releases/client-support-release.json bytes>",
    "release": "0.1.0",
    "go": "1.27.2",
    "node": "24.16.0",
    "packageManager": { "name": "npm", "version": "11.13.0" },
    "goModules": [ { "name": "github.com/jackc/pgx/v5", "version": "v5.11.0" } ],
    "frontendDependencies": [ { "name": "react", "version": "19.3.0" } ],
    "postgresql": "17.4",
    "s3CompatibleStorage": "1.0.0-contract",
    "ffmpeg": "7.1.1",
    "ffprobe": "7.1.1",
    "clients": [ { "family": "chrome", "slot": "current", "version": "155.0.8059.79" } ]
  },
  "samples": [
    {
      "id": "s-001",
      "kind": "create|move|resize|rotate|delete|media",
      "operationId": "op-...",
      "sequence": 1,
      "commandCorrelationId": "<32 hex>",
      "renderCorrelationId": "<32 hex>",
      "acceptedAtNs": 1000,
      "renderedAtMs": 12.5,
      "pings": [ { "clientSentMs": 1.0, "serverAtNs": 1500000, "clientReceivedMs": 2.0 } ],
      "disconnects": 0
    }
  ]
}
```

- `goModules`: every `require` entry of `backend/go.mod` (direct and indirect), sorted by name. `frontendDependencies`: every `packages` entry of `frontend/package-lock.json` except the root, name taken from the path after the last `node_modules/`, sorted by name then version, duplicates kept once.
- `clients`: the six clients of the release manifest, in manifest order.
- Sample validity (S2, S3, S8): ≥5 pings, each with `clientReceivedMs ≥ clientSentMs`; nearest-rank p95 of the ping RTTs (`clientReceivedMs − clientSentMs`) ≤ 100 ms; `disconnects == 0`; `acceptedAtNs ≥ measurementStartNs`; `commandCorrelationId == renderCorrelationId`. Invalid samples are counted by reason and excluded.
- Latency of a valid sample: offset from the minimum-RTT ping `serverAtNs/1e6 − (clientSentMs + clientReceivedMs)/2`; latency ms = `renderedAtMs + offset − acceptedAtNs/1e6`.
- Transmission result `MEASURED` only when: the client equals one of the release manifest's clients exactly (family, slot, version); every field of `versions` equals the current pins (release manifest SHA-256 and values, go.mod modules, package-lock packages); ≥100 valid samples; every kind has ≥1 valid sample. Otherwise `INCONCLUSIVE` with every reason (`incompatible_client`, `version_mismatch:<field>`, `insufficient_samples`, `missing_kind:<kind>`). Never another result.
- Report per transmission: total samples, valid samples, invalid counts by reason, valid count per kind, RTT p95 over all pings of valid samples, latency p95 over valid samples (absent when no valid sample), result, reasons.

## Harness protocol (contract shared by T3-T5)

- `cmd/latencyprobe -listen 127.0.0.1:<port> -static <dir>`: refuses a non-loopback listen address; serves `<dir>` at `/` (the probe page is `/latency/`), one fixed canvas key on a real `realtime.Hub` with an allow-all authorizer (synthetic tenant, harness only, never wired into `cmd/server`). Server clock: ns since process start (monotonic). SIGINT/SIGTERM: HTTP `Shutdown`, then `Hub.Close`.
- `GET /probe/ping` → `200 {"serverAtNs": n}`, `Cache-Control: no-store`.
- `POST /probe/commands` body `{"operationId":"…","objectId":"…","kind":"set|delete","payload":{…}}` (`payload` omitted for delete) → the server creates a `correlation_id` with `platform/correlation`, submits the `realtime.Command`, takes `acceptedAtNs` as soon as `Submit` returns accepted, then broadcasts the hub's retained event (`Hub.Events`) to every SSE subscriber in sequence order. `200 {"outcome":"accepted","sequence":n,"acceptedAtNs":n,"correlationId":"<32 hex>"}`; domain rejection `409 {"outcome":"rejected","reason":"…"}`; malformed `400`.
- `GET /probe/events` (SSE): first `event: ready` `data: {}`; then per accepted event `event: canvas` `data: {"sequence":n,"operationId":"…","objectId":"…","kind":"set|delete","payload":{…}|null,"correlationId":"…"}`. A subscriber whose bounded buffer is full is disconnected (the page counts it).
- Payloads (opaque to the server): `{"op":"create","x","y","width","height","rotation"}`, `{"op":"move","x","y"}`, `{"op":"resize","width","height"}`, `{"op":"rotate","rotation"}`, `{"op":"media","action":"play|pause"}`; delete uses kind `delete`.
- Page API on `window.__pepicanvasLatency`: `ready(): Promise<void>` (SSE `ready` received and the media element loaded: the generated WAV reached `canplaythrough`); `pings(n): Promise<Ping[]>` (sequential `GET /probe/ping`, `performance.now()` around each); `rendered(operationId): Promise<{renderedAtMs, correlationId, sequence}>` (resolves when that event has been rendered, also if it was rendered before the call); `disconnects(): number` (EventSource `error` events since load). Rendering: box events update `CanvasSurface` objects with `flushSync`, media events call `play()`/`pause()` and wait for `playing`/`pause`; the render instant is the second `requestAnimationFrame` after that.

## Delivery slices

- Slice 1 (past ~400 lines, recorded at a969dbf): T0-T2, the evaluator and its CLI. Slice 2: T3-T5, the live harness (T3 at 63f37d8, T4 at 718f4b7, T5 at 35837d2). Slice 3: T6 docs. Chained PRs follow these boundaries when the user asks for a PR.

## Tasks

- [x] T0 — Read issue, roadmap, stack; review control lines (S8-S10). Route: inline. Commit: this document.
- [x] T1 — `backend/internal/latency`: strict evidence decode and evaluator (S1-S6, S8 primitive, S10 comparison). Route: gentle-ai-worker (context backstop), test-first. Commit: 70cd225
- [x] T2 — `backend/cmd/latencycheck`: CLI that loads the release manifest and repository pins, evaluates evidence files, prints per-transmission results, exit codes (S5, S6, S10). Route: gentle-ai-worker, test-first. Commit: a969dbf
- [x] T3 — `backend/cmd/latencyprobe`: loopback harness server with the real hub, SSE events, ping, commands, static probe page (S1, S3, S8). Route: gentle-ai-worker, test-first. Commit: 63f37d8
- [x] T4 — Frontend probe page and latency primitives (render mark, pings, event stream disconnects), separate Vite config (S1-S3). Route: gentle-ai-worker, test-first (Vitest). Commit: 718f4b7
- [x] T5 — Playwright latency runner over the browser matrix writing evidence and validating it with `latencycheck`; real run (S1-S6, S10). Route: gentle-ai-worker. Commit: 35837d2
- [ ] T6 — Docs (`docs/stack.md`), close-out checks, native review per commit if due. Route: inline. Commit: —

## Log

- **L1 (user request, verbatim):** "Empezá el desarrollo del issue #36 [TE-010] Medir latencia desde aceptación hasta render remoto, en la rama feat/te-010-remote-render-latency. Estás en un worktree dedicado sobre origin/main; trabajá solo acá y en tu rama. Leé el issue completo con gh issue view, docs/roadmap.md y docs/stack.md antes de tocar código. Reglas de la etapa 5 (en paralelo están US-005 #15, US-016 #19 y TE-010 #36): (1) solo un issue de la etapa puede agregar dependencias a backend/go.mod o frontend/package.json; si necesitás agregar una, frená y preguntame antes. (2) Revisá las líneas de controles del issue según las reglas 6 a 8 de docs/stack.md: control con propietario cerrado se demuestra completo; propietario abierto se declara Diferido a. La etapa 4 todavía tiene issues abiertos (US-003, US-007, TE-013, TE-014): no mergees nada; podés commitear y pushear tu rama, pero no abras PR hasta que te lo pida."
- **L2 (user decision):** "Instrumento + arnés en vivo (recomendado)": sample format, Go evaluator (count, per-sample RTT p95, per-transmission p95, INCONCLUSO) with a recalculation CLI, and a measurement harness using the real TE-007 hub and real matrix Chrome/Edge through Playwright; harness-only transport on stdlib (SSE + fetch), no new dependencies; media commands on a local fixture without upload; US-017/US-013 replace the transport later.
- **L3 (evidence):** No WebSocket route, no `coder/websocket` in `go.mod`, `CanvasSurface` renders only boxes and text, no playback protocol (US-013). Control review: TRACE and EVID owner TE-015 is open and depends on TE-010, so `Diferido a TE-015` is mandatory and correct (rule 7); VERS owner TE-011 is closed, so it is demonstrated in full (rule 7). The release manifest pins Go `1.27.2` while the host has `go1.27.3` and CI installs `go.mod`'s `1.26.0`; the live run uses `GOTOOLCHAIN=go1.27.2` so its evidence matches the manifest. The issue control lines already follow rules 6-8; no edit needed now.
- **L4 (evidence):** T1/T2 verified by the parent: gofmt empty, vet clean, `go test -race` ok for both packages. Worker resolutions accepted: an invalid sample counts under every failing reason; malformed equal correlations count as `correlation_mismatch`; lists in `versions` must match in order; negative latency is reported as-is; any structural error in any file exits 2 with no stdout. Evidence `versions` must hold OBSERVED values (never copied from the manifest), so the live run uses `GOTOOLCHAIN=go1.27.2`.
- **L5 (evidence):** T3 verified by the parent: gofmt empty, vet clean, `go test -race -count=3` ok. Worker contract clarifications accepted: set requires an object payload; delete omits `payload` (`null` is 400); same operation ID with another intent is 409 `operation_conflict`; a retry returns 200 with the original sequence, a fresh `acceptedAtNs`, and no second broadcast; publish reads `Events(after=lastPublished)`; `localhost` is refused (literal loopback IP only); subscriber buffer 256 frames.
- **L6 (evidence):** T4 verified by the parent: tsc ok, eslint `src/latency vite.latency.config.ts` ok (`latency/` holds only HTML, which ESLint does not lint), prettier ok, Vitest 39/39; production build still emits only editor and overlay. Accepted: the audio element is muted and loops (play() needs no gesture; `playing`/`pause` still fire), so the runner alternates play and pause; a frame that fails parsing never settles `rendered()`, so the runner times out per sample. Browser cache downloaded and SHA-256 verified; `go1.27.2` toolchain downloaded.
- **L7 (evidence):** Real run `npm run latency:probe:offline` (GOTOOLCHAIN=go1.27.2), recomputed by the parent with `latencycheck`, exit 0:
  - chrome-current 155.0.8059.79: MEASURED, 120/120 valid, RTT p95 1.100 ms, latency p95 25.826 ms
  - chrome-previous 154.0.8037.92: MEASURED, 120/120, RTT p95 1.200 ms, latency p95 26.008 ms
  - edge-current 155.0.4283.45: MEASURED, 120/120, RTT p95 1.200 ms, latency p95 25.550 ms
  - edge-previous 154.0.4258.62: MEASURED, 120/120, RTT p95 1.500 ms, latency p95 24.566 ms
  - Every kind 20 valid samples per transmission; versions go 1.27.2, node 24.16.0, npm 11.13.0, 73 Go modules, 258 frontend packages, release manifest SHA-256 matches. Negative paths on copies: 99 samples → INCONCLUSIVE `insufficient_samples` (exit 1); go 1.27.3 → INCONCLUSIVE `version_mismatch:go` (exit 1); one disconnected and one correlation-mismatched sample → excluded, 118 valid, MEASURED. Evidence lives in gitignored `frontend/.cache/latency/evidence/` (durability is TE-015, S9). OBS was not run live (its harness is Windows-only); OBS versions are pinned through the release manifest.
