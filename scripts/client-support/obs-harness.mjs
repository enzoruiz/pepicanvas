import { createHash, randomUUID } from "node:crypto";
import { spawn, spawnSync } from "node:child_process";
import { createReadStream } from "node:fs";
import { mkdir, open, readFile, rm, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import { pipeline } from "node:stream/promises";
import { fileURLToPath } from "node:url";

import {
  cacheRoot,
  extractObs,
  prepareInstaller,
  repositoryRoot,
} from "./obs-cache.mjs";
import { validateObsEvidence } from "./obs-evidence.mjs";
import { loadObsMatrix } from "./obs-matrix.mjs";
import { connectObsWebSocket } from "./obs-websocket.mjs";

const delay = (milliseconds) =>
  new Promise((resolve) => setTimeout(resolve, milliseconds));

async function waitForExit(processHandle) {
  for (
    let attempt = 0;
    processHandle?.exitCode === null && attempt < 40;
    attempt++
  )
    await delay(250);
}

function assertOverlayRender(file) {
  const script = [
    "Add-Type -AssemblyName System.Drawing",
    "$b=[Drawing.Bitmap]::FromFile($args[0])",
    "if($b.Width -ne 1280 -or $b.Height -ne 720){$b.Dispose();exit 2}",
    "$t=0;$v=0",
    "for($y=0;$y -lt 720;$y+=2){for($x=0;$x -lt 1280;$x+=2){$a=$b.GetPixel($x,$y).A;if($a -eq 0){$t++}elseif($a -gt 127){$v++}}}",
    "$b.Dispose()",
    "if($t -lt 184320 -or $v -lt 25){exit 3}",
  ].join(";");
  const probe = spawnSync(
    "powershell.exe",
    ["-NoProfile", "-NonInteractive", "-Command", script, file],
    { windowsHide: true },
  );
  if (probe.status !== 0)
    throw new Error("overlay render is blank or missing its visible marker");
}

async function digest(file) {
  const fileStat = await stat(file);
  const hash = createHash("sha256");
  await pipeline(createReadStream(file), hash);
  return {
    name: path.basename(file),
    sha256: hash.digest("hex"),
    size: fileStat.size,
  };
}

async function waitForHTTP(url, child) {
  for (let attempt = 0; attempt < 60; attempt++) {
    if (child.exitCode !== null)
      throw new Error("overlay server exited before becoming ready");
    try {
      const response = await fetch(url);
      if (response.ok) return;
    } catch {
      // Readiness retries are bounded by the loop.
    }
    await delay(500);
  }
  throw new Error("overlay server readiness timed out");
}

export async function waitForDiagnostic(url, child) {
  for (let attempt = 0; attempt < 20; attempt++) {
    if (child.exitCode !== null)
      throw new Error("overlay server exited before reporting client snapshot");
    try {
      const response = await fetch(url);
      if (response.status === 200) return;
    } catch {
      // Diagnostic retries are bounded by the loop.
    }
    await delay(250);
  }
  throw new Error("OBS client snapshot timed out");
}

async function connectWithRetry(url, version, processHandle) {
  let lastError;
  for (let attempt = 0; attempt < 60; attempt++) {
    if (processHandle.exitCode !== null)
      throw new Error(`OBS exited with code ${processHandle.exitCode}`);
    try {
      return await connectObsWebSocket(url, version);
    } catch (error) {
      lastError = error;
      await delay(1_000);
    }
  }
  throw new Error(`OBS WebSocket readiness timed out: ${lastError?.message}`);
}

async function writePortableConfiguration(root, port) {
  const configRoot = path.join(root, "config", "obs-studio");
  await mkdir(path.join(configRoot, "plugin_config", "obs-websocket"), {
    recursive: true,
  });
  await mkdir(path.join(configRoot, "basic", "profiles", "PepiCanvas"), {
    recursive: true,
  });
  await writeFile(
    path.join(configRoot, "global.ini"),
    "[General]\nFirstRun=true\n[Basic]\nProfile=PepiCanvas\nProfileDir=PepiCanvas\n",
  );
  await writeFile(
    path.join(configRoot, "basic", "profiles", "PepiCanvas", "basic.ini"),
    "[Video]\nBaseCX=1280\nBaseCY=720\nOutputCX=1280\nOutputCY=720\nFPSCommon=30\n",
  );
  await writeFile(
    path.join(configRoot, "plugin_config", "obs-websocket", "config.json"),
    `${JSON.stringify({ alerts_enabled: false, auth_required: false, server_enabled: true, server_port: port }, null, 2)}\n`,
  );
}

export async function executeObsSlot({ slot, offline = false, outputRoot }) {
  if (process.platform !== "win32") {
    throw new Error(
      "Windows OBS execution is pending; this harness never claims execution on Linux",
    );
  }
  const matrix = await loadObsMatrix(
    path.join(repositoryRoot, "releases", "obs-browser-source-matrix.json"),
  );
  const release = matrix.releases.find((candidate) => candidate.slot === slot);
  if (!release) throw new Error(`unknown OBS slot ${slot}`);
  const startedAt = new Date().toISOString();
  const correlationId = `${slot}-${randomUUID()}`;
  const slotOutput = path.resolve(outputRoot, slot);
  await rm(slotOutput, { force: true, recursive: true });
  await mkdir(slotOutput, { recursive: true });
  const prepared = await prepareInstaller(release, { offline });
  const extracted = await extractObs(release, prepared.path);
  const websocketPort = release.releaseTrack === "current" ? 4455 : 4456;
  const overlayPort = release.releaseTrack === "current" ? 4173 : 4174;
  await writePortableConfiguration(extracted.root, websocketPort);

  const serverLog = await open(
    path.join(slotOutput, "overlay-server.log"),
    "w",
  );
  const obsLog = await open(path.join(slotOutput, "obs-process.log"), "w");
  const server = spawn(
    process.execPath,
    [
      path.join(import.meta.dirname, "static-server.mjs"),
      path.join(repositoryRoot, "frontend", "dist"),
      String(overlayPort),
    ],
    {
      cwd: repositoryRoot,
      stdio: ["ignore", serverLog.fd, serverLog.fd],
    },
  );
  let obs;
  let websocket;
  try {
    const overlayUrl = `http://127.0.0.1:${overlayPort}/overlay/`;
    await waitForHTTP(overlayUrl, server);
    obs = spawn(
      extracted.executable,
      [
        "--portable",
        "--multi",
        "--skip-wizard",
        "--disable-shutdown-check",
        "--disable-updater",
        "--collection",
        "PepiCanvas",
        "--profile",
        "PepiCanvas",
      ],
      {
        cwd: path.dirname(extracted.executable),
        stdio: ["ignore", obsLog.fd, obsLog.fd],
        windowsHide: true,
      },
    );
    websocket = await connectWithRetry(
      `ws://127.0.0.1:${websocketPort}`,
      release.version,
      obs,
    );
    const sceneName = "PepiCanvas evidence";
    const sourceName = "PepiCanvas overlay";
    await websocket.request("CreateScene", { sceneName });
    await websocket.request("SetCurrentProgramScene", { sceneName });
    await websocket.request("CreateInput", {
      sceneName,
      inputName: sourceName,
      inputKind: "browser_source",
      inputSettings: {
        height: 720,
        reroute_audio: false,
        url: overlayUrl,
        width: 1280,
      },
      sceneItemEnabled: true,
    });
    await waitForDiagnostic(
      `http://127.0.0.1:${overlayPort}/__pepicanvas/obs-client-snapshot`,
      server,
    );
    await delay(5_000);
    const screenshot = path.join(slotOutput, `${slot}-source-screenshot.png`);
    await websocket.request("SaveSourceScreenshot", {
      sourceName,
      imageFormat: "png",
      imageFilePath: screenshot,
      imageWidth: 1280,
      imageHeight: 720,
      imageCompressionQuality: 100,
    });
    assertOverlayRender(screenshot);
    const evidence = {
      schemaVersion: 1,
      slot,
      obsVersion: websocket.version.obsVersion,
      releaseDate: release.publishedAt.slice(0, 10),
      installer: {
        url: release.installerUrl,
        size: release.installerSize,
        sha256: release.installerSha256,
      },
      scenarioId: `obs-browser-source-${release.releaseTrack}`,
      result: "PASS",
      startedAt,
      completedAt: new Date().toISOString(),
      correlationId,
      runtime: {
        os: "windows",
        architecture: "x64",
        node: process.versions.node,
      },
      outputs: [await digest(screenshot)],
    };
    validateObsEvidence(evidence);
    await writeFile(
      path.join(slotOutput, `${slot}-evidence.json`),
      `${JSON.stringify(evidence, null, 2)}\n`,
      { flag: "wx" },
    );
    return evidence;
  } finally {
    websocket?.close();
    if (obs && obs.exitCode === null) {
      spawnSync("taskkill.exe", ["/PID", String(obs.pid), "/T", "/F"], {
        stdio: "ignore",
      });
    }
    if (server.exitCode === null) server.kill();
    await Promise.all([waitForExit(obs), waitForExit(server)]);
    await Promise.all([serverLog.close(), obsLog.close()]);
    await rm(path.join(cacheRoot, "extracted", slot), {
      force: true,
      maxRetries: 10,
      recursive: true,
      retryDelay: 500,
    });
  }
}

async function main() {
  const [slot, mode, outputRoot, ...rest] = process.argv.slice(2);
  if (
    rest.length ||
    !slot ||
    !["--download", "--offline"].includes(mode) ||
    !outputRoot
  ) {
    throw new Error(
      "usage: node obs-harness.mjs <obs-current|obs-previous> --download|--offline <output-directory>",
    );
  }
  await executeObsSlot({ slot, offline: mode === "--offline", outputRoot });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
