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

## Authorized local-only scope

- Work only in this tracker, `backend/internal/releasecheck/**`, `backend/cmd/releasecheck/**`, and `releases/client-support-policy.json`.
- Use the Go standard library only.
- Perform local development, verification, and parent-owned work-unit commits only; do not access the network, install dependencies, push, or create pull requests.
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
  - Authored change size: 1,186 inserted lines across six new files. This cohesive contract, strict decoder, classification proof, CLI boundary, tracker, and policy artifact exceed the advisory forecast without omitting tests or splitting the release contract unnaturally.
  - Rollback boundary: remove the six TE011-01 files listed in the handoff; existing backend packages and product behavior remain unchanged.
- [ ] **TE011-02 — Frontend client and viewport guard**
  - Enforce supported desktop families, blocked mobile/tablet clients, minimum viewport behavior, and the approved live-resize recovery UX.
  - Route/trigger evidence: future frontend implementation; no frontend edit surface is authorized in this work unit.
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
- Live resize recovery UX after a supported desktop viewport becomes smaller than 1280x720.

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

## Next step

Commit TE011-01 as the first `stacked-to-main` work unit, then resolve the durable evidence store/readers/retention, reproducible previous-browser binaries, supported OBS OS/package matrix, and live resize recovery UX before starting TE011-02 through TE011-04. Only externally verified release metadata may populate a real release manifest.
