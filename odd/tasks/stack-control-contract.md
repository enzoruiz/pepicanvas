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
- [x] **STACK-04 — Update the live GitHub backlog**
  - Route: delegated remote worker after explicit destination/operation/session authorization.
  - Acceptance: existing owners are amended, missing technical enablers are created, and every issue references applicable controls with exact readback.
  - Evidence: 38 existing issues updated; TE-012–TE-017 created as #40–#45; every write received exact target-host readback. One uncertain TE-016 attempt stopped the workflow and was proven absent before a separately authorized fresh creation.
- [x] **STACK-05 — Reconcile and close**
  - Route: inline verification plus bounded delegated checks when applicable.
  - Evidence: 44 open issues reference controls, have a priority label and belong to `MVP piloto`; local validator and `git diff --check` pass; unrelated untracked files remain untouched.

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
- STACK-03 commit: `b6c124e` (`feat(issues): enforce cross-cutting control mapping`).
- Running authored local lines: 332 across both work units, below the 400-line delivery threshold.
- Review outcome: `disabled/unmanaged`; RDD was globally disabled, so ordinary repository policy applies.
- STACK-04 partial remote progress: all 38 pre-existing issues were updated and read back; TE-012 through TE-015 were created as issues #40–#43.
- TE-016 creation returned no identity during a network failure. Later authoritative reads confirmed that issue #44 does not exist and no TE-016 issue was created. TE-017 was not attempted.
- A renewed explicit instruction authorized fresh publication: TE-016 and TE-017 were created and read back exactly as issues #44 and #45.
- Final read-only inventory: 44 open issues; all 38 pre-existing issues have the canonical controls section, and #40–#45 contain their applicable control IDs in the form-authoritative verification field.
- Native dependencies remain textual because the installed GitHub CLI exposes no stable mutation and readback operation for them.
- #40–#45 were normalized through separately authorized exact mutations: `priority:p0` and milestone `MVP piloto` were confirmed while all unrelated state remained unchanged.
- Final remote inventory: 44 open issues, 44 with control references, 44 with priority labels, and 44 in milestone `MVP piloto`.

## Next step

Local implementation and remote backlog alignment are complete. Delivery of the feature branch remains a separate human decision.
