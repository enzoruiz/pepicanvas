# TE-001 authorization matrix

## Objective

Deliver an executable, fail-closed authorization matrix for every supported role, resource, action, and access state, with tenant isolation and repeatable evidence for GATE-001.

## Problem and why

PepiCanvas needs one server-authoritative policy boundary before product features can safely read, mutate, command, configure, or deliver tenant data. Scattered permission checks would drift, leak resource existence, and make revocation races impossible to verify.

## Authorized scope

- Implement the authorization domain under `backend/internal/access` using Go and the standard library.
- Define closed role, resource, action, subscription, invitation, and membership vocabularies.
- Enforce strict tenant isolation and a uniform non-enumerating public denial.
- Reauthorize immediately before mutations and before read or event delivery.
- Add deterministic, table-driven tests with two tenants per resource case.
- Add focused Go CI when the executable backend baseline exists.

## Approved policy baseline

- Tenant boundaries are strict for every tenant-scoped resource.
- Invitations grant no access before membership becomes active.
- Inactive subscriptions block tenant resources.
- OBS access is read-only.
- Moderators cannot administer owners.
- Operators may perform only explicitly enumerated operational actions.
- Unrelated actors are always denied.
- Public denials do not reveal resource existence, content, or metadata.

## Non-goals

- HTTP or WebSocket servers, socket closure, secret handling, persistence, or UI.
- Product workflows owned by later issues.
- Cross-tenant operator access or implicit permissions.

## Tasks

- [x] **AUTH-01 — Map the repository and approve policy semantics**
  - Route: delegated exploration; implementation preparation required broad repository and policy evidence.
  - Acceptance: architecture, tests, policy gaps, safe worktree, and authoritative baseline are resolved before source writes.
  - Evidence: branch `feat/te-001-authorization-matrix` created from `44c3616`; maintainer approved the policy baseline in-session.
- [x] **AUTH-02 — Implement the executable fail-closed matrix**
  - Route: delegated writer; coordinated implementation and tests span multiple non-trivial files.
  - Acceptance: closed vocabularies and explicit policy rows cover roles, resources, actions, subscription, invitation, membership, and two-tenant isolation.
  - Checks: observed RED then GREEN with `go test -race ./internal/access`; `go test -race ./...`; `git diff --check`.
  - Scope: added the Go 1.25 module and the standard-library-only authorization types, positive policy table, and behavior tests under `backend/internal/access`.
  - RED: `cd backend && go test -race ./internal/access` failed to build because production symbols such as `Role`, `Resource`, `Action`, and `Request` were undefined.
  - GREEN: `cd backend && go test -race ./internal/access` returned `ok github.com/enzoruiz/pepicanvas/backend/internal/access 1.010s`; after refactoring and complete inactive-subscription coverage it returned `ok github.com/enzoruiz/pepicanvas/backend/internal/access 1.009s`.
  - Verification: focused race test returned `ok ... (cached)`; full backend race suite returned `ok ... (cached)`; `./scripts/check-control-contract.sh` returned `OK: contrato de controles válido`; `git diff --check` produced no output.
  - Commit: `c97f17d` (`feat(access): enforce fail-closed authorization matrix`).
  - Authored change size: 439 inserted lines across four files. The advisory forecast was exceeded to keep the closed vocabularies readable and retain complete behavior and tenant-isolation tests.
  - Rollback boundary: revert `c97f17d` to remove only `backend/go.mod` and `backend/internal/access`; no persistence, transport, UI, or later authorization guard is coupled to this slice.
  - RDD outcome: `disabled/unmanaged`; global RDD is off, so no receipt or review-authority workflow applied.
  - Remaining risks: AUTH-03 still owns non-enumerating public denials and time-of-use reauthorization; no transport or persistence consumer exists yet.
- [x] **AUTH-03 — Add safe denial and reauthorization guards**
  - Route: general fallback sub-agent; one bounded, framework-neutral Go work unit covered the public denial and three operation guards.
  - Acceptance: one public denial contract reveals no foreign metadata, and authorization is re-evaluated immediately before mutation or delivery.
  - Checks: focused adversarial tests, full backend race suite, and `git diff --check`.
  - Scope: added one comparable `ErrAccessDenied`, a `Check` adapter over the existing positive matrix, and current-fact guards for mutation, read delivery, and event delivery.
  - RED: `cd backend && go test -race ./internal/access` failed to build with undefined `Check`, `ErrAccessDenied`, `GuardMutation`, `GuardReadDelivery`, `GuardEventDelivery`, and `RequestLoader` production symbols.
  - GREEN: after implementation and refactoring, `cd backend && go test -race ./internal/access` returned `ok github.com/enzoruiz/pepicanvas/backend/internal/access 1.009s`; the final focused run returned `ok github.com/enzoruiz/pepicanvas/backend/internal/access (cached)`.
  - Verification: focused race test returned `ok ... (cached)`; full backend race suite returned `ok ... (cached)`; `./scripts/check-control-contract.sh` returned `OK: contrato de controles válido`; `git diff --check` produced no output.
  - Commit: `d8b1f52` (`feat(access): guard operations with current authorization`).
  - Authored change size: 308 inserted lines across four files, within the advisory 400-line work-unit budget.
  - Rollback boundary: revert `d8b1f52` to remove only the denial adapter, framework-neutral guards, and their tests; the AUTH-02 policy matrix remains intact.
  - Chain strategy: `stacked-to-main`; this is the second independent work unit and no remote operation or pull request was performed.
  - RDD outcome: `disabled/unmanaged`; global RDD remains off, so no receipt or review-authority workflow applied.
  - Remaining risks: no transport or persistence consumer exists yet, and consumers needing atomic state-to-operation consistency must execute the loader and callback inside their future transactional boundary.
