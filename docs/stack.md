# Stack técnico de PepiCanvas

Este documento fija las tecnologías del MVP piloto y el motivo de cada elección. Cada decisión se vincula con el issue que la exige.

## Resumen

| Capa | Elección |
|---|---|
| Backend | Go (última versión estable, mínimo 1.26) con biblioteca estándar, **sin framework** |
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
| Consultas SQL | `sqlc` (generador, imagen fijada por digest en `scripts/sqlc.sh`) | SQL explícito con tipos generados; sin ORM, para controlar transacciones y bloqueos de fila (TE-004, TE-006) |
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
- **Primitivas de redacción (TE-003).** Los secretos viajan como `secret.Value`, que se redacta en toda salida de formato, texto, JSON y `slog`. Los tokens generados llevan prefijo por tipo (`pci_`, `pcs_`, `pco_`) para que `secret.Scrub` los redacte también en texto libre; `Scrub` elimina además credenciales, query y fragmento de las URLs. El handler de `logging` aplica esa limpieza a mensajes y atributos, redacta por completo las claves sensibles y agrega `correlation_id` desde el contexto (`platform/correlation`, 32 caracteres hexadecimales). `platform/httpx` aporta `Private`, que reimpone las cabeceras privadas al escribir la respuesta, `Correlate`, `Responder.Error`, con un vocabulario cerrado de errores que nunca refleja el error interno, y `Recover`. Un string sin prefijo bajo una clave no sensible no es detectable, por lo que todo secreto debe tiparse como `secret.Value`.
- **Persistencia atómica del canvas (TE-004).** Cada mutación de dimensiones, contenido, transformación, orden o eliminación corre en una única transacción que primero bloquea la fila del canvas (`SELECT ... FOR UPDATE`). Así las mutaciones de un canvas se confirman de a una, con una revisión sin huecos y un orden z único que sigue el orden de confirmación. La eliminación borra la fila, sin papelera ni estado recuperable. Cada mutación aceptada guarda en la misma transacción su identificador de operación, una huella SHA-256 de la mutación (sin la correlación) y su resultado. Un reintento equivalente devuelve ese resultado sin repetir el efecto, un identificador reutilizado con otra mutación se rechaza y la retención por defecto es de 10 minutos. Las migraciones viven en `backend/migrations` y se embeben en el binario.
- **Corte de acceso (TE-002).** `internal/revocation` aplica revocación de cuenta o sesión, desactivación del tenant y rotación de credencial OBS solo a partir de una confirmación del servidor: cada corte y cada grant llevan la versión confirmada en la que se leyeron los hechos de acceso, y un corte afecta solo a grants leídos antes que él, sin importar el orden de llegada. Al aplicarse, el lease de cada conexión afectada rechaza lecturas, escrituras y comandos y cancela su contexto antes de que termine el cierre del transporte. El transporte implementa un puerto `Conn` (`Close` con motivo y `Abort`): el cierre ordenado dispone de tres cuartos del presupuesto de 2 s y luego se aborta; un cierre fuera de presupuesto devuelve `ErrBudgetExceeded` y se registra como error. Un historial acotado por tenant rechaza la reconexión con grants anteriores al corte, y al desbordarse rechaza como vencidos los grants más viejos que lo retenido. Los reintentos con el mismo identificador de operación devuelven el mismo reporte (CTRL-IDEM-01). `Close` sigue el contrato de `Hub.Close` con motivo reintentable. El reloj empieza cuando el corte confirmado llega al registro; la entrega desde el commit pertenece a TE-012.
- **Cuentas y sesiones (US-001).** La identidad es el email, normalizado sin espacios y en minúsculas, con una contraseña de 12 a 128 caracteres guardada como hash argon2id (19 MiB, 2 pasadas, 1 hilo) en formato PHC. Un email desconocido y una contraseña incorrecta producen la misma respuesta `unauthorized` y el mismo costo, porque también se verifica un hash ficticio. La sesión vence tras 24 h sin uso o 7 días desde el inicio, lo que ocurra primero. Su token (`pcs_`) viaja solo en la cookie `__Host-pcs` (`HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`). El token CSRF es el SHA-256 de una etiqueta y el token de sesión: no se almacena y no puede calcularse sin la cookie. El registro es idempotente por `operation_id` durante 10 minutos y solo conserva el SHA-256 del email. Un reintento es equivalente cuando el email coincide y la contraseña verifica contra el hash de la cuenta, así que nunca se guarda un hash rápido de la contraseña. El inicio de sesión no se repite por identificador de operación, porque repetirlo exigiría conservar el token. Un reintento crea otra sesión que vence sola. Un email ya registrado responde `identity_taken`; sin verificación de email no puede ocultarse sin romper el registro. Pedir un canvas exige una membresía aceptada (`memberships`) en un tenant con suscripción activa (`tenants`); toda denegación, incluido un tenant o canvas inexistente, responde el mismo `forbidden`. US-002 y US-005 escriben esos hechos.
- **Ingreso HTTP de US-001 (primitivas de CTRL-WEB-01 y CTRL-RATE-01).** Todo método no seguro exige que `Origin` sea exactamente `PEPICANVAS_ALLOWED_ORIGIN` antes de leer el cuerpo o tocar el dominio. Las mutaciones autenticadas exigen además `X-CSRF-Token`. Los límites viven en memoria, con ventana fija y tabla acotada que falla cerrada: inicio de sesión 10 por cuenta y 50 por IP cada 15 minutos, y registro 10 por IP por hora, con `429` y `Retry-After`. La IP es la del par TCP. Desde TE-014 estas rutas pasan por el ingreso general y sus límites responden a través de él.
- **Ingreso HTTP y WebSocket (TE-014, propietario de CTRL-WEB-01 y CTRL-RATE-01).** `internal/platform/ingress` protege todo el ingreso antes del dominio. Cada ruta con método no seguro se registra con una clase: `Anonymous` exige solo `Origin`, y `Session` exige además el token CSRF ligado a la cookie de sesión. El token se verifica antes de leer la sesión, porque `Authenticate` lee y puede borrar sesiones. Sin cookie no hay credencial que falsificar, y el handler responde `401`. Una ruta no segura sin clase responde `403`, así que olvidar la clase falla cerrado. `Origin` debe estar exactamente en la lista configurada. La configuración rechaza `*`, `null`, comodines, rutas, barra final, credenciales y toda forma que un navegador nunca envía (mayúsculas, puerto por defecto o vacío, caracteres no ASCII), y una configuración inválida detiene el proceso antes de abrir PostgreSQL o escuchar. CORS queda cerrado: nunca se escribe una cabecera `Access-Control-*` y todo `OPTIONS` a la API responde `403`. Toda respuesta de la API lleva `nosniff`, `X-Frame-Options: DENY`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`, `Cross-Origin-Resource-Policy` y `Cross-Origin-Opener-Policy` `same-origin`, y HSTS de un año cuando el origen es HTTPS. Los archivos estáticos llevan las mismas protecciones de encuadre y transporte, y solo aceptan `GET` y `HEAD`. `Upgrade` protege cada upgrade WebSocket: verifica `Origin` exacto, la forma del handshake y los límites de apertura por sesión y por IP antes de ceder la conexión. Todavía no existe una ruta WebSocket, así que la primera que se agregue debe montarse con `Upgrade`. Los límites viven en memoria, con ventana fija y tabla acotada de 100 000 claves que falla cerrada. Por defecto: 300 métodos no seguros por IP, 120 mutaciones por sesión, 30 aperturas WebSocket por IP y 10 por sesión, todos por minuto. A eso se suman los de inicio de sesión y registro de US-001. Responden `429` con `Retry-After`. La clave de sesión es el SHA-256 del token, así que la tabla nunca guarda tokens. Un rechazo por `Origin` no gasta cupo y uno por CSRF no gasta el de la sesión, pero todo método no seguro que pasa `Origin` y la clase de su ruta gasta el cupo de su IP. En un upgrade, el cupo de sesión solo se consulta antes de la IP y se gasta después de que la IP admitió la apertura, así que un intento rechazado no ocupa lugar en la tabla de sesiones. Cada cuenta o sesión tiene su propio cupo; la única dimensión compartida entre streamers es la IP. La IP es la del par TCP y se ignoran `X-Forwarded-For`, `X-Real-IP` y `Forwarded`, por lo que un despliegue detrás de un proxy necesita una decisión explícita posterior. Cada rechazo responde sin reflejar datos de la solicitud y queda registrado con motivo, superficie, patrón de ruta registrado (o `unmatched`) y `correlation_id`. `Snapshot` cuenta los rechazos por superficie (`http`, `websocket`) y motivo, ambos de un conjunto cerrado, sin dimensiones de origen, ruta ni identidad. La exportación de métricas corresponde a TE-015 y la preparación del proceso a TE-016. Prueba retroactiva (regla 8). CTRL-WEB-01 queda demostrado sobre US-001, US-002, TE-001 (el rechazo ocurre antes de la matriz de autorización, sin reemplazarla) y TE-003 (correlación y redacción en cada rechazo). Para TE-002, TE-007, TE-008 y TE-009 no aplica: no exponen ruta ni upgrade, y la primera ruta WebSocket debe usar `Upgrade`. CTRL-RATE-01 queda demostrado sobre US-001. Para TE-005 y TE-006 no aplica: ninguna ruta HTTP llega a la reserva ni a la inspección, y la historia que agregue la carga debe aplicar su límite con `Ingress.Allow` antes de reservar cuota o ejecutar ffmpeg.
- **Activación manual de streamers (US-002).** Un streamer es un tenant con una cuenta dueña (`tenants.owner_account_id`, única) y un único canvas (`tenants.canvas_id`), creado con 1920×1080. El rol operativo vive en la tabla `operators` y se otorga desde el host con `server grant-operator <email>`; no se revoca en el MVP. `POST /api/activations` (`operation_id`, `email`) exige sesión, `Origin` y token CSRF antes de leer el cuerpo. En una sola transacción, `internal/subscription` lee el permiso del operador con `FOR SHARE` y lo evalúa con TE-001 antes de buscar la cuenta, así que alguien sin rol recibe `forbidden` sin poder enumerar emails. Después bloquea la fila de la cuenta con `FOR UPDATE`, lo que serializa activaciones concurrentes, y crea tenant, membresía `owner` y canvas, o reactiva el tenant existente sin cambiar su canvas. Una falla no deja nada confirmado. Repetir la activación con otro `operation_id` devuelve el mismo canvas; el mismo `operation_id` devuelve la misma respuesta durante 10 minutos y uno reutilizado con otro operador o email responde `conflict` (CTRL-IDEM-01). Un email desconocido responde `not_found` solo a un operador. El registro de la activación guarda el operador, el resultado y el `correlation_id`. `GET /api/canvas` devuelve el canvas del tenant propio con suscripción activa; cualquier otro caso responde el mismo `forbidden`. El editor muestra ese canvas y, a los operadores, un formulario de activación. La publicación por outbox pertenece a TE-012, y la evidencia de esta superficie alimenta GATE-001.
- **Desactivación y reactivación de suscripciones (US-003).** `POST /api/deactivations` (`operation_id`, `email`) pasa por el ingreso de TE-014 con clase `Session`, igual que la activación: `Origin` y token CSRF antes de leer la sesión, sesión antes de leer el cuerpo, permiso del operador con `FOR SHARE` evaluado con TE-001 (`deactivate`) antes de buscar la cuenta y otra vez antes del commit, y la misma idempotencia de 10 minutos (CTRL-IDEM-01). Activación y desactivación comparten el espacio de identificadores de operación: ambas toman un lock transaccional (`pg_advisory_xact_lock`) por `operation_id` después de verificar el permiso del operador, y un identificador retenido por una acción responde `conflict` en la otra, sin efecto. En una sola transacción pone el tenant `inactive`, incrementa `tenants.access_version` (la versión confirmada de los hechos de acceso), guarda el registro de la operación y escribe un corte pendiente en `access_cuts` (CTRL-CONS-01). El esquema vive en la migración `00008_deactivation.sql`. Desactivar un tenant ya inactivo no cambia nada y no escribe corte. El corte es efectivo en el commit: cada lectura HTTP vuelve a leer la suscripción, y `CanvasStore.Apply` lee la fila del tenant con `FOR SHARE` después de bloquear el canvas, así que una mutación confirmada antes de la desactivación persiste y una posterior se rechaza con `canvas.ErrInactive` (una repetición de una operación ya retenida sigue devolviendo su resultado). `subscription.CutRelay` entrega cada corte pendiente al registro de TE-002 como desactivación del tenant (`cut-<id>`) apenas se confirma y además consulta cada segundo y al iniciar, así que un corte que no llegó a publicarse antes de una caída se aplica al reiniciar; un registro cerrado o una falla del store lo dejan pendiente, y el registro deduplica la reentrega. Los leases de editor y OBS del tenant terminan al aplicarse el corte y sus conexiones cierran en ≤2 s con motivo `subscription_inactive`; un grant leído antes del corte no vuelve a registrarse, y `Service.EditorGrant` solo emite grants para el dueño de un tenant activo, en su versión de acceso. `cmd/server` arranca el registro y el relay antes de escuchar y los cierra después de drenar HTTP. La reactivación es la activación de US-002: devuelve el mismo tenant y canvas, con dimensiones, objetos y archivos intactos, sin reanudar reproducción efímera. El dueño de un tenant desactivado recibe en `GET /api/canvas` el código `subscription_inactive` (solo sobre su propio tenant); toda otra denegación sigue siendo `forbidden`. El editor lo explica y el panel de operador permite desactivar y reactivar por email. El cierre del WebSocket del editor y de la página OBS reales pertenece a US-017 y US-021, que deben registrar sus conexiones en el registro con grants de `EditorGrant` o equivalentes. CTRL-WEB-01 queda demostrado sobre US-003 con el ingreso de TE-014. Incumplir el corte bloquea GATE-001.
- **Binario del servidor.** `backend/cmd/server` migra PostgreSQL al iniciar, sirve `/api/` y, con `PEPICANVAS_STATIC_DIR`, el frontend compilado desde el mismo origen, con los HTML en `no-store`. Se configura con `PEPICANVAS_DATABASE_URL`, `PEPICANVAS_ALLOWED_ORIGIN` y `PEPICANVAS_LISTEN_ADDR` (por defecto `127.0.0.1:8080`). En desarrollo, Vite reenvía `/api` a `PEPICANVAS_API_TARGET`. Ante SIGTERM drena HTTP durante 5 s; el cierre completo del proceso corresponde a TE-016.
- **Concesiones de bloqueo (TE-009).** `internal/locking` concede un bloqueo exclusivo por objeto, identificado por tenant, canvas y objeto. Una adquisición denegada y `Lookup` muestran al titular. Cada concesión pertenece a una sesión ligada al contexto de su conexión, que debe ser el del lease de TE-002. Cuando ese contexto termina por desconexión o corte de acceso, todas las concesiones de la sesión se liberan al instante. Una reconexión es otra sesión y nunca recupera concesiones anteriores. La señal de vida (heartbeat) de la sesión renueva todas sus concesiones e informa las perdidas. Una concesión vence exactamente 5 s después de la última señal aceptada: un temporizador la libera y además cada acceso verifica el plazo, de modo que una señal que llega en el plazo exacto ya no se acepta. La autorización se reevalúa antes de cada adquisición y en cada señal de vida, y cualquier error deniega la adquisición o libera la concesión. La adquisición y la liberación se deduplican por identificador de operación dentro de cada cuenta. Un reintento tras reconectar devuelve la respuesta original, que no implica que la concesión siga vigente (CTRL-IDEM-01). La señal de vida es presencia efímera y queda excluida. `Close` sigue el contrato de `Hub.Close`: rechaza trabajo nuevo, libera todas las concesiones, detiene los temporizadores y drena las autorizaciones en curso. Los registros llevan el `correlation_id` de la operación que fijó el plazo, incluso en la expiración asíncrona, y nunca los identificadores del titular. Las concesiones viven en memoria y no se persisten.
- **Resincronización desde estado autorizado (TE-008).** `internal/resync` devuelve el estado confirmado en PostgreSQL, leído por `canvas.Store.Load` en una sola transacción `RepeatableRead`, cuya `revision` sirve de cursor. Nunca reproduce comandos históricos ni restaura estado efímero: el snapshot no lleva bloqueos, cursores ni presencia, y cada video o audio llega detenido en la posición cero. `Service.Snapshot` corre bajo el lease de la conexión (TE-002) y reautoriza con TE-001 antes de leer y de nuevo justo antes de entregar. Una conexión denegada, cortada, liberada o cerrada por apagado recibe un snapshot vacío y un error, y si el acceso ya estaba denegado no se lee el store. El snapshot devuelve la correlación de la solicitud; como módulo de dominio, `resync` no importa `platform`. Los eventos de canvas se numeran por la revisión que produjo cada mutación (`resync.Event`) y `canvas.Fold` los aplica. La réplica de referencia ignora duplicados, retiene eventos fuera de orden hasta un máximo acotado y detecta discontinuidades: un hueco, una revisión anunciada por `Notice` que no tiene o un evento inconsistente. En esos casos adopta un snapshot nuevo y descarta los eventos de revisión igual o menor. Un snapshot repetido o viejo no cambia nada. La publicación de esos eventos desde el commit pertenece a TE-012.
- **Outbox PostgreSQL (TE-012).** `CanvasStore.Apply` escribe, en la misma transacción y bajo el bloqueo de la fila del canvas, un registro de `canvas_outbox` por mutación aceptada, con el `resync.Event` (mutación, resultado y revisión), la clave del tenant y canvas autorizados y el `correlation_id`. Una repetición, un conflicto o una mutación rechazada no escriben nada, y una falla antes o durante el commit no deja ni dominio ni registro. `internal/outbox.Publisher` entrega los pendientes en orden de revisión por canvas, sin Redis, Kafka ni otro broker. Un commit lo despierta (`WithCommitted(publisher.Notify)`) y además consulta cada segundo, lo que también recupera al iniciar lo que quedó pendiente de una caída. Cada intento tiene un límite de 5 s; un registro que falla bloquea solo los siguientes de su canvas y se reintenta con espera exponencial de 100 ms a 30 s. `HubSink` entrega al actor del canvas (TE-007) bajo el operation ID `outbox-<id>`, así que una reentrega se reconoce y produce la misma respuesta con un único efecto (CTRL-IDEM-01). Un comando inválido, un conflicto o un acceso denegado se resuelven `rejected` sin reintento; un registro que no se puede decodificar también, sin frenar a los demás canvas. Los registros resueltos se purgan después de 24 h en lotes acotados. `OutboxStore.Stats` (por tenant) y `AllStats` muestran pendientes, antigüedad del más viejo, reintentos y rechazados; `Publisher.Counters` cuenta entregas, rechazos, inválidos, reintentos, purgas y fallas del store. `Running` y `Close`, con el contrato de `Hub.Close`, son las primitivas de CTRL-READY-01 y CTRL-SHUTDOWN-01 para TE-016. `Close` deja pendiente todo lo no entregado y marca el hub con `RequireResync`, así que los clientes se recuperan desde PostgreSQL. La entrega es al menos una vez y en una sola instancia. `cmd/server` todavía no arranca un publicador porque ninguna ruta aplica mutaciones de canvas; la primera que lo haga lo arranca en el mismo proceso.
- **Inspección de medios.** `ffprobe` determina el tipo real, los códecs, la duración y la resolución. `ffmpeg -f null` decodifica el archivo completo, incluidos todos los fotogramas de un GIF, antes de aceptarlo (TE-005). Corre como subproceso con límite de tiempo.
- **Cuota y ciclo de vida de los assets (TE-006).** Cada canvas tiene una fila `media_usage` con una cuota inclusiva de 1 000 000 000 bytes, los bytes usados por assets `ready` y los reservados por cargas en curso. Toda reserva, finalización, abandono, asociación y liberación bloquea esa fila (`SELECT ... FOR UPDATE`) y se confirma en una única transacción, y un `CHECK` impide que `used + reserved` supere la cuota. Cada carga cuenta su tamaño declarado completo, aunque duplique contenido. Un asset pasa de `pending` a `ready` solo tras inspeccionarse. Un rechazo lo deja en `quarantined` y una falla, cancelación, reserva vencida (30 min) o la liberación de su última referencia lo deja en `deleting`; en todos los casos se libera su cuota en la misma transacción. Solo `ready` se asocia o se sirve. Un reemplazo reserva el tamaño completo del archivo nuevo, y al completarse mueve la referencia y libera el anterior si no le quedan otras. Las operaciones se deduplican por identificador durante 10 minutos, como en TE-004. `media.Uploads` reserva, guarda los bytes con una clave aleatoria interna, los inspecciona y completa. Ante cualquier falla posterior a la reserva abandona el asset con un contexto separado de la cancelación, borra los bytes y lo olvida; si el borrado falla, el asset queda aislado hasta `Sweep` o el reconciliador de TE-013. `platform/s3` usa `minio-go` con un único PUT por objeto, sin multipart, con el tamaño exacto y sin hashear el cuerpo en memoria, de modo que una carga fallida no deja un objeto legible. El PUT es condicional (`If-None-Match: *`): nunca reemplaza un objeto existente, así que un duplicado concurrente de una carga no puede cambiar los bytes ya inspeccionados ni abandonar el intento original: recibe su resultado cuando quedó `ready`, repite el abandono registrado si fue abandonado y, mientras sigue en curso, responde `ErrUploadInProgress` sin tocar la reserva ni los bytes. Los errores de configuración no incluyen la causa del cliente para no repetir credenciales. Las pruebas usan MinIO fijado por digest (`cgr.dev/chainguard/minio`, porque `minio/minio` ya no se publica en Docker Hub).
- **Reconciliación de assets (TE-013).** Cada objeto guardado lleva como metadatos el tenant, el canvas y el asset que lo poseen (`X-Amz-Meta-Pepicanvas-*`), y `media_assets.state_changed_at` registra cuándo entró el asset en su estado actual. `media.Reconciler` corre una pasada al iniciar, cada 5 minutos y bajo demanda (`Run`, `Trigger`), y las pasadas se serializan. Cada pasada tiene tres pasos. Primero borra los bytes y olvida los assets `quarantined`, `deleting` y las reservas vencidas. Después recorre el bucket: un objeto que ninguna fila retiene se borra solo si sus metadatos nombran un dueño válido, supera el período de gracia (1 h) y la base confirma que ese asset no existe. Un objeto sin metadatos, reciente o reclamado por un asset existente solo se cuenta. Por último, un asset `ready` cuyo objeto falta se reporta sin modificar su fila; `Open` ya responde no encontrado, así que nunca se sirve contenido incompleto. Todo estado vive en PostgreSQL, por eso una pasada interrumpida o repetida converge con un único efecto por asset. El reporte de cada pasada solo trae conteos, su `correlation_id` e instantes, sin identificadores ni claves (primitiva para TE-015). El inventario por tenant agrupa cantidad, bytes, antigüedad y el asset más antiguo por estado, sin nombres privados. `Close` sigue el contrato de `Hub.Close` y deja el trabajo pendiente en su estado persistido, sin borrado ciego (primitiva para TE-016). El reconciliador solo necesita un `Store` y un `ObjectStore`, así que puede correr sobre una restauración aislada (TE-017).

### Estructura (screaming + hexagonal)

```text
backend/
  cmd/server/            # composición e inicio del binario
  internal/
    access/              # autorización, sesiones, invitaciones, membresías (TE-001, US-001..006)
    subscription/        # activación y desactivación de streamers (US-002, US-003)
    canvas/              # canvas, objetos, transformaciones, orden z (TE-004, US-007..)
    realtime/            # actor por canvas, conexiones, resincronización (TE-007, TE-008)
    revocation/          # corte de acceso confirmado y cierre de conexiones en ≤2 s (TE-002)
    locking/             # concesiones con heartbeat (TE-009)
    media/               # inspección, cuota, almacenamiento y reconciliación (TE-005, TE-006, TE-013)
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

