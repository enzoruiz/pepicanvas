# Borradores de edición de issues: propiedad de controles transversales

Textos exactos para aplicar en GitHub después de la revisión humana. Cada bloque `controles` reemplaza por completo la sección `## Controles técnicos transversales` del issue o se agrega al final si no existe. Cada bloque `terminado` se agrega al final de `### Definición de Terminado`. Las reglas citadas son las de `docs/stack.md`, sección «Aplicación desde el backlog».

## #6 TE-004

```controles
## Controles técnicos transversales

CTRL-CONS-01 — Diferido a TE-012 (TE-012 depende de TE-004): cada mutación de dimensiones, contenido, transformación, orden y eliminación se confirma en una única transacción PostgreSQL, a la que TE-012 incorpora el evento de outbox.
CTRL-IDEM-01 — reintentos de TE-004 (#6) antes y después de timeout o reconexión producen un único efecto durable y la misma respuesta, con la semántica de identificador de operación de TE-007; un payload distinto con el mismo identificador se rechaza y la retención acotada se prueba en su vencimiento.
CTRL-TRACE-01 — Diferido a TE-015: cada operación de persistencia registra el `correlation_id` de `platform/correlation` (TE-003), con redacción verificada.
CTRL-RESTORE-01 — Diferido a TE-017 (TE-017 depende de TE-004 a través de TE-012): sin primitiva propia; TE-017 restaura canvas, objetos, transformaciones y orden, y verifica que reaparecen idénticos.
```

## #7 TE-002

```controles
## Controles técnicos transversales

CTRL-CONS-01 — Diferido a TE-012: el corte se aplica solo a partir de una confirmación del servidor y ninguna conexión recibe un evento no confirmado.
CTRL-IDEM-01 — reintentos de TE-002 (#7) de una misma revocación, desactivación o rotación producen un único corte y la misma respuesta; un payload distinto con el mismo identificador de operación se rechaza.
CTRL-WEB-01 — Diferido a TE-014: sin primitiva propia; TE-014 valida CSRF y Origin en las rutas y el upgrade que usa este mecanismo.
CTRL-TRACE-01 — Diferido a TE-015: la confirmación y el cierre de cada conexión afectada registran el mismo `correlation_id` (TE-003), con redacción verificada.
CTRL-RESTORE-01 — Diferido a TE-017: sin primitiva propia; TE-017 verifica que una restauración no reactiva accesos revocados ni credenciales rotadas.
CTRL-SHUTDOWN-01 — Diferido a TE-016 (TE-016 depende de TE-002): el registro de conexiones expone un cierre con el contrato de `Hub.Close` de TE-007: rechaza conexiones nuevas, cierra las existentes con motivo reintentable dentro del presupuesto y no deja goroutines ni temporizadores.
```

## #8 TE-006

```controles
## Controles técnicos transversales

CTRL-CONS-01 — Diferido a TE-012: reserva, éxito, cancelación, reemplazo y liberación se confirman cada uno en una única transacción PostgreSQL.
CTRL-S3-01 — Diferido a TE-013 (TE-013 depende de TE-006): PostgreSQL registra los estados `pending`, `ready`, `quarantined` y `deleting`; solo `ready` se asocia, se sirve o se contabiliza como utilizable, y cada transición interrumpida deja un estado reintentable o aislado que TE-013 reconcilia.
CTRL-IDEM-01 — reintentos de TE-006 (#8) antes y después de timeout o reconexión producen una única reserva o liberación y la misma respuesta; un payload distinto con el mismo identificador de operación se rechaza y la retención acotada se prueba en su vencimiento.
CTRL-RATE-01 — Diferido a TE-014: sin primitiva propia; TE-014 limita la reserva y la inspección antes de consumir cuota o ejecutar ffmpeg.
CTRL-MEDIA-01 — un medio rechazado, vencido o interrumpido durante la inspección de TE-005 libera su reserva y no deja temporales ni archivos recuperables.
CTRL-TRACE-01 — Diferido a TE-015: cada reserva, inspección y liberación registra el mismo `correlation_id` (TE-003), con redacción verificada.
CTRL-RESTORE-01 — Diferido a TE-017 (TE-017 depende de TE-006): sin primitiva propia; TE-017 verifica después de restaurar que la cuota contabilizada coincide con los assets `ready` y las reservas vigentes.
```

## #36 TE-010

```controles
## Controles técnicos transversales

CTRL-TRACE-01 — Diferido a TE-015 (TE-015 depende de TE-010): cada muestra aceptación→render conserva el `correlation_id` de TE-003 y la correlación que TE-007 transporta del comando al evento; TE-015 la vuelve consultable a través de persistencia, publicación, cliente remoto y render.
CTRL-EVID-01 — Diferido a TE-015 (TE-015 depende de TE-010): cada muestra emite release, escenario, marcas de tiempo y correlación suficientes para recalcular la latencia; TE-015 la conserva de forma durable, íntegra y relegible.
CTRL-VERS-01 — el manifiesto de TE-010 (#36) fija release, Go, módulos, Node.js, gestor de paquetes, dependencias frontend, PostgreSQL, almacenamiento S3, ffmpeg/ffprobe, navegadores y OBS exactos; cualquier diferencia obliga a repetir la evidencia afectada.
```

