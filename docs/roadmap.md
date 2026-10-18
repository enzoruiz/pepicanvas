# Roadmap de desarrollo del MVP piloto

Este documento fija el orden de trabajo de los issues abiertos del milestone «MVP piloto». Se desarrolla por etapas: una etapa empieza cuando la anterior está mergeada en `main`.

## Cómo leer la tabla

- Todos los issues de una etapa se trabajan **en paralelo**, cada uno en su rama o worktree.
- **Camino crítico:** el issue que, si se atrasa, atrasa todo el piloto. Tiene prioridad dentro de su etapa.
- **Paralelo seguro:** toca paquetes distintos de los demás issues de la etapa y no agrega dependencias.
- **Paralelo con condición:** puede trabajarse en paralelo solo si se cumple la condición indicada.

El orden respeta las «Dependencias exactas» de cada issue y las dependencias prácticas de la sección siguiente.

## Etapas

| Etapa | Camino crítico | Paralelo seguro | Paralelo con condición |
|---|---|---|---|
| 1 | TE-004 (#6) | TE-002 (#7) | — |
| 2 | US-001 (#5) | TE-008 (#10), TE-009 (#11) | — |
| 3 | US-002 (#12) | TE-006 (#8), TE-012 (#40) | — |
| 4 | US-004 (#14) | US-003 (#13), US-007 (#17), TE-013 (#41) | TE-014 (#42) — C1 |
| 5 | US-005 (#15) | US-016 (#19), TE-010 (#36) | — |
| 6 | US-020 (#20) | US-006 (#16), US-011 (#24), TE-015 (#43) | — |
| 7 | US-008 (#21) | US-012 (#25), TE-016 (#44), TE-017 (#45) | US-010 (#23) — C2 |
| 8 | US-009 (#22) | US-014 (#26) | — |
| 9 | US-017 (#27) | — | — |
| 10 | US-018 (#28) | — | — |
| 11 | US-013 (#29) | US-019 (#31) | US-015 (#30) — C3 |
| 12 | US-021 (#32) | US-024 (#35) | — |
| 13 | US-022 (#33) | — | — |
| 14 | US-023 (#34) | — | — |
| 15 | GATE-001 (#37) | — | — |
| 16 | EXP-001 (#38) | — | — |
| 17 | GATE-002 (#39) | — | — |

## Condiciones de paralelo

- **C1 — TE-014 con las historias de la etapa 4.** TE-014 agrega middleware y las historias agregan rutas en el mismo punto de composición del servidor. Se mergean de a uno y ese archivo se resuelve a mano.
- **C2 — US-010 con US-008.** Ambos tocan el dominio de objetos del canvas. US-010 trabaja en archivos separados y se mergea primero. Si no se cumple, US-010 pasa a la etapa 8, nunca más tarde, porque US-017 lo necesita.
- **C3 — US-015 con US-013.** Ambos usan el protocolo de reproducción sincronizada. Se trabajan en paralelo solo si ese protocolo se acuerda antes; si no, US-015 va después de US-013 y agrega una etapa.

## Dependencias prácticas

Estas restricciones no figuran en las «Dependencias exactas» de los issues, pero el orden las respeta:

- **TE-004 antes que US-001.** Los dos necesitan PostgreSQL. TE-004 crea la infraestructura (driver, migraciones, testcontainers y CI) y US-001 la reutiliza.
- **TE-006 después de US-001.** US-001 agrega `golang.org/x/crypto` y TE-006 agrega el cliente S3. Se separan para que una etapa no tenga dos issues que agreguen dependencias a `go.mod`.
- **TE-014 después de US-001.** El token CSRF va ligado a la sesión, y las sesiones las crea US-001.

## Reglas

1. Dentro de una etapa, solo un issue agrega dependencias a `backend/go.mod` o a `frontend/package.json`.
2. Al empezar un issue, revisá sus líneas de controles según las reglas 6 a 8 de `docs/stack.md`: un control cuyo propietario ya cerró se demuestra completo; si el propietario sigue abierto, se declara `Diferido a`.
3. Si un issue cambia de etapa, actualizá esta tabla en el mismo cambio que lo justifica.