Ambos documentos declaran `<meta name="referrer" content="no-referrer">` e instalan `installWindowRestoreGuard()`: al salir hacia la caché de ida y vuelta (`pagehide` con `persisted`) ocultan el documento, y al restaurarse (`pageshow` con `persisted`) recargan para que el servidor vuelva a decidir el acceso. `Cache-Control: no-store` no basta: Chrome restauró desde esa caché una página privada después de un corte de acceso en el servidor (TE-003).

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
- **GitHub Actions** para CI: `golangci-lint`, `go test -race`, `sqlc diff` (`scripts/sqlc.sh diff`), ESLint, `prettier --check`, Vitest y Playwright.

## Pruebas

| Nivel | Herramienta | Alcance |
|---|---|---|
| Dominio Go | `testing` + pruebas en tabla + `-race` | Matriz de autorización con dos tenants por caso (TE-001), orden y deduplicación |
| Tiempo | `testing/synctest` | Heartbeat de 5 s y cierres en ≤2 s sin esperas reales |
| Integración Go | `testcontainers-go` | PostgreSQL y MinIO reales, atomicidad tras reinicio (TE-004). Sin Docker las pruebas se omiten; con `PEPICANVAS_REQUIRE_POSTGRES=1`, como en CI, fallan |
| Frontend | Vitest | Lógica del almacén y del renderizador |
| E2E | Playwright (canales `chrome` y `msedge`) | Matriz de navegadores soportados (TE-011) |
| OBS | Procedimiento propio | Playwright no puede automatizar OBS; se definirá en TE-011 |
| Cuentas y sesiones | Playwright + servidor Go real que sirve el build + PostgreSQL en contenedor (`npm run auth:e2e`) | Por cada navegador de la matriz: registro, duplicado, credenciales inválidas, recarga con sesión válida y sesión terminada que no se restaura desde la caché de ida y vuelta (US-001) |
| Privacidad | Playwright + fixture Go de prueba (`npm run privacy:matrix`) | Por cada navegador de la matriz, señuelos nuevos; se escanean el log del fixture, la salida de Playwright y `test-results`, se exige `correlation_id` en cada registro de solicitud y se prueba que ningún señuelo anterior reaparece (TE-003) |

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
6. **Primitiva temprana.** Cuando un consumidor debe cerrar antes que el propietario, el catálogo puede asignar una primitiva temprana: la parte mínima del control que ese consumidor puede construir y demostrar sin la capacidad del propietario. La primitiva no satisface el control completo.
7. **Diferimiento.** Un consumidor puede declarar `<ID> — Diferido a <TE-0XX>: <evidencia de la primitiva o de la parte propia>` solo si el propietario sigue abierto cuando el consumidor cierra. El diferimiento es obligatorio cuando el propietario depende del consumidor. Si el propietario ya cerró, el consumidor debe demostrar el control completo con la capacidad existente y reemplazar la línea diferida antes de cerrar.
8. **Prueba retroactiva.** Antes de cerrar, el propietario debe demostrar el control sobre cada consumidor ya cerrado con una línea diferida hacia él, o registrar para ese consumidor `No aplica` con justificación. La lista se calcula al cerrar el propietario; la que figura en su issue es el mínimo conocido, no un límite.
9. **Issues cerrados antes de las reglas 6 a 8.** TE-001, TE-003, TE-005, TE-007 y TE-011 se tratan como si hubieran declarado una línea diferida por cada control que listan y cuyo propietario seguía abierto al cerrarse. No se reabren; sus propietarios los incluyen en la prueba retroactiva.

