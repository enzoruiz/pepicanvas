# TE-006 atomic media quota and failed-upload cleanup

Issue #8 (TE-006). Branch `feat/te-006-media-quota`, worktree `/home/enzo/projects/pepicanvas-te-006`, base `5756810`.
Scope: quota and asset lifecycle domain in `internal/media`, PostgreSQL adapter in `internal/platform/postgres`, S3 adapter in `internal/platform/s3` (minio-go, assigned to TE-006 by `docs/roadmap.md`).

## Specs

- **S1 — Atomic decimal accounting.** Issue: "Reservas, éxito, cancelación, reemplazo y liberación de última referencia producen contabilidad decimal atómica y ningún archivo fallido recuperable." US-016: "Cuota inclusiva de 1 000 000 000 bytes por canvas." Quota is an integer byte count per tenant and canvas; usage is `used + reserved`.
- **S2 — Boundary races.** Issue: "Carreras cerca de 1,000,000,000 bytes." US-016: "Total exactamente igual a la cuota se acepta; exceder un byte se rechaza." and "Dos cargas concurrentes cercanas al límite se deciden atómicamente y nunca superan la cuota."
- **S3 — Reservation and success.** US-016: "Una carga en curso reserva su tamaño declarado." and "Solo una carga validada con éxito consume cuota; una falla o cancelación libera la reserva y no deja archivos recuperables."
- **S4 — Duplicates and references.** Issue: "Duplicados, referencias múltiples y reemplazo sin capacidad." US-016: "Cada carga cuenta por separado, aunque duplique contenido.", "Eliminar la última referencia libera cuota lógicamente de inmediato." and "Eliminar una referencia no libera si quedan otras; eliminar la última sí."
- **S5 — Replacement.** US-016: "Reemplazar exige espacio para el nuevo archivo y libera el anterior solo tras éxito.", "Reemplazo sin capacidad adicional se rechaza y conserva el archivo anterior." and "Reemplazo exitoso cambia el consumo de forma atómica y después libera el anterior."
- **S6 — No orphan reservation.** Issue: "La contabilidad nunca supera la cuota, nunca queda una reserva huérfana y ningún fallo deja contenido recuperable." A reservation abandoned by a crashed upload expires and is released.
- **S7 — Failure and cancellation at every stage.** Issue: "Cancelación y falla en cada etapa observable." Stages: reserve, store bytes, inspect, complete. Each failure or cancellation releases the reservation and leaves no stored object.
- **S8 — No recoverable rejected file.** Issue: "Intento de recuperar archivos rechazados." Opening a failed, cancelled, rejected, or released asset returns not found, and its stored bytes are gone or the asset is isolated in `deleting`.
- **S9 — CTRL-IDEM-01 (owned here).** Issue: "reintentos de TE-006 (#8) antes y después de timeout o reconexión producen una única reserva o liberación y la misma respuesta; un payload distinto con el mismo identificador de operación se rechaza y la retención acotada se prueba en su vencimiento."
- **S10 — CTRL-MEDIA-01 (owner TE-005 closed: full proof).** Issue: "un medio rechazado, vencido o interrumpido durante la inspección de TE-005 libera su reserva y no deja temporales ni archivos recuperables."
- **S11 — CTRL-S3-01 early primitive.** Issue: "PostgreSQL registra los estados `pending`, `ready`, `quarantined` y `deleting`; solo `ready` se asocia, se sirve o se contabiliza como utilizable, y cada transición interrumpida deja un estado reintentable o aislado que TE-013 reconcilia."
- **S12 — CTRL-CONS-01 early primitive.** Issue: "reserva, éxito, cancelación, reemplazo y liberación se confirman cada uno en una única transacción PostgreSQL."
- **S13 — Isolation.** US-016: "Tenant A no consulta ni consume cuota de tenant B." and "Desactivar y reactivar conserva consumo." (no quota change on deactivation).

### Deferred to their owners

- CTRL-CONS-01 outbox event → TE-012. CTRL-S3-01 reconciler → TE-013. CTRL-RATE-01 → TE-014. CTRL-TRACE-01 → TE-015 (primitive: each operation row stores `correlation_id`). CTRL-RESTORE-01 → TE-017.

### Non-goals

Issue: "Elegir cuándo borrar físicamente o inspeccionar el formato." Also: HTTP routes, authorization at commit (US-011, US-016), canvas object foreign keys (US-010, US-011), the periodic reconciler (TE-013).

## Tasks

| ID | Specs | Route | Status | Commit |
|---|---|---|---|---|
| T1 | S1, S9 | inline, test-first: `internal/media` quota domain, asset states, ports, validation, fingerprints | pending | |
| T2 | S1-S6, S9, S11-S13 | inline, test-first: migration, sqlc queries, `postgres.MediaStore` with row lock, DB checks, idempotency, reservation expiry | pending | |
| T3 | S3, S7, S8, S10 | inline, test-first: `media.Uploads` pipeline (reserve, store, inspect, complete) with cleanup at every stage | pending | |
| T4 | S7, S8 | inline, test-first: `platform/s3` minio-go adapter, pinned MinIO testcontainer, end-to-end retrieval attempts | pending | |
| T5 | all | docs (`docs/stack.md`), verification, review assess, per-S verdicts | pending | |

## Log

- **L1** (user, 2026-10-18): "empieza el desarrollo del issue 8 en su respectivo worktree"
- **L2** (design, parent): quota row `media_usage` per canvas with `CHECK (used_bytes + reserved_bytes <= quota_bytes)` and `SELECT ... FOR UPDATE` per operation; assets in `media_assets` with states `pending|ready|quarantined|deleting`; references in `media_references` keyed by `(tenant, canvas, reference_id)`; operations in `media_operations` (TE-004 pattern, 10-minute retention). Completion attaches the first reference; replacement moves one reference to the new asset on success and releases the old asset when it was the last reference. Inspection rejection → `quarantined`; failure or cancellation → `deleting`; both release the reservation in the same transaction and are purged after the stored object is deleted. Bytes are stored before inspection under a random key and are never served unless `ready`. Migration number 00005: US-002 already uses 00004 on its branch; renumber at merge if TE-012 takes 00005 first.