## #40 TE-012

```controles
## Controles técnicos transversales

CTRL-CONS-01 — Propio: una prueba de integración con PostgreSQL real fuerza una caída entre commit y publicación, reinicia el publicador y demuestra entrega posterior sin pérdida ni confirmación WebSocket fantasma; pendientes y antigüedad son consultables.
CTRL-IDEM-01 — la reentrega del publicador no repite efectos en el actor del canvas: un evento ya aplicado se reconoce por su identidad y produce la misma respuesta.
CTRL-TRACE-01 — Diferido a TE-015: cada evento de outbox conserva el `correlation_id` de la mutación que lo originó y lo registra al publicarse.
CTRL-READY-01 — Diferido a TE-016 (TE-016 depende de TE-012): el publicador expone si está operativo y el tamaño de su cola pendiente, para que TE-016 lo use en la preparación.
CTRL-SHUTDOWN-01 — Diferido a TE-016 (TE-016 depende de TE-012): el publicador expone un cierre con el contrato de `Hub.Close` y deja reintentable todo evento no entregado.
CTRL-RESTORE-01 — Diferido a TE-017 (TE-017 depende de TE-012): sin primitiva propia; TE-017 verifica que el outbox restaurado no pierde eventos confirmados ni publica eventos sin estado de dominio.

Prueba retroactiva de CTRL-CONS-01 (regla 8): TE-005 y TE-007 (cerrados), TE-004 (cierra antes por dependencia) y todo consumidor cerrado con `CTRL-CONS-01 — Diferido a TE-012` antes de cerrar TE-012.
```

```terminado
- [ ] La prueba retroactiva de CTRL-CONS-01 cubre, o justifica como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-004, TE-005 y TE-007.
```

## #41 TE-013

```controles
## Controles técnicos transversales

CTRL-S3-01 — Propio: pruebas de integración interrumpen cada transición antes y después de escribir en S3, ejecutan la reconciliación y demuestran convergencia; el inventario por estado y edad es consultable sin exponer nombres privados.
CTRL-IDEM-01 — repetir una reconciliación o reanudarla después de una interrupción produce un único efecto por asset.
CTRL-TRACE-01 — Diferido a TE-015: cada ejecución de reconciliación registra su `correlation_id` (TE-003) sin exponer nombres privados.
CTRL-EVID-01 — Diferido a TE-015: cada reconciliación emite resultado, instante y correlación suficientes para que TE-015 la conserve como evidencia durable.
CTRL-SHUTDOWN-01 — Diferido a TE-016 (TE-016 depende de TE-013): el reconciliador expone un cierre con el contrato de `Hub.Close` y deja reanudable el trabajo pendiente.
CTRL-RESTORE-01 — Diferido a TE-017 (TE-017 depende de TE-013): el reconciliador puede ejecutarse sobre una restauración aislada para que TE-017 verifique la convergencia de los assets.

Prueba retroactiva de CTRL-S3-01 (regla 8): TE-005 (cerrado), TE-006 (cierra antes por dependencia) y todo consumidor cerrado con `CTRL-S3-01 — Diferido a TE-013` antes de cerrar TE-013.
```

```terminado
- [ ] La prueba retroactiva de CTRL-S3-01 cubre, o justifica como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-005 y TE-006.
```

## #42 TE-014

```controles
## Controles técnicos transversales

CTRL-WEB-01 — Propio: pruebas de integración para Origin permitido, ausente y hostil y para token CSRF válido, faltante e inválido, con rechazo antes de ejecutar dominio, escribir archivos o abrir el WebSocket.
CTRL-RATE-01 — Propio: pruebas deterministas del límite y su ventana, respuesta 429 o cierre documentado, recuperación al vencer la ventana, métricas sin dimensiones no acotadas e independencia entre streamers.
CTRL-TRACE-01 — Diferido a TE-015: cada rechazo registra motivo, ruta y `correlation_id` (TE-003), con cardinalidad controlada y redacción verificada.

Prueba retroactiva de CTRL-WEB-01 (regla 8): TE-001, TE-003 y TE-007 (cerrados) y todo consumidor cerrado con `CTRL-WEB-01 — Diferido a TE-014` antes de cerrar TE-014.
Prueba retroactiva de CTRL-RATE-01 (regla 8): TE-005 (cerrado) y todo consumidor cerrado con `CTRL-RATE-01 — Diferido a TE-014` antes de cerrar TE-014.
```

```terminado
- [ ] Las pruebas retroactivas de CTRL-WEB-01 y CTRL-RATE-01 cubren, o justifican como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-001, TE-003, TE-005 y TE-007.
```

## #43 TE-015

