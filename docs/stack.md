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

## Contrato transversal de controles

Este catálogo es la definición canónica de las garantías transversales del MVP. Los issues consumidores deben referenciar los identificadores aplicables y la evidencia que producirán; no deben copiar ni reinterpretar estas definiciones. Los términos **debe**, **no debe** y **solo** expresan requisitos normativos.

### Aplicación desde el backlog

1. Cada historia, habilitador o validación debe declarar una línea por control aplicable con el formato `<ID> — <evidencia esperada>`.
2. Cuando ningún control sea aplicable, el issue debe registrar `No aplica` y justificarlo respecto de sus superficies y datos. No alcanza con afirmar que el cambio es pequeño.
3. El issue propietario implementa y mantiene la capacidad común. Un issue consumidor sigue siendo responsable de demostrar que la usa correctamente en su propio recorrido.
4. Si un cambio altera una obligación, una superficie, la evidencia mínima, el comportamiento ante fallo o una exclusión, primero debe actualizar este catálogo y revisar sus consumidores.
5. Las exclusiones del MVP limitan la solución permitida; no eliminan la obligación del control.

### Definition of Ready y Definition of Done

Un issue está **Ready** cuando identifica los controles aplicables, enlaza sus propietarios, especifica evidencia repetible y resuelve cualquier dependencia que impida verificarla. Está **Done** cuando conserva la evidencia declarada, verifica los fallos exigidos y no introduce una excepción sin documentarla en este contrato. La Definition of Done del issue debe contener los resultados concretos; no debe duplicar la prosa del catálogo.

<!-- CONTROL-CONTRACT:START -->

### CTRL-CONS-01 — Consistencia entre PostgreSQL y WebSocket

- **Obligación:** toda mutación durable debe confirmar primero la transacción de dominio y un evento de outbox en la misma transacción PostgreSQL. Un publicador reintentable entrega después el evento al actor del canvas y marca su entrega; ninguna confirmación WebSocket puede representar estado que no quedó confirmado en PostgreSQL.
- **Familias y superficies aplicables:** mutaciones de canvas, objetos, miembros, suscripciones, credenciales OBS y cualquier operación durable que produzca una notificación WebSocket.
- **Issue propietario planificado:** `TE-012 — Publicar mutaciones durables mediante outbox PostgreSQL`.
- **Evidencia mínima:** prueba de integración con PostgreSQL real que fuerce una caída entre el commit y la publicación, reinicie el publicador y demuestre entrega posterior sin pérdida; métricas o consulta que permitan observar pendientes y antigüedad.
- **Comportamiento ante fallo:** conservar el evento pendiente, reintentar con espera acotada y no revertir un commit ya confirmado. Si no puede publicarse, el cliente debe recuperar el estado por resincronización; la cola pendiente debe quedar observable y operable.
- **Exclusiones MVP:** no se incorporan Redis, Kafka ni otro broker; no se promete entrega exactamente una vez ni coordinación entre múltiples instancias. El orden autoritativo sigue siendo por canvas y la entrega es al menos una vez.

### CTRL-S3-01 — Reconciliación entre PostgreSQL y almacenamiento S3

- **Obligación:** PostgreSQL debe registrar el estado del asset como `pending`, `ready`, `quarantined` o `deleting`. Solo `ready` puede asociarse a objetos o servirse. La carga, inspección y eliminación deben ser reanudables, y un reconciliador debe detectar objetos temporales, registros incompletos y eliminaciones pendientes.
- **Familias y superficies aplicables:** imágenes, GIF, audio, video, miniaturas y cualquier flujo que escriba o elimine objetos en MinIO, S3 o un servicio compatible.
- **Issue propietario planificado:** `TE-013 — Reconciliar assets entre PostgreSQL y almacenamiento S3`.
- **Evidencia mínima:** pruebas de integración que interrumpan cada transición antes y después de escribir en S3, ejecuten reconciliación y demuestren convergencia; inventario consultable por estado y edad sin exponer nombres privados.
- **Comportamiento ante fallo:** no publicar ni contabilizar como utilizable un asset incompleto; mantenerlo en un estado reintentable o aislado, liberar reservas cuando corresponda y programar limpieza idempotente. Una discrepancia no puede resolverse borrando contenido válido sin evidencia de propiedad.
- **Exclusiones MVP:** no hay transacción distribuida, replicación multirregión, versionado de bucket ni garantía inmediata de recolección; la reconciliación periódica y bajo demanda es suficiente para el piloto.

### CTRL-IDEM-01 — Idempotencia y deduplicación