### Definition of Ready y Definition of Done

Un issue está **Ready** cuando identifica los controles aplicables, enlaza sus propietarios, especifica evidencia repetible y resuelve cualquier dependencia que impida verificarla, salvo los controles diferidos según la regla 7. Está **Done** cuando conserva la evidencia declarada, verifica los fallos exigidos y no introduce una excepción sin documentarla en este contrato. La Definition of Done del issue debe contener los resultados concretos; no debe duplicar la prosa del catálogo.

<!-- CONTROL-CONTRACT:START -->

### CTRL-CONS-01 — Consistencia entre PostgreSQL y WebSocket

- **Obligación:** toda mutación durable debe confirmar primero la transacción de dominio y un evento de outbox en la misma transacción PostgreSQL. Un publicador reintentable entrega después el evento al actor del canvas y marca su entrega; ninguna confirmación WebSocket puede representar estado que no quedó confirmado en PostgreSQL.
- **Familias y superficies aplicables:** mutaciones de canvas, objetos, miembros, suscripciones, credenciales OBS y cualquier operación durable que produzca una notificación WebSocket.
- **Issue propietario planificado:** `TE-012 — Publicar mutaciones durables mediante outbox PostgreSQL`.
- **Primitiva temprana:** `TE-004` confirma cada mutación del canvas en una única transacción PostgreSQL, a la que `TE-012` incorpora el evento de outbox.
- **Evidencia mínima:** prueba de integración con PostgreSQL real que fuerce una caída entre el commit y la publicación, reinicie el publicador y demuestre entrega posterior sin pérdida; métricas o consulta que permitan observar pendientes y antigüedad.
- **Comportamiento ante fallo:** conservar el evento pendiente, reintentar con espera acotada y no revertir un commit ya confirmado. Si no puede publicarse, el cliente debe recuperar el estado por resincronización; la cola pendiente debe quedar observable y operable.
- **Exclusiones MVP:** no se incorporan Redis, Kafka ni otro broker; no se promete entrega exactamente una vez ni coordinación entre múltiples instancias. El orden autoritativo sigue siendo por canvas y la entrega es al menos una vez.

