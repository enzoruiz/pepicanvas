# TE-019 latency harness hardening

Issue #80, branch `feat/te-019-latency-harness-hardening`, worktree `/home/enzo/projects/pepicanvas-te-019`, base `78ba8e0` (origin/main).
Four non-blocking findings of the TE-010 native review; today each fails closed. No new dependency; push and PR only when the user asks.

## Specs

- **S1 — SSE ids and replay.** "`cmd/latencyprobe` emite `id: <sequence>` en cada evento `canvas`. Ante una reconexión con `Last-Event-ID`, repite en orden los eventos que `Hub.Events` aún retiene con una secuencia mayor, antes de los eventos en vivo. Si la secuencia pedida ya salió de la retención, cierra el flujo sin repetir nada, y la espera de render sigue venciendo."
- **S2 — Page ignores rendered sequences.** "La página de prueba ignora un evento cuya secuencia ya renderizó, así que una repetición nunca registra un segundo render ni una segunda muestra."
- **S3 — Lockfile `link` entries.** "`latencycheck` y `latency-evidence.mjs` omiten las entradas con `\"link\": true` y siguen rechazando una entrada sin `link` que no tenga `version`. Ambos lectores producen la misma lista `frontendDependencies` para el mismo lockfile."
- **S4 — Default port in the Host guard.** "La guarda de `Host` de `latencyprobe` acepta la dirección de escucha con o sin el puerto por defecto `:80` cuando escucha en el puerto 80. Todo otro `Host` u `Origin` sigue recibiendo `403`."
- **S5 — Own-key operations.** "`reduceBoxes` resuelve la operación solo entre claves propias (`Object.hasOwn` o un `Map`). Un `op` heredado (`constructor`, `toString`, `__proto__`) deja el estado sin cambios y no lanza."
- **S6 — Expected results.** "una reconexión con `Last-Event-ID` recibe exactamente los eventos retenidos posteriores, en orden y sin duplicados; una secuencia fuera de la retención no repite nada; un lockfile con entradas `link` produce la misma lista en Go y en Node, y una entrada sin `version` ni `link` sigue fallando; `Host` `127.0.0.1` y `127.0.0.1:80` se aceptan en el puerto 80, y cualquier otro `Host` u `Origin` hostil recibe `403` sin difusión; un `op` heredado deja el estado sin cambios y no llama a `registry.fail`."
- **S7 — Evidence.** "salida de las pruebas y una ejecución real de `npm run latency:probe` cuyo `latencycheck` sigue clasificando cada transmisión sin cambios de criterio."
- **S8 — Cleanup.** "el flujo cerrado por una secuencia fuera de retención libera su suscriptor, y no quedan goroutines."
- **S9 — Never a false MEASURED.** "Ninguno de los cambios puede producir un `MEASURED` con evidencia que hoy es inválida." "una reconexión sigue contando como desconexión, así que las muestras afectadas quedan inválidas por `disconnects` y nunca cuentan como válidas."
- **S10 — Deferred.** "CTRL-TRACE-01 — Diferido a TE-015" and "CTRL-EVID-01 — Diferido a TE-015".

## Design decisions

- Replay (S1): the events handler registers its subscriber under `publishMu`, so no broadcast can run between the replay snapshot and the registration; it replays the retained events in `(Last-Event-ID, published]` and live broadcast delivers everything after. A cursor that left the hub log (`ErrResyncRequired`) answers `204 No Content`, which ends `EventSource` without reconnecting and registers no subscriber. A malformed `Last-Event-ID` answers `400`.
- Default port (S4): only when the listening port is `80` the guard also accepts the host without `:80`, and the matching `Origin` `http://<host>` (browsers omit the default port in an origin).
- S2 gate: the page tracks the highest sequence it handled and drops any event at or below it.

## Tasks

- T1 — S3, S6 — inline — lockfile `link` entries in Go and Node with one shared fixture — commit: 4459102 (done)
- T2 — S5, S6 — inline — `reduceBoxes` own-key operations — commit: b7251f3 (done)
- T3 — S4, S6 — inline + verify (Risk item 2: guard) — Host guard default port — commit: 4cd8283 (done, verified)
- T4 — S1, S2, S6, S8, S9 — inline + verify (Risk item 4: publish/subscribe ordering) — SSE ids, `Last-Event-ID` replay, page sequence gate — commit: da4c810 (done, verified)
- T5 — S7, S9 — inline — real `npm run latency:probe` run and `latencycheck` — commit: n/a (done)

## Log

- L1 (user, 2026-10-24): "empieza el desarrollo del issue 80 en su respectivo worktree"
- L2: US-017 (#27) is still open at start, so point 1 (SSE replay) applies.
- L3 (evidence): T1 RED (Go `node_modules/local-kit has no version`, Node fixture test failed) then GREEN; T2 RED 4 inherited ops failed then GREEN (Vitest src/latency 49/49); T3 RED only the no-`:80` cases got 403, then GREEN; T4 RED (no `id:`, no replay, 200 instead of 204/400) then GREEN. `go test -race` latencyprobe, latencycheck, internal/latency ok. The page gate `sequenceGate.ts` was written before its test (no observed RED). `scripts/latency-evidence` tests use `node --test` (the existing runner), not Vitest. `docs/stack.md` updated in da4c810. Slice: 569 authored lines in 4 commits, mostly tests; one PR (issue size S).
- L4 (evidence): Independent gentle-ai-verify (Risk items 2 and 4) on 4cd8283 and da4c810: S1, S2, S4, S6, S8, S9 PASS, no blockers; RED confirmed on the parent commits; guard matrix of 34 Hosts x 21 Origins found no foreign value accepted; 16 concurrent resumes with 60 commands had no gap or duplicate; 204 path leaves 0 subscribers. Warnings, pre-existing or fail-safe: a request with two Origin headers is judged on the first (unchanged behavior); a reconnect before any canvas event replays nothing; a cursor ahead of the stream replays nothing and the gate drops later events, so the wait times out.
- L5 (evidence, S7): real `npm run latency:probe`, exit 0, latencycheck per transmission: chrome-current MEASURED 120/120 RTT p95 2.600 ms latency p95 26.038 ms; chrome-previous MEASURED 120/120 2.200 / 26.494; edge-current MEASURED 120/120 2.800 / 25.884; edge-previous MEASURED 120/120 2.800 / 26.019; every kind 20; all invalid counters 0.
