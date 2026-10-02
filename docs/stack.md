# Stack técnico de PepiCanvas

Este documento fija las tecnologías del MVP piloto y el motivo de cada elección. Cada decisión se vincula con el issue que la exige.

## Resumen

| Capa | Elección |
|---|---|
| Backend | Go (última versión estable, mínimo 1.25) con biblioteca estándar, **sin framework** |
| Frontend | TypeScript + React + Vite (SPA), **sin framework de servidor** |
| Base de datos | PostgreSQL |
| Archivos | Almacenamiento compatible con S3 (MinIO en local) |
| Medios | ffprobe / ffmpeg como subproceso |
| Tiempo real | WebSocket |
| Pruebas | `go test -race`, Vitest y Playwright |

## Principios que guían el stack

1. **El servidor es la fuente de verdad.** El orden, la autorización y los cortes de acceso se deciden en el servidor (TE-001, TE-002, TE-007).
2. **Un solo proceso con estado para el piloto.** Los bloqueos, el orden por canvas y el registro de conexiones viven en memoria. No se introduce Redis ni colas externas hasta que exista una necesidad medida.
3. **El dominio no depende de la infraestructura.** La arquitectura es hexagonal; ni frameworks ni drivers entran en el núcleo.
4. **Pocas dependencias y auditables.** Es un producto con requisitos de seguridad y aislamiento entre streamers.

## Backend: Go sin framework

### Por qué no usar un framework (Gin, Echo, Fiber)

- Desde Go 1.22, `net/http` enruta por método y parámetros de ruta (`GET /canvases/{id}`). Cubre lo que antes justificaba un framework.
- La superficie HTTP es pequeña: autenticación, invitaciones, administración, subida de medios y entrega de archivos. El núcleo del sistema corre sobre WebSocket, donde los frameworks HTTP no aportan nada.
- Fiber usa `fasthttp`, que es incompatible con el ecosistema de `net/http`.
- Un framework tiende a filtrarse hacia los handlers y el dominio, lo que choca con la arquitectura hexagonal.

**Costo aceptado:** `http.ServeMux` no tiene grupos de rutas ni middleware por grupo, así que la composición (autenticación → tenant → handler) se hace envolviendo handlers. Si ese armado se vuelve tedioso, se incorpora `github.com/go-chi/chi/v5`, un router compatible con `net/http` que agrega grupos y middleware sin reescribir handlers.

### Bibliotecas

| Necesidad | Biblioteca | Motivo |
|---|---|---|
| HTTP y enrutamiento | `net/http` (stdlib) | Suficiente desde Go 1.22 |
| WebSocket | `github.com/coder/websocket` | Mantenida, integra `context.Context` para cancelar y cerrar conexiones en ≤2 s (TE-002) |
| Driver de PostgreSQL | `github.com/jackc/pgx/v5` | Driver nativo, transacciones y pool |
| Consultas SQL | `sqlc` (generador) | SQL explícito con tipos generados; sin ORM, para controlar transacciones y bloqueos de fila (TE-004, TE-006) |
| Migraciones | `github.com/pressly/goose/v3` | Migraciones SQL versionadas, ejecutables desde el binario |
| Contraseñas | `golang.org/x/crypto/argon2` (argon2id) | Hash de contraseñas recomendado por OWASP (US-001) |
| Almacenamiento de archivos | `github.com/minio/minio-go/v7` | Cliente S3 liviano, compatible con MinIO, S3 y R2 |
| Logs | `log/slog` (stdlib) | Logs estructurados, con un handler que redacta secretos (TE-003) |
| Configuración | Variables de entorno (stdlib) | Sin dependencias extra |
| Tipos para el frontend | `github.com/gzuidhof/tygo` (generador) | Genera tipos TypeScript del protocolo a partir de los structs de Go |

### Decisiones específicas