### CTRL-S3-01 — Reconciliación entre PostgreSQL y almacenamiento S3

- **Obligación:** PostgreSQL debe registrar el estado del asset como `pending`, `ready`, `quarantined` o `deleting`. Solo `ready` puede asociarse a objetos o servirse. La carga, inspección y eliminación deben ser reanudables, y un reconciliador debe detectar objetos temporales, registros incompletos y eliminaciones pendientes.
- **Familias y superficies aplicables:** imágenes, GIF, audio, video, miniaturas y cualquier flujo que escriba o elimine objetos en MinIO, S3 o un servicio compatible.
- **Issue propietario planificado:** `TE-013 — Reconciliar assets entre PostgreSQL y almacenamiento S3`.
- **Primitiva temprana:** `TE-006` registra los estados `pending`, `ready`, `quarantined` y `deleting` y solo asocia o sirve assets `ready`; `TE-013` aporta el reconciliador, los metadatos de propiedad y el inventario por estado y edad.
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
- **Primitiva temprana:** `TE-003` (cerrado) aporta `platform/correlation`, la propagación por contexto, `httpx.Correlate` y `correlation_id` en los registros redactados; `TE-007` (cerrado) transporta la correlación del comando al resultado y al evento. Los consumidores previos a `TE-015` registran esa correlación en su recorrido.
- **Evidencia mínima:** prueba de integración o recorrido instrumentado que muestre una correlación continua a través de al menos una frontera asíncrona; consulta reproducible de logs o trazas por identificador y verificación de redacción.
- **Comportamiento ante fallo:** la indisponibilidad del exportador no debe bloquear el recorrido de producto ni acumular memoria sin límite; debe descartar de forma acotada, exponer la pérdida mediante métricas y mantener logs locales correlacionados.
- **Exclusiones MVP:** no se exige un backend específico de telemetría, trazado exhaustivo de cada frame ni alta disponibilidad del colector. El muestreo no puede eliminar recorridos de seguridad o evidencia obligatoria definidos por una validación.

