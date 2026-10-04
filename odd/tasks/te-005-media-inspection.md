# TE-005 media inspection

## Objective

Deliver a content-based media inspection boundary that accepts only supported, fully decoded media within explicit resource limits and exposes bounded, non-sensitive rejection information.

## Problem and why

Uploaded media is untrusted input. File names, extensions, declared content types, and partial probes cannot prove that an image, GIF, audio file, or video is safe to process. PepiCanvas needs a domain-level acceptance contract before it can add controlled `ffprobe` and `ffmpeg` execution, real-tool evidence, or persistence. Without that contract, adapters can disagree about required metadata, omit checks, leak private tool output, or apply limits inconsistently.

## Issue #2 scope

- Define the supported media kinds: image, GIF, audio, and video.
- Normalize content-derived inspection metadata, including dimensions for every decoded GIF frame.
- Reject unsupported, malformed, contradictory, incomplete, or over-limit media.
- Inspect content with controlled `ffprobe` and complete `ffmpeg` decoding.
- Bound process lifetime, output, descendants, memory, CPU, and temporary resources.
- Prove behavior against pinned real-tool fixtures and integrate reproducible verification.

## Non-goals

- Trusting file extensions, client MIME types, file names, codecs, or raw tool output.
- Persisting assets, reserving or reconciling quota, or implementing upload transport.
- Transcoding for delivery, playback, thumbnailing, general antivirus analysis, or a distributed sandbox.
- Defining universal media safety truths; the matrix below contains configurable MVP defaults.

## Authorized scope

- Work only in `odd/tasks/te-005-media-inspection.md` and the approved `backend/internal/media` files.
- Use Go standard library only and follow established repository package and table-test conventions.
- Implement and commit MEDIA-01 only in the first `stacked-to-main` slice.
- Perform local verification and local commits only; do not push, open a pull request, or perform other remote operations.

## Secure provisional MVP limit matrix

These values are secure MVP defaults configurable through the domain contract. They are product safeguards for this deployment stage, not universal truths about valid media. Exact operating-system and subprocess resource enforcement belongs to MEDIA-02, not MEDIA-01.

| Limit | MVP default | Applicability |
|---|---:|---|
| Input bytes | 25 MiB | Image and GIF |
| Input bytes | 50 MiB | Audio |
| Input bytes | 250 MiB | Video |
| Decoded width | 3,840 px | Image, every GIF frame, and video |
| Decoded height | 3,840 px | Image, every GIF frame, and video |
| Total decoded pixels | 8,294,400 | Image, every GIF frame, and video |
| Duration | 120 s | GIF |
| Duration | 600 s | Audio and video |
| Decoded frame count | 3,600 | GIF |
| Stream count | 8 | All supported kinds |
| Inspection timeout | 30 s | MEDIA-02 process containment |
| Captured tool output | 1 MiB | MEDIA-02 process containment |
| Process count | 8 | MEDIA-02 process containment |
| Memory | 512 MiB | MEDIA-02 process containment |
| CPU time | 30 s | MEDIA-02 process containment |

The dimension limits intentionally combine a 3,840 px per-axis cap with an 8,294,400-pixel cap. This permits both horizontal and vertical 4K-equivalent bounds without orientation bias while rejecting oversized square or extreme-axis inputs.

## Stable tasks

- [x] **MEDIA-01 — Domain acceptance contract and safe rejection taxonomy**
  - Route: general fallback implementation with the `go-testing`, `work-unit-commits`, and `chained-pr` workflows loaded from the authoritative injected paths.
  - Trigger evidence: GitHub issue #2 / TE-005 authorizes local implementation; the maintainer approved the provisional limit matrix, `ask-on-risk`, and `stacked-to-main`.
  - Acceptance: supported kinds, normalized metadata, per-frame GIF dimensions, validated configurable limits, exact-boundary behavior, malformed-state rejection, and bounded safe public errors are executable and table-tested.
  - Checks: observed RED then GREEN and REFACTOR with `cd backend && go test -race ./internal/media`; final `cd backend && go test -race ./...`; `git diff --check`.
  - Exclusions: no subprocess, extension inspection, persistence, quota reservation, playback, or exact resource enforcement.
  - RED: `cd backend && go test -race ./internal/media` failed to build because the test-only package referenced undefined production symbols including `Kind`, `Inspection`, and `Dimensions`.
  - GREEN: after implementing the contract, `cd backend && go test -race ./internal/media` returned `ok github.com/enzoruiz/pepicanvas/backend/internal/media 1.010s`.
  - REFACTOR: duration-limit selection and boundary-case construction were simplified without changing behavior; the focused race test returned `ok github.com/enzoruiz/pepicanvas/backend/internal/media 1.010s`.
  - Final verification: the focused race test returned `ok github.com/enzoruiz/pepicanvas/backend/internal/media (cached)`; the full backend race suite returned `ok` for `internal/access` in `1.009s` and `internal/media` from cache; `git diff --check` produced no output.
  - Runtime harness: N/A for this pure domain contract; MEDIA-01 has no process, filesystem, network, persistence, quota, or playback boundary.
  - Rollback boundary: revert the MEDIA-01 work-unit commit to remove only this tracker and `backend/internal/media`; the existing access package and backend module remain unchanged.
  - Commit and exact authored size: pending local commit and evidence readback.