- **Obligación:** toda mutación HTTP o WebSocket susceptible de reintento debe aceptar un identificador de operación estable dentro de su ámbito, persistir o retener el resultado autoritativo y devolver el mismo resultado ante repeticiones equivalentes. Reutilizar el identificador con un payload distinto debe rechazarse.
- **Familias y superficies aplicables:** comandos del canvas, cambios de orden, reproducción de medios, cargas finalizables, invitaciones, rotaciones, reintentos de outbox y tareas de reconciliación.
- **Issue propietario planificado:** `TE-007 — Serializar y deduplicar comandos por canvas`, ampliado por el issue implementador de cada operación durable fuera del actor.
- **Evidencia mínima:** pruebas con duplicados antes, durante y después de una reconexión o timeout que demuestren un único efecto durable y una respuesta estable; caso negativo de colisión entre identificador y payload.
- **Comportamiento ante fallo:** no repetir efectos laterales de manera ciega; consultar el resultado registrado o dejar la operación reintentable. Si no puede determinarse equivalencia, rechazar de forma explícita y exigir un identificador nuevo para una intención nueva.
- **Exclusiones MVP:** no existe deduplicación global ni indefinida; el ámbito y la retención pueden definirse por operación. Las acciones de presencia efímera sin efecto durable pueden quedar excluidas con justificación.

### CTRL-WEB-01 — Protección de origen y solicitudes web

- **Obligación:** HTTP con efecto lateral debe validar un token CSRF ligado a la sesión además de las cookies `SameSite`; WebSocket debe validar `Origin` contra una lista exacta antes del upgrade. CORS permanece cerrado salvo necesidad documentada, y nunca sustituye autorización ni aislamiento entre streamers.
- **Familias y superficies aplicables:** autenticación basada en cookies, formularios y API mutante, invitaciones, administración, cargas, editor WebSocket y overlay OBS cuando inicie una conexión autenticada.
- **Issue propietario planificado:** `TE-014 — Endurecer ingreso HTTP y WebSocket`.
- **Evidencia mínima:** pruebas de integración para origen permitido, ausente y hostil; token CSRF válido, faltante e inválido; cookies y cabeceras de seguridad verificadas sobre HTTPS o su terminación representativa.
- **Comportamiento ante fallo:** rechazar antes de ejecutar dominio, escribir archivos o abrir el WebSocket; responder sin reflejar secretos y registrar motivo, ruta y correlación con cardinalidad controlada.
- **Exclusiones MVP:** no se admite integración web de terceros ni CORS comodín. Clientes no navegador y despliegues con múltiples orígenes requieren una decisión explícita posterior.

### CTRL-RATE-01 — Límites de abuso y costo

- **Obligación:** los puntos de entrada con riesgo de fuerza bruta, enumeración, consumo intensivo o creación no acotada deben aplicar límites explícitos por una combinación adecuada de cuenta, sesión, streamer e IP. Los límites deben ser deterministas, observables y no debilitar la autorización.
- **Familias y superficies aplicables:** inicio de sesión, creación y aceptación de invitaciones, subida e inspección de medios, rotación de credenciales, apertura o reconexión WebSocket y comandos de alta frecuencia.
- **Issue propietario planificado:** `TE-014 — Endurecer ingreso HTTP y WebSocket`.
- **Evidencia mínima:** pruebas del límite y su ventana, respuesta `429` o cierre documentado, recuperación al vencer la ventana y métricas sin dimensiones no acotadas; caso que demuestre aislamiento entre streamers.
- **Comportamiento ante fallo:** rechazar trabajo nuevo antes de consumir el recurso costoso, indicar una espera segura cuando corresponda y conservar la disponibilidad de operaciones de revocación o emergencia. La indisponibilidad del limitador local debe fallar de forma segura en superficies sensibles.
- **Exclusiones MVP:** el limitador puede vivir en memoria por existir una sola instancia; no se incorpora un almacén distribuido, reputación de IP, CAPTCHA ni mitigación DDoS de capa de red.

### CTRL-MEDIA-01 — Ejecución acotada de ffprobe y ffmpeg

