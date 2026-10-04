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
- [ ] **AUTH-04 — Integrate automated verification**
  - Route: delegated writer; CI and evidence must match the executable Go baseline.
  - Acceptance: CI runs the backend race suite and existing control-contract validation without weakening either check.
  - Checks: workflow structural readback, local command parity, and `git diff --check`.
  - Test-first exception: CI configuration has no meaningful local workflow execution boundary in this worktree, so no synthetic RED was produced; verification uses local command parity and read-only YAML structure checks.
- [ ] **AUTH-05 — Reconcile evidence and close**
  - Route: inline status reconciliation after implementation and verification.
  - Acceptance: every task records commits, exact checks, review outcome, rollback boundary, and remaining limitations.

## Delivery plan

- Strategy: `ask-on-risk`.
- Chain strategy: `stacked-to-main`, selected by the maintainer after AUTH-02 crossed the 400-line review threshold.
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
- Native assessment classified `8e64801..988c686` as medium risk because it introduces `backend/go.mod`; global RDD is disabled, and the parent spot-check `go test -race ./...` passed.
- Running implementation size exceeded 400 lines, so subsequent work units will be delivered as independent PRs to `main` in order.

## Next step

Implement AUTH-04 as the next independent stacked-to-main work unit.