- **Sesiones propias en el servidor, sin JWT.** El token es opaco y aleatorio, se guarda como hash SHA-256 en PostgreSQL y viaja en una cookie `HttpOnly`, `Secure` y `SameSite=Lax`. Un JWT no puede revocarse al instante, y US-003 y US-006 exigen corte inmediato.
- **Un actor por canvas.** Una goroutine por canvas recibe los comandos por canal, les asigna una secuencia de aceptación y deduplica por identificador de operación (TE-007).
- **Reloj inyectable.** Los bloqueos con heartbeat de 5 s (TE-009) y los plazos de cierre de 2 s dependen de una interfaz de reloj. Se prueban con `testing/synctest`.
- **Cuota en bytes enteros.** Se guarda como `BIGINT` en bytes y se reserva en transacción. Así la contabilidad es exacta, sin flotantes (TE-006).
- **Los medios se sirven a través del backend.** No se exponen URLs firmadas del almacenamiento. Las respuestas privadas usan `Cache-Control: no-store` y `Referrer-Policy: no-referrer` (TE-003).
- **Inspección de medios.** `ffprobe` determina el tipo real, los códecs, la duración y la resolución. `ffmpeg -f null` decodifica el archivo completo, incluidos todos los fotogramas de un GIF, antes de aceptarlo (TE-005). Corre como subproceso con límite de tiempo.

### Estructura (screaming + hexagonal)

```text
backend/
  cmd/server/            # composición e inicio del binario
  internal/
    access/              # autorización, sesiones, invitaciones, membresías (TE-001, US-001..006)
    subscription/        # activación y desactivación de streamers (US-002, US-003)
    canvas/              # canvas, objetos, transformaciones, orden z (TE-004, US-007..)
    realtime/            # actor por canvas, conexiones, resincronización (TE-007, TE-008)
    locking/             # concesiones con heartbeat (TE-009)
    media/               # inspección, cuota y almacenamiento (TE-005, TE-006)
    platform/            # adaptadores: postgres, s3, http, websocket, reloj
  migrations/
```

Cada módulo de dominio expone puertos (interfaces) y no importa `platform`. Los adaptadores implementan esos puertos.

## Frontend: React + Vite, sin framework de servidor

### Por qué React + Vite y no Next.js

- No hay necesidad de SSR ni SEO: el editor es una aplicación autenticada y OBS carga una página fija.
- Next.js agregaría un segundo servidor que competiría con Go por sesiones, autorización y rutas.
- Vite genera archivos estáticos que el binario de Go sirve directamente. Así hay un solo origen y la cookie de sesión es la misma para API y WebSocket.

### Dos puntos de entrada en un mismo proyecto

- **Editor** (`/editor`): lo usan streamers y moderadores.
- **Overlay de OBS** (`/overlay`): página mínima con fondo transparente para la fuente de navegador. No carga lógica de edición.

Ambos comparten un **renderizador de canvas basado en DOM** (elementos `img`, `video` y `audio` con transformaciones CSS). Usar DOM en vez de `<canvas>` permite reproducir video, GIF y audio de forma nativa, y garantiza que el editor y OBS muestren exactamente lo mismo (US-007).

### Bibliotecas

| Necesidad | Biblioteca | Motivo |
|---|---|---|
| UI | React | Ecosistema maduro para un editor interactivo |
| Build | Vite (modo multipágina) | Rápido, salida estática, dos entradas |
| Rutas | React Router | Pocas rutas: login, editor, invitaciones y ajustes |
| Estado | Zustand | Almacén pequeño alimentado por el WebSocket; el servidor es la fuente de verdad |
| Estilos | Tailwind CSS v4 (plugin de Vite) | Escala de tokens forzada y CSS generado solo con las clases usadas |
| Componentes | shadcn/ui sobre Radix | Diálogos, menús y popovers accesibles; el código vive en el repo y se adapta |
| Lint | ESLint (flat config) + `typescript-eslint` `strictTypeChecked` | Reglas con información de tipos del compilador real |
| Formato | Prettier | Estándar del ecosistema; no se superpone con ESLint |