- **Obligación:** la inspección debe ejecutar binarios fijados por release como procesos sin shell, con argumentos controlados, identidad sin privilegios, directorio temporal aislado y límites de tiempo, bytes, resolución, duración, cantidad de streams, memoria, CPU, procesos y salida capturada. El tipo declarado nunca sustituye la inspección completa.
- **Familias y superficies aplicables:** carga, validación, decodificación, extracción de metadatos y cualquier transformación de imagen, GIF, audio o video.
- **Issue propietario planificado:** `TE-005 — Validar medios por contenido y decodificación completa`.
- **Evidencia mínima:** pruebas con archivo válido y muestras truncadas, sobredimensionadas, lentas, con salida excesiva o estructura maliciosa; comprobación de timeout, terminación del grupo de procesos y limpieza temporal.
- **Comportamiento ante fallo:** terminar el proceso y sus descendientes, rechazar o aislar el asset, eliminar temporales, liberar la reserva aplicable y registrar categorías acotadas sin conservar contenido privado en logs.
- **Exclusiones MVP:** no hay transcodificación para entrega, análisis antivirus general ni sandbox distribuido. Los límites del host o contenedor complementan, pero no reemplazan, los límites del proceso.

### CTRL-TRACE-01 — Correlación de operaciones

- **Obligación:** cada recorrido debe propagar un identificador de correlación y contexto OpenTelemetry desde HTTP o WebSocket hacia dominio, PostgreSQL, outbox, actor del canvas, S3 y subprocesos. Logs, métricas y trazas deben permitir relacionar aceptación, persistencia, publicación y render sin incluir secretos ni contenido privado.
- **Familias y superficies aplicables:** todos los endpoints, conexiones y comandos; obligatoria en recorridos asíncronos, medios, revocación, resincronización y medición del piloto.
- **Issue propietario planificado:** `TE-015 — Correlacionar operaciones y conservar evidencia operativa`.
- **Evidencia mínima:** prueba de integración o recorrido instrumentado que muestre una correlación continua a través de al menos una frontera asíncrona; consulta reproducible de logs o trazas por identificador y verificación de redacción.
- **Comportamiento ante fallo:** la indisponibilidad del exportador no debe bloquear el recorrido de producto ni acumular memoria sin límite; debe descartar de forma acotada, exponer la pérdida mediante métricas y mantener logs locales correlacionados.
- **Exclusiones MVP:** no se exige un backend específico de telemetría, trazado exhaustivo de cada frame ni alta disponibilidad del colector. El muestreo no puede eliminar recorridos de seguridad o evidencia obligatoria definidos por una validación.

### CTRL-EVID-01 — Evidencia durable y evaluable

- **Obligación:** la evidencia exigida por un issue o control del piloto debe almacenarse fuera de buffers de proceso con versión de release, escenario, instante, resultado, correlación y ubicación verificable. Debe ser legible por una persona evaluadora autorizada y estar redactada antes de persistirse.
- **Familias y superficies aplicables:** pruebas de integración y E2E, GATE, experimentos, incidentes de seguridad, métricas de latencia, restauraciones, reconciliaciones y verificaciones de compatibilidad OBS.
- **Issue propietario planificado:** `TE-015 — Correlacionar operaciones y conservar evidencia operativa`.
- **Evidencia mínima:** manifiesto por ejecución con referencias a artefactos durables, comprobación de integridad, política de acceso y retención, más una lectura independiente que permita recalcular o clasificar el resultado.
- **Comportamiento ante fallo:** si falta evidencia obligatoria, el resultado es `INCONCLUSO`, nunca aprobado; si la escritura durable falla, debe señalarse la ejecución como incompleta y evitar que artefactos parciales parezcan válidos.
- **Exclusiones MVP:** no se define un data lake, SIEM ni retención indefinida. Logs efímeros del proceso y capturas sin metadatos no constituyen evidencia suficiente.

### CTRL-VERS-01 — Versiones reproducibles por release

- **Obligación:** cada release debe fijar y registrar versiones exactas de Go, módulos, Node.js, gestor de paquetes, dependencias frontend, PostgreSQL, almacenamiento S3 compatible, ffmpeg/ffprobe, navegadores y OBS usados para construir o validar. Las actualizaciones requieren una revisión explícita y regeneración de evidencia afectada.
- **Familias y superficies aplicables:** build, CI, imágenes o paquetes de despliegue, migraciones, validación de medios, pruebas de navegadores y matriz OBS.
- **Issue propietario planificado:** `TE-011 — Registrar la matriz exacta de clientes soportados por versión`, ampliado para conservar también el manifiesto reproducible del toolchain y la release.
- **Evidencia mínima:** manifiesto versionado y generado por release, instalación limpia reproducible, hashes o lockfiles aplicables y registro exacto de clientes usados por GATE o experimento.
- **Comportamiento ante fallo:** no promover una release cuando una versión requerida sea implícita, mutable o no reproducible; una diferencia detectada invalida la evidencia dependiente hasta repetirla o justificar su equivalencia.
- **Exclusiones MVP:** no se exige reproducción bit a bit en hardware distinto ni soporte simultáneo de versiones no listadas. La frase «última versión estable» orienta actualizaciones, pero no reemplaza el pin del release.