- [x] **AUTH-04 — Integrate automated verification**
  - Route: delegated writer; CI and evidence must match the executable Go baseline.
  - Acceptance: CI runs the backend race suite and existing control-contract validation without weakening either check.
  - Scope: added a backend-only GitHub Actions workflow for pull requests, pushes to `main`, and manual dispatch; backend or workflow changes run the Go race suite and the repository control-contract validator with read-only contents permission.
  - Test-first exception: CI configuration has no meaningful local workflow execution boundary in this worktree, so no synthetic RED was produced; verification uses local command parity and read-only YAML structure checks.
  - Verification: `cd backend && go test -race ./...` returned `ok github.com/enzoruiz/pepicanvas/backend/internal/access (cached)`; `./scripts/check-control-contract.sh` returned `OK: contrato de controles válido`; a read-only Python 3/PyYAML `BaseLoader` parse with exact assertions for triggers, permissions, job settings, action versions, Go module input, working directory, and commands returned `OK: backend workflow YAML parsed and required structure verified`; `git diff --check` produced no output.
  - Commit: `44c27cb` (`ci(backend): verify executable baseline`).
  - Authored change size: 44 inserted lines across the workflow and its initial tracker evidence, within the advisory 400-line work-unit budget.
  - Rollback boundary: revert `44c27cb` and this evidence-only follow-up to remove only the backend workflow and AUTH-04 completion record; the existing control-contract workflow and AUTH-02/AUTH-03 implementation remain intact.
  - Remaining limitation: GitHub-hosted execution is unobserved because no push or other remote operation was authorized.
  - Chain strategy: `stacked-to-main`; this is the third independent work unit and no remote operation or pull request was performed.
  - RDD outcome: `disabled/unmanaged`; global RDD remains off, so no receipt or review-authority workflow applied.
- [x] **AUTH-05 — Reconcile evidence and close**
  - Route: inline status reconciliation after implementation and verification.
  - Acceptance: every task records commits, exact checks, review outcome, rollback boundary, and remaining limitations.
  - Evidence: the worktree was clean after each implementation slice; parent spot-checks reran the complete Go race suite successfully after AUTH-02, AUTH-03, and AUTH-04.
  - Commit: `82a3c4d` (`docs(odd): close TE-001 local development`).
  - Independent verification: AUTH-04 received PASS with no candidate-caused CRITICAL, WARNING, or SUGGESTION findings; official static actions, read-only permissions, trusted command inputs, Go version resolution, working directories, and unchanged control validation were confirmed.
  - Tooling limitation: `actionlint` and local ShellCheck were unavailable; read-only PyYAML assertions passed, and GitHub-hosted execution remains unobserved until an authorized push.
  - Delivery boundary: local development is complete; preparing stacked branches or pull requests is a separate remote-delivery action.

## Delivery plan

- Strategy: `exception-ok`, explicitly selected by the maintainer for one honest issue-closing PR after repository policy made per-slice `Closes #1` links semantically incorrect.
- Superseded chain strategy: `stacked-to-main`; retained as historical evidence but not used for delivery.
- Forecast: approximately 380 authored lines for the first autonomous matrix slice; later reauthorization and CI work remain separate work units.
- Slice 1: executable matrix and tests (`c97f17d`); 439 implementation lines form one cohesive policy-and-evidence unit and may require an explicit size exception at PR preparation.
- Slice 2: denial and reauthorization guards.
- Slice 3: automated verification and final evidence.

## Progress and evidence

- Worktree: `/home/enzo/projects/pepicanvas-te-001`.
- Branch: `feat/te-001-authorization-matrix`.
- Branch point: `44c3616`.
- Original worktree untracked files remain untouched.
- No source code was written before this tracker and its recovery mirror.
- AUTH-02 completed in `c97f17d` with 439 inserted lines and all required local checks passing.
- AUTH-03 completed in `d8b1f52` with 308 inserted lines and all required local checks passing.
- AUTH-04 completed in `44c27cb` with 44 inserted lines and all required local checks passing; hosted CI remains unobserved.
- Native assessment classified `8e64801..988c686` as medium risk because it introduces `backend/go.mod`; global RDD is disabled, and the parent spot-check `go test -race ./...` passed.
- Native assessment classified AUTH-03 as medium risk and under budget; the parent race-suite spot-check passed.
- Native assessment classified AUTH-04 as high risk because the workflow executes shell processes; required independent verification passed with no findings.
- Running implementation size exceeded 400 lines; the maintainer approved a single-PR `size:exception` so the complete issue behavior, tests, guards, CI, and evidence remain linked to the one approved issue.

## Next step

PR #48 (`https://github.com/enzoruiz/pepicanvas/pull/48`) is open against `main`, closes approved issue #1, and has exactly `type:feature` plus the authorized `size:exception`; next observe its automated checks without merging.