### Estilos: Tailwind CSS en vez de CSS Modules

- **El canvas no usa ninguno de los dos.** La posición, el tamaño, la rotación y el orden z de cada objeto son datos del servidor y se aplican con `style` inline (`transform`, `zIndex`). Tailwind solo se usa en la UI del editor.
- La UI del editor está formada por diálogos, menús y formularios (login, invitaciones, miembros, ajustes). shadcn/ui sobre Radix da componentes accesibles y probados. Con CSS Modules habría que estilizar esos componentes desde cero.
- Tailwind impone una escala de espaciado y color compartida, lo que mantiene la consistencia sin depender de convenciones propias.
- Ninguna de las dos opciones tiene costo en runtime, así que la performance no fue un factor diferencial.
- El overlay de OBS no carga Tailwind ni componentes de UI: solo el renderizador.

### Lint: ESLint en vez de Biome

- El frontend es asíncrono por naturaleza (WebSocket, reconexión, resincronización). `typescript-eslint` detecta promesas no esperadas y funciones `async` mal usadas (`no-floating-promises`, `no-misused-promises`) usando el compilador de TypeScript. Biome 2 tiene inferencia de tipos propia, pero cubre solo una parte de esos casos.
- `eslint-plugin-boundaries` aplica las reglas de dependencia entre capas. Por ejemplo, impide que el renderizador importe código del editor o que una feature importe otra.
- `eslint-plugin-react-hooks` es el plugin oficial del equipo de React.
- Costo aceptado: el lint con información de tipos es más lento y la configuración es más extensa que con Biome.

No se usa una biblioteca de estado de servidor (TanStack Query), porque casi todo el estado llega por WebSocket. Se reevaluará si la superficie REST crece.

## Infraestructura y desarrollo local

- **Docker Compose** para PostgreSQL y MinIO en local.
- **Un solo binario de Go** en producción, que sirve la API, el WebSocket y los archivos estáticos del frontend.
- **GitHub Actions** para CI: `golangci-lint`, `go test -race`, `sqlc diff`, ESLint, `prettier --check`, Vitest y Playwright.

## Pruebas

| Nivel | Herramienta | Alcance |
|---|---|---|
| Dominio Go | `testing` + pruebas en tabla + `-race` | Matriz de autorización con dos tenants por caso (TE-001), orden y deduplicación |
| Tiempo | `testing/synctest` | Heartbeat de 5 s y cierres en ≤2 s sin esperas reales |
| Integración Go | `testcontainers-go` | PostgreSQL y MinIO reales, atomicidad tras reinicio (TE-004) |
| Frontend | Vitest | Lógica del almacén y del renderizador |
| E2E | Playwright (canales `chrome` y `msedge`) | Matriz de navegadores soportados (TE-011) |
| OBS | Procedimiento propio | Playwright no puede automatizar OBS; se definirá en TE-011 |

## Lo que no se usa y por qué

| Descartado | Motivo |
|---|---|
| Serverless (Vercel, Firebase, Lambda) | Sin WebSockets persistentes ni control del orden por canvas |
| JWT | No permite revocación inmediata |
| ORM (GORM, Ent) | Oculta transacciones y bloqueos que son requisitos explícitos |
| Redis | Innecesario con una sola instancia; se reevaluará al escalar horizontalmente |
| Next.js | Duplica el servidor sin aportar SSR útil |
| Frameworks HTTP de Go (Gin, Echo, Fiber) | Resuelven routing y binding, que la stdlib ya cubre; no aportan nada a WebSocket, orden ni cortes de acceso. Fiber además es incompatible con `net/http` |
| CSS Modules | Obligaría a construir y estilizar a mano componentes accesibles |
| Biome | Su análisis con tipos es parcial y no tiene un equivalente a `eslint-plugin-boundaries` |
