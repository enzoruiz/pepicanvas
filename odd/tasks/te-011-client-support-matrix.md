# TE-011 client support matrix

## Objective

Establish a repeatable, release-blocking client support matrix that records exact browser, OBS browser-source, toolchain, scenario, and durable evidence identities for every PepiCanvas release.

## Problem and why

Labels such as "latest stable" are useful update instructions but are not reproducible release evidence. A release cannot be evaluated later unless its exact client versions, release dates, execution conditions, toolchain, results, correlations, and integrity-protected evidence are fixed in a strict manifest. Missing evidence must block promotion rather than silently becoming approval.

## Issue #3 scope

- Cover exactly the latest two stable Chrome versions and latest two stable Edge versions selected for a release.
- Cover exactly the current and previous stable OBS browser-source versions selected for a release.
- Require exact immutable client versions and release dates without claiming current real-world versions until fresh external evidence is authorized.
- Require a viewport of at least 1280x720, block mobile and tablet clients, and explicitly exclude Firefox and Safari from support.
- Record exact CTRL-VERS-01 toolchain/runtime versions.
- Record release scenarios and durable CTRL-EVID-01 evidence references with result, time, correlation, SHA-256 integrity, access, and retention metadata.
- Classify missing required evidence as `INCONCLUSIVE` and block the release.

## Non-goals

- Discovering or asserting current Chrome, Edge, or OBS versions without fresh external evidence.
- Implementing browser automation, frontend client detection, viewport UI, OBS execution, durable storage, CI, or a deployment release gate in TE011-01.
- Adding Firefox, Safari, mobile, tablet, or smaller viewports to the supported matrix.
- Choosing an evidence vendor, indefinite retention policy, or a universal OBS operating-system/package matrix.

## Authorized implementation scope

- Work only in this tracker, `backend/internal/releasecheck/**`, `backend/cmd/releasecheck/**`, `releases/client-support-policy.json`, the explicitly authorized `frontend/**` foundation, and the root `.gitignore` when generated frontend output requires it.
- Keep the release validator on the Go standard library; use exact npm package versions and a committed lockfile for the frontend foundation.
- Remote access is limited to current documentation through Context7 and public package downloads from `registry.npmjs.org` without credentials. Do not push or create pull requests.
- Preserve all unrelated files and leave remote delivery actions to the parent.

## Constraints

- Parse manifests as strict JSON and reject unknown fields, trailing values, duplicate identities, missing contract entries, and mutable version labels.
- Treat the six required client slots as a closed set: Chrome current/previous, Edge current/previous, and OBS current/previous.
- Keep policy artifacts separate from release evidence so examples cannot be mistaken for observed compatibility results.
- Follow test-first development with table-driven behavior tests and `t.TempDir()` for filesystem boundaries.
- Keep package boundaries idiomatic and avoid speculative storage, browser, or automation abstractions.

## Acceptance criteria

- A standard-library Go package strictly decodes and evaluates one release manifest.
- Exact release/client versions and ISO release dates are mandatory and immutable.
- The required six client slots appear exactly once; missing, extra, duplicate, or mutable entries fail validation.
- The minimum viewport is exactly 1280x720 or stricter, mobile/tablet are blocked, and Firefox/Safari are explicitly excluded.
- Every CTRL-VERS-01 toolchain/runtime field is present with exact versions; module and frontend dependency lists are explicit and duplicate-free.
- Every required client slot has a scenario and durable evidence reference containing SHA-256, correlation, timestamps, result, access policy, and retention policy.
- Missing or incomplete required evidence produces `INCONCLUSIVE`; failing scenario evidence produces `FAIL`; only complete passing evidence produces `PASS`.
- A CLI validates a manifest file, prints its classification, and exits nonzero when release promotion is blocked.
- Focused and full race tests, `go vet`, `git diff --check`, and a local CLI harness pass.

## Stable tasks