### CTRL-EVID-01 — Evidencia durable y evaluable

- **Obligación:** la evidencia exigida por un issue o control del piloto debe almacenarse fuera de buffers de proceso con versión de release, escenario, instante, resultado, correlación y ubicación verificable. Debe ser legible por una persona evaluadora autorizada y estar redactada antes de persistirse.
- **Familias y superficies aplicables:** pruebas de integración y E2E, GATE, experimentos, incidentes de seguridad, métricas de latencia, restauraciones, reconciliaciones y verificaciones de compatibilidad OBS.
- **Issue propietario planificado:** `TE-015 — Correlacionar operaciones y conservar evidencia operativa`.
- **Primitiva temprana:** `TE-011` (cerrado) aporta el manifiesto de evidencia por release con integridad SHA-256; `TE-010` emite muestras con release, escenario, marcas de tiempo y correlación que `TE-015` vuelve durables.
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
- **Primitiva temprana:** `TE-007` (cerrado) aporta `Hub.Close`, que rechaza trabajo nuevo, drena lo encolado y no deja goroutines ni temporizadores. `TE-002`, `TE-012`, `TE-013` y `TE-015` exponen ese mismo contrato de cierre por componente; `TE-016` los compone a nivel de proceso.
- **Evidencia mínima:** prueba de proceso con trabajo HTTP, WebSocket, outbox y ffmpeg en curso que envíe la señal, mida el presupuesto y demuestre ausencia de commits parciales, procesos huérfanos y conexiones aceptadas después del drenaje.
- **Comportamiento ante fallo:** al vencer el presupuesto, cancelar contextos y terminar recursos restantes; preservar operaciones durables como reintentables y emitir evidencia correlacionada antes del cierre cuando sea posible. Nunca confirmar al cliente una operación no durable para acelerar la salida.
- **Exclusiones MVP:** no se coordinan múltiples instancias ni migraciones en vivo. El objetivo de ≤2 s para conexiones individuales no implica que todo el proceso comparta ese mismo presupuesto.

<!-- CONTROL-CONTRACT:END -->

### Mantenimiento del contrato

`scripts/check-control-contract.sh` valida la lista aprobada y la integración mínima de los Issue Forms. El script detecta deriva estructural; la revisión humana debe validar el significado, la proporcionalidad y la evidencia de cada cambio. Un nuevo control transversal requiere un ID estable, un propietario planificado y la actualización coordinada del catálogo y del validador.
