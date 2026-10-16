const timeout = (milliseconds, message) =>
  new Promise((_, reject) => {
    const timer = setTimeout(() => reject(new Error(message)), milliseconds);
    timer.unref();
  });

export async function connectObsWebSocket(url, expectedVersion) {
  const socket = new WebSocket(url);
  const queue = [];
  const waiters = [];
  socket.addEventListener("message", ({ data }) => {
    const value = JSON.parse(data);
    const waiter = waiters.shift();
    if (waiter) waiter(value);
    else queue.push(value);
  });
  await Promise.race([
    new Promise((resolve, reject) => {
      socket.addEventListener("open", resolve, { once: true });
      socket.addEventListener(
        "error",
        () => reject(new Error("OBS WebSocket connection failed")),
        { once: true },
      );
    }),
    timeout(30_000, "OBS WebSocket connection timed out"),
  ]);

  const receive = () =>
    queue.length
      ? Promise.resolve(queue.shift())
      : new Promise((resolve) => waiters.push(resolve));
  const hello = await Promise.race([
    receive(),
    timeout(5_000, "OBS WebSocket hello timed out"),
  ]);
  if (hello.op !== 0 || hello.d?.authentication)
    throw new Error(
      "OBS WebSocket must be enabled without authentication in the isolated workspace",
    );
  socket.send(JSON.stringify({ op: 1, d: { rpcVersion: 1 } }));
  const identified = await receive();
  if (identified.op !== 2)
    throw new Error("OBS WebSocket identification failed");

  let requestNumber = 0;
  async function request(requestType, requestData = {}) {
    const requestId = `request-${++requestNumber}`;
    socket.send(
      JSON.stringify({ op: 6, d: { requestType, requestId, requestData } }),
    );
    while (true) {
      const response = await Promise.race([
        receive(),
        timeout(30_000, `${requestType} timed out`),
      ]);
      if (response.op !== 7 || response.d?.requestId !== requestId) continue;
      if (!response.d.requestStatus?.result) {
        throw new Error(
          `${requestType} failed: ${response.d.requestStatus?.comment ?? response.d.requestStatus?.code}`,
        );
      }
      return response.d.responseData ?? {};
    }
  }
  const version = await request("GetVersion");
  if (version.obsVersion !== expectedVersion) {
    throw new Error(
      `OBS reported version ${version.obsVersion}; expected ${expectedVersion}`,
    );
  }
  return { close: () => socket.close(), request, version };
}