- [x] **FND-01 — Minimal executable frontend foundation**
  - Create one React/TypeScript/Vite multipage project with independent `/editor/` and `/overlay/` HTML and mount boundaries, without implementing TE011-02 client detection or viewport behavior.
  - Pin exact npm dependency versions in a committed lockfile and declare the npm toolchain used to generate it.
  - Add deterministic TypeScript, production-build, Vitest DOM, ESLint, Prettier check, and Playwright configuration surfaces. Playwright must target branded `chrome` and `msedge` channels without downloading or claiming browser execution in this work unit.
  - Preserve the architectural boundary that the OBS overlay loads no editor UI or Tailwind dependency.
  - Route/trigger evidence: the maintainer explicitly authorized this separate prerequisite after repository mapping proved that TE011-02 had no executable frontend substrate.
  - RED: after the tooling-bootstrap exception established Vitest, `cd frontend && npm test -- --run` failed both suites because `./EditorShell` and `./OverlayShell` did not exist; Vitest reported `2 failed` files and `no tests` executed.
  - GREEN: after adding only the two semantic shells and independent mounts, `cd frontend && npm test -- --run` reported `2 passed` files and `2 passed` tests in 676 ms.
  - REFACTOR: formatting and tooling configuration were normalized without introducing shared application abstractions; the focused shell run remained green with `2 passed` files and `2 passed` tests in 729 ms.
  - Toolchain and dependencies: generated with Node `v24.16.0` and npm `11.13.0`; `package.json` and `package-lock.json` record exact direct dependency versions. TypeScript was pinned to `6.0.3` because `typescript-eslint@8.71.2` rejects TypeScript `7.0.2` through its `<6.1.0` peer boundary.
  - Final verification: `cd frontend && npm test -- --run` passed 2 tests; `npm run typecheck` and `npm run lint` exited successfully with no diagnostics; `npm run format:check` reported all matched files formatted; `npm run build` transformed 18 modules and emitted independent `dist/editor/index.html` and `dist/overlay/index.html` entries; `npm run playwright:list` loaded the configuration and reported `Total: 0 tests in 0 files`; root `git diff --check` produced no output.
  - Browser scope: Playwright projects configure branded `channel: 'chrome'` and `channel: 'msedge'`; no browser binary was installed or launched, no browser test exists yet, and exact Chrome/Edge execution remains TE011-03.
  - Scope boundary: no client/browser/device detection, viewport blocking, router, state, Tailwind, UI library, product UI, backend integration, or real-browser evidence was added. The overlay imports only React, React DOM, and its own shell.
  - Work-unit identity: `0f31a42` (`feat(frontend): establish multipage foundation`).
  - Native review: medium-risk lineage `review-1cb83499bcadd01f` approved and acknowledged target `sha256:6e6a1f651488a404625c6e5da2fb14bd787e6d46c9b33f1406a9ebd8478c5177`. R3-001 was informational and is resolved by advancing this next step to TE011-02.
  - Rollback boundary: revert the `frontend/**` project, the frontend-only root `.gitignore` entries, and this FND-01 tracker evidence; TE011-01 and all backend behavior remain unchanged.