```controles
## Controles técnicos transversales

CTRL-TRACE-01 — Propio: un recorrido instrumentado mantiene una correlación continua a través de al menos una frontera asíncrona, con consulta reproducible por identificador y redacción verificada.
CTRL-EVID-01 — Propio: un manifiesto por ejecución referencia artefactos durables con integridad, acceso y retención, y una lectura independiente recalcula o clasifica el resultado.
CTRL-VERS-01 — la evidencia durable referencia la release y las versiones del manifiesto de TE-011.
CTRL-SHUTDOWN-01 — Diferido a TE-016 (TE-016 depende de TE-015): el exportador expone un cierre con el contrato de `Hub.Close` y descarta de forma acotada lo que no alcance a exportar, sin bloquear el proceso.

Prueba retroactiva de CTRL-TRACE-01 (regla 8): TE-001, TE-003, TE-005 y TE-007 (cerrados), TE-010 (cierra antes por dependencia) y todo consumidor cerrado con `CTRL-TRACE-01 — Diferido a TE-015` antes de cerrar TE-015.
Prueba retroactiva de CTRL-EVID-01 (regla 8): TE-011 (cerrado), TE-010 (cierra antes por dependencia) y todo consumidor cerrado con `CTRL-EVID-01 — Diferido a TE-015` antes de cerrar TE-015.
```

```terminado
- [ ] Las pruebas retroactivas de CTRL-TRACE-01 y CTRL-EVID-01 cubren, o justifican como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-001, TE-003, TE-005, TE-007, TE-010 y TE-011.
```

## #44 TE-016

```controles
## Controles técnicos transversales

CTRL-READY-01 — Propio: pruebas distinguen proceso vivo de servicio preparado durante el inicio, la caída de dependencias requeridas y el cierre, sin reinicios en bucle.
CTRL-SHUTDOWN-01 — Propio: una prueba de proceso con trabajo HTTP, WebSocket, outbox y ffmpeg en curso envía la señal, mide el presupuesto y demuestra ausencia de commits parciales, procesos huérfanos y conexiones aceptadas después del drenaje.
CTRL-TRACE-01 — las transiciones de preparación y el cierre emiten registros correlacionados con la capacidad de TE-015.

Prueba retroactiva de CTRL-READY-01 (regla 8): TE-007 (cerrado), TE-012 (cierra antes por dependencia) y todo consumidor cerrado con `CTRL-READY-01 — Diferido a TE-016` antes de cerrar TE-016.
Prueba retroactiva de CTRL-SHUTDOWN-01 (regla 8): TE-007 (cerrado), TE-002, TE-012, TE-013 y TE-015 (cierran antes por dependencia) y todo consumidor cerrado con `CTRL-SHUTDOWN-01 — Diferido a TE-016` antes de cerrar TE-016.
```

```terminado
- [ ] Las pruebas retroactivas de CTRL-READY-01 y CTRL-SHUTDOWN-01 cubren, o justifican como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-002, TE-007, TE-012, TE-013 y TE-015.
```

## #45 TE-017

```controles
## Controles técnicos transversales

CTRL-RESTORE-01 — Propio: restauración desde copias reales en un entorno limpio, consulta de integridad referencial y de assets, RPO/RTO observados y manifiesto durable sin secretos expuestos.
CTRL-S3-01 — la restauración ejecuta el reconciliador de TE-013 y demuestra convergencia sin servir assets incompletos ni borrar contenido válido.
CTRL-EVID-01 — el ejercicio de restauración conserva su manifiesto durable con la capacidad de TE-015; si falta evidencia obligatoria, el resultado es INCONCLUSO.

Prueba retroactiva de CTRL-RESTORE-01 (regla 8): TE-005 (cerrado), TE-004, TE-006, TE-012 y TE-013 (cierran antes por dependencia) y todo consumidor cerrado con `CTRL-RESTORE-01 — Diferido a TE-017` antes de cerrar TE-017.
```

```terminado
- [ ] La prueba retroactiva de CTRL-RESTORE-01 cubre, o justifica como «No aplica», a cada consumidor diferido cerrado; como mínimo TE-004, TE-005, TE-006, TE-012 y TE-013.
```

## #37 GATE-001

Reemplazos exactos en el cuerpo:

- Las cuatro apariciones de `US-001–US-024, TE-001–TE-011` (Condiciones de entrada, Protocolo, Umbrales medibles y Dependencias exactas) pasan a `US-001–US-024, TE-001–TE-017`.
- Las dos apariciones de `las 24 historias y 11 habilitadores` pasan a `las 24 historias y 17 habilitadores`.
- Las dos apariciones de `evidencia de registros y caché.` en las líneas «Inconcluso» pasan a `evidencia de registros y caché, o una línea diferida sin la prueba retroactiva de su propietario.`

Línea agregada al final de `### Protocolo` y de `### Umbrales medibles`:

```gate
- Confirmar que cada línea `Diferido a` de los issues cerrados tiene registrada la prueba retroactiva de su propietario.
```
