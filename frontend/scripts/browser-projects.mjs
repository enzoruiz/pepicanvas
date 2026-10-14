/* global process */

import path from "node:path";

import {
  browserCachePaths,
  loadBrowserMatrix,
  repositoryRoot,
} from "./browser-matrix.mjs";

const matrixPath = path.join(
  repositoryRoot,
  "releases",
  "client-browser-matrix.json",
);
const frontendRoot = path.join(repositoryRoot, "frontend");
const matrix = await loadBrowserMatrix(matrixPath);
const projects = matrix.browsers.map((browser) => ({
  executablePath: browserCachePaths(frontendRoot, browser).executablePath,
  family: browser.family,
  slot: browser.slot,
}));

process.stdout.write(JSON.stringify(projects));