- [x] **TE011-01 — Release contract**
  - Build the strict release manifest validator, classification model, CLI boundary, contract policy artifact, and behavior tests.
  - Route/trigger evidence: issue #3 / TE-011 explicitly authorizes local implementation; this bounded standard-library work unit uses the injected `go-testing` and `work-unit-commits` workflows.
  - Checks: observe focused RED, GREEN, and REFACTOR; then run the required focused/full race suites, vet, diff check, and local CLI harness.
  - RED: `cd backend && go test -race -count=1 ./internal/releasecheck` failed to compile because `Classification`, `Pass`, `Fail`, and the remaining production API were undefined. The CLI boundary separately failed to compile because `run` was undefined. Later focused regressions first exposed unsupported exact ecosystem version formats and the missing client-release chronology check.
  - GREEN: the first focused package run returned `ok github.com/enzoruiz/pepicanvas/backend/internal/releasecheck 1.053s`; the package and CLI then passed together after the CLI implementation. The exact-version and chronology regressions passed after their minimal corrections.
  - REFACTOR: mutable version-token policy was centralized without changing behavior; `go test -race -count=1 ./internal/releasecheck ./cmd/releasecheck` remained green (`1.079s` and `1.013s` in the observed refactor run).
  - Final verification: the required focused race test passed in `1.091s`; the full race suite passed for `cmd/releasecheck` (`1.016s`), `internal/access` (`1.018s`), `internal/media` (`1.012s`), `internal/platform/mediaexec` (`2.057s`), and `internal/releasecheck` (`1.085s`). `go vet ./...` and `git diff --check` produced no output.
  - Runtime harness: `cd backend && go test -race -count=1 -run '^TestRun$' -v ./cmd/releasecheck` passed in `1.015s`, including the file-backed `PASS`, missing-evidence `INCONCLUSIVE`, and invalid-arguments cases. Fixtures are generated under `t.TempDir()` and contain explicitly synthetic client identities.
  - Policy artifact check: `python3 -m json.tool releases/client-support-policy.json >/dev/null` produced no output and exited successfully.
  - Scope boundary: no real-world client version was asserted, no network or external tool was used, and no browser, OBS, frontend, storage, CI, or release-pipeline behavior was added.
  - R3-001 correction RED: the focused scenario-version regression failed because `clientVersion` was unknown and the report did not contain `does not match client`.
  - R3-001 correction GREEN: each scenario now binds `clientVersion` to the selected client's declared version; changing only the client version makes the manifest `FAIL`. The focused regression passed in `1.013s`.
  - R3-001 verification: the focused race suites passed in `1.083s` and `1.014s`; the full race suite passed across all five packages; `go vet ./...` and `git diff --check` produced no output.
  - Work-unit commits: `8a22533` (`feat(release): enforce client support evidence`) and bounded correction `898279d` (`fix(release): bind evidence to client version`).
  - Native review: medium-risk lineage `review-214c343eafa07974` was approved and acknowledged for target `sha256:23316a77c301763d4534c04563675e3643776b35f3806334c4febe0c0ae42752`; review authority was burned after the exact acknowledgement.
  - R3-002/R3-003 follow-up RED: before production edits, `cd backend && go test -race -count=1 -run '^TestEvaluateEvidenceFollowUps$' ./internal/releasecheck` failed all three focused cases. The first strict-contract attempt exposed `releasedAt` as unknown; a subsequent full-suite run showed that adding a second release-time field would also invalidate the existing CLI contract fixtures, so the correction was narrowed to the already-required exact client `releaseDate`.
  - R3-002/R3-003 follow-up GREEN: evidence `startedAt` is now compared with the selected exact client's `releaseDate` boundary, while durable locations require a non-file absolute hierarchical URI with both a hostname and object path. Scheme-only and host-only values produce `INCONCLUSIVE`. The focused regression passed in `1.015s`.
  - R3-002/R3-003 follow-up REFACTOR: durable URI validation was extracted into `isDurableEvidenceURI`, and client release-date lookup was hoisted outside each scenario's evidence loop. The focused regression remained green in `1.015s`.
  - R3-002/R3-003 verification: the focused regression passed in `1.015s`; the required package race test passed in `1.088s`; the full race suite passed for `cmd/releasecheck` (`1.015s`), `internal/access` (`1.013s`), `internal/media` (`1.013s`), `internal/platform/mediaexec` (`2.056s`), and `internal/releasecheck` (`1.091s`). `go vet ./...` and `git diff --check` produced no output. No separate runtime harness was needed because both corrections are pure manifest validation paths and the full race suite exercised the existing file-backed CLI boundary.
  - R3-002/R3-003 work-unit identity: `08ea482` (`fix(release): validate evidence provenance`). Native assessment classified `898279d..08ea482` as medium risk but `under_budget`, so no new review transaction was due.
  - R3-002/R3-003 rollback boundary: revert the changes to `backend/internal/releasecheck/manifest.go`, `backend/internal/releasecheck/manifest_test.go`, and this tracker entry; the original TE011-01 contract and TE011-02 through TE011-04 remain otherwise unchanged.
  - Authored change size: 1,186 inserted lines across six new files. This cohesive contract, strict decoder, classification proof, CLI boundary, tracker, and policy artifact exceed the advisory forecast without omitting tests or splitting the release contract unnaturally.
  - Rollback boundary: remove the six TE011-01 files listed in the handoff; existing backend packages and product behavior remain unchanged.