- [ ] **MEDIA-02 — Controlled ffprobe, full ffmpeg decode adapter, and process containment**
  - Route: security-sensitive adapter work; reassess risk before implementation under `ask-on-risk`.
  - Acceptance: shell-free controlled arguments, full decode, timeout, bounded capture, descendant termination, temporary isolation and cleanup, and enforceable process, memory, and CPU containment.
  - Checks: focused adapter tests, adversarial timeout/output/process tests, complete backend race suite, and containment evidence.
- [ ] **MEDIA-03 — Pinned real-tool fixtures and integration evidence**
  - Route: integration work with external binaries and curated fixtures; keep generated or binary evidence outside review scope unless explicitly authorized.
  - Acceptance: pinned valid and adversarial fixtures prove content-derived classification, truncation rejection, all-frame GIF validation, complete decode, cleanup, and safe failures against exact tool versions.
  - Checks: skippable real-tool integration tests, fixture integrity verification, complete backend race suite, and durable redacted evidence.
- [ ] **MEDIA-04 — CI/toolchain pin and complete verification**
  - Route: CI and reproducibility work; reassess the final workflow boundary before implementation.
  - Acceptance: exact `ffprobe`/`ffmpeg` versions and fixture hashes are pinned, CI runs the required media suite, and all TE-005 evidence is reconciled without weakening existing checks.
  - Checks: local command parity, workflow structure validation, complete backend race suite, control-contract validation, and hosted evidence after separately authorized delivery.

## Acceptance criteria

- The domain vocabulary is closed to image, GIF, audio, and video.
- Configurable limits reject zero or negative values before metadata validation.
- Exact byte, axis, pixel, duration, GIF frame, and stream boundaries are accepted where applicable; one unit beyond each boundary is rejected.
- Image, GIF, and video dimensions are content-derived and required; every GIF frame is independently validated.
- Duration is required only for GIF, audio, and video; GIF frame count is required only for GIF.
- Missing, contradictory, non-positive, unknown, or kind-incompatible properties fail closed.
- Public rejections use a bounded code and fixed safe message and never contain paths, stderr, raw metadata, codecs, or private content.
- MEDIA-01 remains a pure domain unit with no subprocess, filesystem, persistence, quota, or playback behavior.

## Delivery and chain strategy

- Delivery strategy: `ask-on-risk`.
- Chain strategy: `stacked-to-main`.
- Review budget: approximately 400 authored changed lines per pull-request slice; the budget controls slicing, never code quality or test completeness.
- Forecast: 900–1,300 authored changed lines across TE-005.
- Slice 1: MEDIA-01 domain contract, tests, and this governing tracker; targets `main` when remote delivery is separately authorized.
- Slice 2: MEDIA-02 controlled tool adapter and process containment; independently targets `main` after Slice 1 lands.
- Slice 3: MEDIA-03 pinned fixtures and real-tool integration evidence; independently targets `main` after its prerequisite slices land.
- Slice 4: MEDIA-04 CI/toolchain pin and final verification; independently targets `main` after the implementation and fixture slices land.
- If a cohesive slice exceeds the review budget after one honest slicing pass, stop, report the overage, and request risk acceptance rather than compressing implementation or tests.

## Progress and evidence

- Worktree: `/home/enzo/projects/pepicanvas-te-005`.
- Branch: `feat/te-005-media-inspection`.
- Initial worktree: clean at `351c3f4`.
- Committed architecture evidence: `docs/stack.md` requires content-derived `ffprobe` inspection, complete `ffmpeg` decoding including every GIF frame, timeout-bound subprocesses, safe bounded failures, and release-pinned tool versions.
- Existing committed evidence contains no stricter numeric media limit matrix, so the maintainer-approved provisional MVP defaults govern this change.
- This tracker is the first file written for TE-005 in this worktree; no source file was changed before its creation.
- MEDIA-01 is implemented test-first with exact-boundary, one-beyond, malformed-state, kind/property mismatch, horizontal/vertical dimension, invalid-configuration, and safe-rejection coverage.
- The cohesive domain-and-tests work unit exceeds the advisory 400-line review budget. It cannot be split without separating behavior from its tests, so remote delivery requires an explicit size-risk decision rather than code compression.
- MEDIA-01 commit identity and exact authored line count are pending local commit and tracker-only evidence follow-up.

## Next step

Record the MEDIA-01 commit identity and exact slice size, then stop before MEDIA-02 for its `ask-on-risk` process-containment assessment.