### CTRL-RESTORE-01 — Backup y restauración verificable

- **Obligación:** debe existir un procedimiento conjunto y versionado para respaldar PostgreSQL y assets `ready`, restaurarlos en un entorno aislado y reconciliar sus referencias. Deben declararse RPO, RTO, cifrado, acceso, retención y orden de recuperación antes del piloto.
- **Familias y superficies aplicables:** estado durable de cuentas, membresías, canvas, assets, sesiones o credenciales recuperables, outbox y metadatos necesarios para operación.
- **Issue propietario planificado:** `TE-017 — Verificar backup y restauración de PostgreSQL y assets`.
- **Evidencia mínima:** restauración periódica desde copias reales en un entorno limpio, consulta de integridad referencial y assets, medición observada de RPO/RTO y manifiesto durable sin secretos expuestos.
- **Comportamiento ante fallo:** no declarar el backup válido; conservar las copias anteriores, aislar la restauración fallida y bloquear el inicio del piloto o una recuperación productiva hasta obtener una restauración íntegra y reconciliada.
- **Exclusiones MVP:** no se requiere conmutación automática, recuperación multirregión ni cero pérdida. Un backup creado pero nunca restaurado no satisface el control.

### CTRL-READY-01 — Salud y preparación operativa

- **Obligación:** el proceso debe exponer una señal de vida separada de una señal de preparación. La preparación solo puede ser positiva cuando configuración, migraciones, PostgreSQL y recursos imprescindibles permiten aceptar trabajo; debe volverse negativa durante inicio, degradación no operable y cierre.
- **Familias y superficies aplicables:** servidor Go, despliegue, balanceador o supervisor, PostgreSQL, almacenamiento requerido por rutas críticas y publicador de outbox.
- **Issue propietario planificado:** `TE-016 — Operar salud, preparación y cierre ordenado`.
- **Evidencia mínima:** pruebas que distingan proceso vivo de servicio preparado, simulen dependencias requeridas caídas y confirmen transiciones de estado sin reinicios en bucle; procedimiento de diagnóstico con respuestas acotadas.
- **Comportamiento ante fallo:** retirar la instancia de servicio sin revelar configuración ni credenciales. Una dependencia opcional degradada debe identificarse sin convertir automáticamente la señal de vida en fallo.
- **Exclusiones MVP:** no se incorpora una plataforma de orquestación específica ni autorreparación compleja. La señal no garantiza por sí sola la corrección funcional de todos los recorridos.

### CTRL-SHUTDOWN-01 — Cierre ordenado

- **Obligación:** ante una señal de terminación, el proceso debe dejar de estar preparado, rechazar trabajo nuevo, drenar HTTP, cerrar WebSockets con motivo reintentable, detener consumidores y temporizadores, resolver o dejar reintentables outbox y reconciliaciones, terminar subprocesos y cerrar pools dentro de un presupuesto definido.
- **Familias y superficies aplicables:** servidor HTTP, WebSocket, actores de canvas, publicador de outbox, reconciliador S3, inspección de medios, telemetría y conexiones PostgreSQL/S3.
- **Issue propietario planificado:** `TE-016 — Operar salud, preparación y cierre ordenado`.
- **Evidencia mínima:** prueba de proceso con trabajo HTTP, WebSocket, outbox y ffmpeg en curso que envíe la señal, mida el presupuesto y demuestre ausencia de commits parciales, procesos huérfanos y conexiones aceptadas después del drenaje.
- **Comportamiento ante fallo:** al vencer el presupuesto, cancelar contextos y terminar recursos restantes; preservar operaciones durables como reintentables y emitir evidencia correlacionada antes del cierre cuando sea posible. Nunca confirmar al cliente una operación no durable para acelerar la salida.
- **Exclusiones MVP:** no se coordinan múltiples instancias ni migraciones en vivo. El objetivo de ≤2 s para conexiones individuales no implica que todo el proceso comparta ese mismo presupuesto.

<!-- CONTROL-CONTRACT:END -->

### Mantenimiento del contrato

`scripts/check-control-contract.sh` valida la lista aprobada y la integración mínima de los Issue Forms. El script detecta deriva estructural; la revisión humana debe validar el significado, la proporcionalidad y la evidencia de cada cambio. Un nuevo control transversal requiere un ID estable, un propietario planificado y la actualización coordinada del catálogo y del validador.
