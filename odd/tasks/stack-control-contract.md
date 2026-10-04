# Stack control contract

## Objective

Make `docs/stack.md` the canonical technical contract for cross-cutting guarantees and ensure every new issue records the controls and evidence that apply to its work.

## Problem and why

The MVP stack covers the core technologies, but recovery, boundary security, pilot evidence, reproducibility, and process operations are not owned consistently. Repeating those rules in every issue would create drift. The repository needs one canonical control catalog, lightweight references from Issue Forms, and an executable structural check.

## Authorized scope

- Extend the existing stack document with the approved cross-cutting controls.
- Add one required control-mapping field to each existing Issue Form without changing existing fields.
- Add a dependency-free structural validator for the contract and forms.
- Prepare coordinated GitHub backlog updates only after explicit remote-write authorization.
- Do not modify unrelated untracked Python, README, skill, or registry files.

## Constraints

- Keep PostgreSQL as the coordination source for the pilot; do not add Redis, Kafka, or another broker.
- Keep `docs/stack.md` as the only semantic definition of each control.
- Preserve every existing Issue Form ID, option, label, validation, and ordering except for the additive control field.
- Technical artifacts remain in professional Spanish because the existing stack and backlog use Spanish.
- Remote GitHub writes require separate authorization for the authenticated session.

## Tasks

- [x] **STACK-01 — Map current stack and backlog gaps**
  - Route: delegated; broad issue and document mapping exceeded the inline evidence budget.
  - Acceptance: every systemic gap has an owner recommendation and evidence reference.
  - Evidence: audits of all 38 issues and `docs/stack.md` completed in-session.
- [x] **STACK-02 — Publish the canonical control catalog**
  - Route: delegated writer; non-trivial architecture documentation change.
  - Acceptance: twelve stable control IDs define obligation, applicability, owner, evidence, failure behavior, and MVP exclusions.
  - Evidence: twelve controls documented in `docs/stack.md`; structural readback and `git diff --check` passed.
- [x] **STACK-03 — Enforce control mapping in Issue Forms**
  - Route: delegated writer; coordinated changes across three non-trivial YAML files and one validator.
  - Acceptance: every form requires control applicability/evidence without duplicating control definitions.
  - Evidence: all three forms require `cross_cutting_controls`; `sh -n`, `dash -n`, nominal validation, executable mode, and three independent mutation checks passed.
- [ ] **STACK-04 — Update the live GitHub backlog**
  - Route: delegated remote worker after explicit destination/operation/session authorization.
  - Acceptance: existing owners are amended, missing technical enablers are created, and every issue references applicable controls with exact readback.
  - Checks: one authorized mutation per target followed by target-host readback; stop on unknown outcomes.
- [ ] **STACK-05 — Reconcile and close**
  - Route: inline verification plus bounded delegated checks when applicable.
  - Acceptance: local and remote state agree, all checks are recorded, and no unrelated files changed.

## Delivery plan

- Strategy: `ask-on-risk`.
- Forecast: approximately 360 authored local lines, excluding generated or remote issue content.
- Slice 1: canonical stack contract.
- Slice 2: Issue Forms plus anti-drift validator.
- Remote backlog mutations remain a separate authorized operation and are not implied by local commits.

## Progress and evidence

- Branch: `feat/stack-control-contract`.
- RDD: disabled globally; ordinary repository policy applies.
- Reviewed boundary: branch point `64f86ef`.
- STACK-01 completed from read-only evidence; no local implementation had started before this document.
- STACK-02 and STACK-03 completed after one verifier-driven correction to reject malformed field types, marker ordering, and non-canonical control headings.
- STACK-02 commit: `1c113be` (`docs(stack): define cross-cutting control contract`).

## Next step

Commit the two verified local work units, record their identities, then prepare the separately authorized backlog update.