- [x] **TE011-02 — Frontend client and viewport guard**
  - Added a pure fail-closed classifier for desktop Google Chrome and Microsoft Edge, a testable browser/device/viewport snapshot adapter, and a shared React guard at both independent mount boundaries. Firefox, Safari, unrecognized Chromium derivatives, malformed or unknown identities, mobile devices, tablets, and viewports below 1280x720 are blocked.
  - Approved recovery behavior: children remain mounted behind an inert, pointer-blocked, `aria-hidden` content layer while an accessible fixed blocking explanation is shown. A resize callback reclassifies the current snapshot and automatically exposes the same mounted child state when support is restored.
  - RED: before production files existed, `cd frontend && npm test -- --run src/client-support` failed all three suites because `./clientSupport`, `./clientSnapshot`, and `./ClientSupportGuard` could not be resolved; Vitest reported `3 failed` files and `no tests` executed.
  - GREEN: after the minimum classifier, snapshot adapter, guard, and mount wiring were added, the focused run exposed one remaining interaction defect: a synthetic click still changed hidden child state. Adding capture-level click suppression alongside native `inert` and pointer blocking produced `3 passed` files and `17 passed` tests in 731 ms.
  - REFACTOR: type-safe assertions replaced unsafe asymmetric matcher assignments and Prettier normalized the new modules; `cd frontend && npm test -- --run src/client-support` remained green with `3 passed` files and `17 passed` tests in 690 ms.
  - Behavior evidence: table-driven tests prove Chrome and Edge acceptance, the inclusive 1280x720 boundary, undersized viewport rejection, mobile and tablet rejection, iPad desktop-mode rejection, and Firefox, Safari, unknown Chromium derivative, malformed, and unknown identity rejection. React tests prove resize recovery, preserved child state, blocking semantics, accessible explanation, and exact resize-listener cleanup.
  - Final verification: `cd frontend && npm test -- --run` passed `5` files and `19` tests in 722 ms; `npm run typecheck` and `npm run lint` exited successfully with no diagnostics; `npm run format:check` reported all matched files formatted; `npm run build` transformed 21 modules and emitted independent editor and overlay entries in 90 ms; `npm run playwright:list` loaded the configuration and reported `Total: 0 tests in 0 files`; root `git diff --check` produced no output.
  - OBS identification limitation: browser APIs and user-agent signals do not positively identify OBS or its exact version. The overlay can run in a Chromium host accepted by this runtime guard, but exact OBS identity and compatibility remain external release-manifest evidence for TE011-03 and TE011-04.
  - Scope boundary: no dependency, router, state library, Tailwind, UI library, backend integration, real client version, browser download, browser launch, or TE011-03/TE011-04 evidence was added. The overlay imports only the shared client-support guard and its own overlay shell; it does not import editor code.
  - Runtime harness: the Vitest jsdom guard suite exercises resize subscription, rerendering, interaction suppression, state continuity, and cleanup. Real branded-browser execution is intentionally deferred to TE011-03.
  - Work-unit identity: `feat(frontend): enforce client support guard` (single TE011-02 implementation, tests, mount wiring, and tracker commit).
  - Rollback boundary: revert `frontend/src/client-support/**`, the guard wrappers in `frontend/src/editor/main.tsx` and `frontend/src/overlay/main.tsx`, and this TE011-02 tracker evidence. FND-01, TE011-01, and backend behavior remain unchanged.
- [ ] **TE011-03 — Exact Chrome and Edge execution**
  - Select externally verified stable versions, obtain reproducible current/previous browser binaries, and execute the release scenarios against exact Chrome and Edge identities.
  - Route/trigger evidence: future browser automation and externally sourced release metadata; network access is not authorized in this work unit.
- [ ] **TE011-04 — OBS evidence and release gate**
  - Execute the approved OBS OS/package matrix, persist independently readable evidence, and wire the manifest classification into the release gate.
  - Route/trigger evidence: future OBS, durable storage, and release-pipeline implementation; none is authorized in this work unit.

## Unresolved decisions

- Durable evidence store, authorized readers, access implementation, and retention duration.
- Reproducible acquisition and preservation of previous Chrome and Edge stable binaries.
- Supported OBS operating-system and package/distribution matrix.

## Delivery strategy and forecast

- Delivery strategy: `ask-on-risk` resolved to `stacked-to-main` by the maintainer after the first work unit exceeded the advisory review budget.
- Slice boundary: TE011-01 is the first independent PR-ready slice and targets `main`; later slices will also target `main` in dependency order.
- Advisory TE011-01 forecast: approximately 400 authored changed lines. This is a review heuristic, not permission to code-golf, omit proof, or split one contract unnaturally.
- Clean rollback boundary: remove only this tracker, the releasecheck package and CLI, and the client support policy artifact; no existing product behavior depends on TE011-01.

## Checks

- `cd backend && go test -race -count=1 ./internal/releasecheck`
- `cd backend && go test -race -count=1 ./...`
- `cd backend && go vet ./...`
- `git diff --check`
- Local CLI fixture/harness for complete `PASS` and missing-evidence `INCONCLUSIVE` classifications.

## Progress

- [x] Created this feature document before task-specific source or tests.
- [x] Observed TE011-01 RED.
- [x] Implemented and observed TE011-01 GREEN.
- [x] Refactored while green.
- [x] Completed final verification and runtime harness.
- [x] Selected `stacked-to-main` delivery after the review-budget forecast triggered `ask-on-risk`.
- [x] Corrected R3-002 evidence chronology and R3-003 durable URI validation as a bounded follow-up work unit.
- [x] Approved state-preserving automatic viewport recovery and verified that TE011-02 depends on a missing executable frontend foundation.
- [x] Established and verified the independent editor and overlay frontend foundation required by TE011-02.
- [x] Implemented and verified the fail-closed runtime client and viewport guard at both frontend mount boundaries.

## Next step

Resolve the durable evidence store/readers/retention, reproducible previous-browser binaries, and supported OBS OS/package matrix before starting TE011-03 and TE011-04. Only externally verified release metadata may populate a real release manifest.
