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

- Work only in `odd/tasks/te-005-media-inspection.md`, the approved `backend/internal/media` files, and the MEDIA-02 platform adapter under `backend/internal/platform/mediaexec`.
- Use Go standard library only and follow established repository package and table-test conventions.
- Implement and commit one bounded MEDIA work unit per `stacked-to-main` slice.
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
| Linux task count | 64 | MEDIA-02 cgroup v2 process/thread containment |
| Memory | 512 MiB | MEDIA-02 process containment |
| CPU time | 30 s | MEDIA-02 process containment |

The dimension limits intentionally combine a 3,840 px per-axis cap with an 8,294,400-pixel cap. This permits both horizontal and vertical 4K-equivalent bounds without orientation bias while rejecting oversized square or extreme-axis inputs.

## Supported MVP media matrix

Classification and allowlisting use content-derived container and codec metadata, never file names, extensions, or client-declared MIME types.

- Images: JPEG, PNG, and WebP.
- Animated image: GIF.
- Audio: MP3; WAV with `pcm_u8`, `pcm_s16le`, `pcm_s24le`, `pcm_s32le`, `pcm_f32le`, or `pcm_f64le`; FLAC; Ogg with Vorbis or Opus; M4A with AAC.
- Video: MP4 with H.264 and optional AAC; WebM with VP9 and optional Opus.
- Every other container or codec combination is outside the MVP and fails closed.

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
  - Commit: `296bf1b` (`feat(media): define inspection acceptance contract`).
  - Authored change size: 687 inserted lines across five files: 564 lines for the domain contract and tests plus 123 lines for this governing tracker.
  - Follow-up proof correction: the original tests did not directly demonstrate rejection of an invalid non-first GIF frame, custom-limit boundaries beyond `MaxPixels`, or representative negative stream, dimension, duration, and GIF-frame metadata.
  - Follow-up coverage: table-driven tests now reject invalid width, height, and pixel area on the second GIF frame; exercise custom byte, width, height, pixel, duration, GIF-frame, and stream limits at the exact boundary and one unit beyond; and cover zero plus representative negative metadata.
  - Follow-up progression: this is verification-only work for already-correct production behavior, so no production RED is claimed. The first focused run exposed only test-fixture interference between custom axis and pixel limits; after each ceiling received independent limits, the focused race test passed.
  - Follow-up verification: `cd backend && go test -race -count=1 ./internal/media` returned `ok github.com/enzoruiz/pepicanvas/backend/internal/media 1.012s`; `cd backend && go test -race -count=1 ./...` returned `ok` for `internal/access` and `internal/media`, each in `1.012s`; `cd backend && go vet ./...` and `git diff --check` produced no output.
  - Follow-up rollback boundary: revert the test-strengthening work-unit commit to remove only these independent proof cases and their tracker evidence; production behavior remains unchanged.
  - Follow-up commit: `365e687` (`test(media): strengthen inspection boundary coverage`).
  - Independent verification: the complete `351c3f4..365e687` range passed with no CRITICAL, WARNING, or SUGGESTION findings; focused and full race tests, `go vet`, and `git diff --check` passed, and production files were unchanged by the follow-up.
- [ ] **MEDIA-02 — Controlled ffprobe, full ffmpeg decode adapter, and process containment**
  - Route: delegated security-sensitive adapter work split into three `stacked-to-main` work units: pure tool normalization, command/capture and complete decode, then Linux containment.
  - Acceptance: shell-free controlled arguments, content-derived allowlist enforcement, full decode, timeout, bounded capture, descendant termination, temporary isolation and cleanup, and enforceable Linux cgroup v2 task, memory, and CPU containment.
  - Platform decision: cgroup v2 delegation is a deployment prerequisite; unsupported or non-delegated environments fail closed rather than silently degrading resource guarantees.
  - Isolation boundary: the domain owns the inspection port and normalized contract; `internal/platform/mediaexec` owns staging, commands, parsing, classification, containment, cleanup, and failure redaction.
  - Checks: focused adapter tests, adversarial timeout/output/process tests, complete backend race suite, and containment evidence.
  - [x] **MEDIA-02A — Pure ffprobe normalization and closed allowlist**
    - Acceptance: bounded JSON parsing normalizes trusted input size, stream count, dimensions, duration, and available GIF frame count; the approved content-derived matrix is closed; malformed, unknown, contradictory, mixed, duplicate-primary, unsupported-stream, and disallowed combinations fail closed through fixed typed adapter errors.
    - GIF boundary: ffprobe metadata alone returns `frame_metadata_required` with an intentionally domain-invalid partial inspection. It never claims final GIF acceptance because MEDIA-02B owns separate all-frame metadata and complete decode.
    - Domain decision: `MaxProcesses: 8` is replaced by `MaxTasks: 64` because cgroup v2 `pids.max` accounts for Linux processes and threads. No inspection port is added before an execution adapter exists; a parser-only interface would add indirection without a domain consumer.
    - Exclusions: no command construction, `os/exec`, subprocess, stderr capture, temporary directory, cgroup, persistence, network, real media fixture, or real `ffprobe`/`ffmpeg` execution.
    - RED: `cd backend && go test -race -count=1 ./internal/platform/mediaexec ./internal/media` failed to compile. The media tests reported undefined `Limits.MaxTasks`; the adapter tests reported undefined production symbols beginning with `container`, `containerJPEG`, and `ErrorCode`.
    - GREEN: after the smallest complete parser and limit implementation, the same command returned `ok` for `internal/platform/mediaexec` in `1.010s` and `internal/media` in `1.011s`.
    - REFACTOR: image demuxer normalization was centralized, incompatible image duration and audio dimensions were rejected explicitly, and internal naming was clarified; the same focused command remained green with `internal/platform/mediaexec` in `1.013s` and `internal/media` in `1.010s`.
    - Final pre-commit verification: the focused race command returned `ok` for `internal/platform/mediaexec` and `internal/media`, each in `1.014s`; the full backend race suite returned `ok` for `internal/access`, `internal/media`, and `internal/platform/mediaexec`, each in `1.012s`; `cd backend && go vet ./...` and `git diff --check` produced no output.
    - Runtime harness: N/A; this is deliberately a pure parser work unit and does not execute external tools.
    - Rollback boundary: revert this work-unit commit to remove the parser, its tests, the `MaxTasks` semantic correction, and this evidence without changing MEDIA-01 validation behavior.
    - Commit evidence: parent `9a1a18d`; Conventional Commit subject `feat(media): normalize ffprobe metadata`. The resulting hash is reported from Git after commit because a commit cannot embed its own identity.
    - Authored change size: the final pre-commit diff contains 675 inserted and 15 deleted lines across five files, or 690 authored changed lines. The approximately 400-line heuristic remains advisory; this cohesive parser, contract correction, exhaustive tests, and evidence unit is reported honestly rather than code-golfed.
    - Independent verification findings: accepted MP4/WebM inspections ignored contradictory present AAC/Opus stream durations; positive durations below half a nanosecond could normalize to zero while returning success; the WAV allowlist accepted any nonempty `pcm_` prefix; and attachment plus arbitrary unknown stream rejection lacked explicit proof.
    - Approved WAV refinement: the exact MVP set is `pcm_u8`, `pcm_s16le`, `pcm_s24le`, `pcm_s32le`, `pcm_f32le`, and `pcm_f64le`. Prefix-based or other PCM codec names remain unsupported unless separately approved.
    - Correction RED: after adding the regressions first, `cd backend && go test -race -count=1 ./internal/platform/mediaexec ./internal/media` failed in three independent areas: contradictory optional AAC and Opus duration cases returned nil errors, the positive sub-nanosecond duration returned nil, and `pcm_not_a_codec` returned nil; `internal/media` passed in `1.010s`.
    - Correction GREEN: after reconciling every present accepted duration source, rejecting rounded-zero durations, and closing the WAV codec set, the same focused command returned `ok` for `internal/platform/mediaexec` and `internal/media`, each in `1.010s`. The approved-matrix success cases continue to pass `media.Validate`, while GIF remains the documented intentionally incomplete exception.
    - Correction coverage: table-driven regressions cover contradictory optional AAC and Opus duration, positive sub-nanosecond duration, all six approved WAV codecs, fake `pcm_not_a_codec`, attachment streams, and arbitrary unknown stream types.
    - Correction boundary: this follow-up changes only pure normalization, its tests, and this tracker. It does not add command execution, real media tools, process containment, or any remote operation.
    - Correction verification: the final focused race command returned `ok` for `internal/platform/mediaexec` and `internal/media`, each in `1.011s`; the full backend race suite returned `ok` for `internal/access` in `1.010s` and for `internal/media` plus `internal/platform/mediaexec` in `1.012s`; `cd backend && go vet ./...` and `git diff --check` produced no output.
    - Correction commit evidence: parent `7555d25`; Conventional Commit subject `fix(media): close probe normalization gaps`. The resulting hash is reported from Git after commit because a commit cannot embed its own identity.
    - Delivery status: after final independent verification, the maintainer accepted `size:exception` for the cohesive MEDIA-02A slice. MEDIA-02B and MEDIA-02C remain separate `stacked-to-main` slices subject to the approximately 400-line review budget.
    - Contract clarification: `NormalizeProbe` establishes structurally coherent metadata and enforces the closed container/codec matrix; successful non-GIF output is not domain acceptance. Every caller must pass that inspection to `media.Validate` with its selected limits, which may intentionally differ from `DefaultLimits`.
    - Verifier warning disposition: the warning that every successful non-GIF normalization must satisfy `media.Validate(DefaultLimits())` was rejected because it conflated pure normalization with configurable domain acceptance. Applying defaults inside the parser would silently override approved caller-specific limits.
    - Clarification proof: focused table-driven cases normalize structurally valid over-default-limit image input, image dimensions, audio duration, and video duration, then prove that `media.Validate(DefaultLimits())` rejects each inspection. `cd backend && go test -race -count=1 ./internal/platform/mediaexec ./internal/media` returned `ok` for both packages in `1.010s`; `cd backend && go test -race -count=1 ./...` returned `ok` for `internal/access` in `1.012s`, `internal/media` in `1.011s`, and `internal/platform/mediaexec` in `1.013s`; `cd backend && go vet ./...` produced no output.
    - Cumulative MEDIA-02A size: `git diff --numstat 9a1a18d..001c808` records 807 insertions and 16 deletions across five files, or 823 authored changed lines.
    - Final independent verification: the complete `9a1a18d..001c808` range passed with no CRITICAL, WARNING, or SUGGESTION findings. Focused and full race tests, `go vet`, `git diff --check`, branch identity, and clean status passed. The verifier found no honest normalization/classification split that would remain independently deliverable with tests paired to behavior.
  - [x] **MEDIA-02B — Controlled commands, bounded capture, and complete decode**
    - Own shell-free argument construction, bounded stdout/stderr capture, temporary staging and cleanup, separate all-frame GIF metadata, complete ffmpeg decoding, timeout behavior, and deterministic helper-process tests without real media fixtures.
    - Exact scope: add the domain `Inspector` port and a `mediaexec.Adapter` configured only with trusted absolute executable/temp paths and approved limits; inject a narrow `Runner`; stage one bounded reader privately; construct fixed metadata, GIF-frame, and complete-decode command tokens; share one deadline and aggregate output budget; normalize, merge GIF frames, validate, decode completely, clean up, and return only fixed typed safe errors.
    - Execution boundary: production contains no `os/exec` runner. Construction without an injected runner fails closed; only tests use the trusted Go helper-process pattern. MEDIA-02C owns the production cgroup-v2 runner.
    - RED: after tests were added first, `cd backend && go test -race -count=1 ./internal/platform/mediaexec ./internal/media` failed to compile because `Command`, `Config`, `Runner`, and the new adapter/capture/frame symbols were undefined; `internal/media` passed in `1.013s`.
    - GREEN: after the smallest complete implementation and correction of test assertions, the same focused command returned `ok` for `internal/platform/mediaexec` in `2.037s` and `internal/media` in `1.015s`.
    - REFACTOR: staging cleanup was centralized around a preserved directory path, and regressions proved cleanup after overflow/reader failure plus rejection after failed complete decode. The focused race command returned `ok` for `internal/platform/mediaexec` in `2.041s` and `internal/media` in `1.010s`.
    - Deterministic coverage: exact token order and no shell/interpolation; fixed private environment and working directory; 0700 directory/0600 O_EXCL file staging; overflow and cleanup; shared stdout/stderr and cross-command capture budget; image/audio/video/GIF orchestration; every GIF frame and count mismatch; validation before decode; one shared deadline; helper-process timeout/output flood; complete-decode failure; and bounded safe errors.
    - Final pre-commit verification: focused race tests passed for `internal/platform/mediaexec` in `2.041s` and `internal/media` in `1.010s`; the full backend race suite passed for `internal/access` in `1.012s`, `internal/media` in `1.011s`, and `internal/platform/mediaexec` in `2.040s`; `cd backend && go vet ./...` and `git diff --check` produced no output.
    - Runtime harness: the test-only runner executes the trusted Go test binary to prove context timeout and aggregate stdout/stderr flooding without invoking real `ffprobe` or `ffmpeg`; a separate synchronized in-memory race test proves genuinely concurrent stdout/stderr budget accounting.
    - Exclusions: no production `os/exec`, cgroup implementation, descendant/resource containment claim, real media fixture or tool execution, dependency installation, network, persistence, quota, upload transport, playback, or remote operation.
    - Commit boundary: parent `c5ea79d`; Conventional Commit subject `feat(media): orchestrate controlled media inspection`. The resulting hash is reported after commit because a commit cannot embed its own identity.
    - Size forecast and actual: forecast 850–1,050 authored changed lines for this cohesive adapter-and-tests slice. The pre-tracker implementation contains 934 inserted lines across eleven files; the final cumulative diff contains 950 insertions and 3 deletions across twelve files, or 953 authored changed lines. The approximately 400-line heuristic remains advisory, and no size exception is claimed in this implementation step.
    - Rollback boundary: revert the MEDIA-02B work-unit commit to remove the domain port, orchestration adapter, command/capture/frame helpers, deterministic tests, and this evidence while preserving MEDIA-02A normalization.
    - Independent verification findings: construction accepted typed-nil Runner values; executable validation accepted missing, non-regular, non-executable, and NUL-bearing paths; trusted paths were not canonicalized; staged-input overflow detection could overflow `int64` at `math.MaxInt64`; concurrent stdout/stderr capture and command isolation lacked direct proof; and the runtime-harness wording overstated concurrency.
    - Correction RED: regressions were added first. `cd backend && go test -race -count=1 ./internal/platform/mediaexec ./internal/media` failed to compile because the new boundary test referenced undefined `boundedReadCapacity`; `internal/media` passed in `1.012s`. That compile failure prevented the same run from independently displaying the other expected failures, so none are claimed as observed RED results.
    - Correction GREEN: construction now rejects nil values across pointer-like Runner representations without invoking them; `EvalSymlinks` canonicalizes and retains absolute executable and temporary-root paths; executable paths must resolve to executable regular files; the temporary root must resolve to an existing directory; and staging compares the read count with remaining capacity before addition, avoiding `limit+1` and accumulated-byte overflow.
    - Correction coverage: controlled temporary stubs and roots cover missing, directory, non-executable, NUL, relative, canonical symlink, and valid paths without executing media tools. Focused arithmetic cases cover `math.MaxInt64` without large I/O; synchronized goroutines concurrently write stdout/stderr under one budget; and a runner mutation regression proves later command arguments and environment remain isolated without requiring a production aliasing change.
    - Correction verification: focused race tests passed for `internal/platform/mediaexec` in `2.038s` and `internal/media` in `1.010s`; the full backend race suite passed for `internal/access` in `1.013s`, `internal/media` in `1.015s`, and `internal/platform/mediaexec` in `2.039s`; the concurrent capture race test passed 50 repetitions in `1.016s`; `cd backend && go vet ./...` and `git diff --check` produced no output.
    - Correction commit boundary: parent `22f695e`; Conventional Commit subject `fix(media): harden controlled inspection boundary`. The resulting hash is reported after commit because a commit cannot embed its own identity.
    - Correction delivery status: `git diff --numstat c5ea79d..1f6b88e` records 1,209 insertions and 3 deletions across twelve files, or 1,212 cumulative authored changed lines. After final independent verification, the maintainer accepted `size:exception` because the hardening commit closes known security gaps in the same acceptance unit and no smaller independently safe split exists.
    - Final independent verification: the complete `c5ea79d..1f6b88e` range passed with no CRITICAL, WARNING, or SUGGESTION findings. Focused and full race tests, 50 repeated concurrent-capture race runs, `go vet`, `git diff --check`, branch identity, and clean status passed.
    - Next step: MEDIA-02C supplies the production cgroup-v2 Runner, delegated containment, descendant termination, and task/memory/CPU enforcement as a separate `stacked-to-main` slice.
  - [ ] **MEDIA-02C — Linux cgroup v2 containment**
    - Own delegated cgroup setup and cleanup, 64-task process/thread enforcement, memory and CPU limits, descendant termination, and fail-closed unsupported-environment behavior.
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
- Slice 2: MEDIA-02A pure ffprobe normalization and closed allowlist enforcement; independently targets `main` after Slice 1 lands.
- Slice 3: MEDIA-02B controlled commands, bounded capture, temporary-resource lifecycle, all-frame GIF metadata, complete decoding, and deterministic helper-process tests; independently targets `main` after Slice 2 lands.
- Slice 4: MEDIA-02C Linux cgroup v2 containment, descendant termination, and resource enforcement; independently targets `main` after Slice 3 lands.
- Slice 5: MEDIA-03 pinned fixtures and real-tool integration evidence; independently targets `main` after its prerequisite slices land.
- Slice 6: MEDIA-04 CI/toolchain pin and final verification; independently targets `main` after the implementation and fixture slices land.
- If a cohesive slice exceeds the review budget after one honest slicing pass, stop, report the overage, and request risk acceptance rather than compressing implementation or tests.

## Progress and evidence

- Worktree: `/home/enzo/projects/pepicanvas-te-005`.
- Active branch: `feat/te-005-mediaexec-runner`; MEDIA-01 began on `feat/te-005-media-inspection`, which remains part of the recorded branch history.
- Initial worktree: clean at `351c3f4`.
- Committed architecture evidence: `docs/stack.md` requires content-derived `ffprobe` inspection, complete `ffmpeg` decoding including every GIF frame, timeout-bound subprocesses, safe bounded failures, and release-pinned tool versions.
- Existing committed evidence contains no stricter numeric media limit matrix, so the maintainer-approved provisional MVP defaults govern this change.
- This tracker is the first file written for TE-005 in this worktree; no source file was changed before its creation.
- The initial MEDIA-01 suite covered default-limit boundaries, custom `MaxPixels`, malformed states, kind/property mismatches, horizontal/vertical dimensions, invalid configuration, and safe rejections; it did not independently prove every claim later added by this follow-up.
- The cohesive domain-and-tests work unit exceeds the advisory 400-line review budget. It cannot be split without separating behavior from its tests, so remote delivery requires an explicit size-risk decision rather than code compression.
- MEDIA-01 completed in `296bf1b` with 687 inserted lines and all required local checks passing.
- The bounded verification follow-up changes tests and this tracker only; no production file or behavior changed.
- The maintainer accepted `size:exception` for the cohesive 778-line MEDIA-01 slice after independent verification; subsequent slices remain subject to the approximately 400-line review budget and `stacked-to-main` strategy.
- The maintainer authorized `backend/internal/platform/mediaexec`, a closed MVP container/codec matrix, a 64-task cgroup limit, and fail-closed cgroup v2 delegation as a Linux deployment prerequisite for MEDIA-02.
- MEDIA-02A remains pure and adds no domain port because command execution has not been introduced; MEDIA-02B will define the useful execution boundary against an actual caller.
- MEDIA-02A correction closes the independent duration, rounded-zero, exact-WAV-allowlist, and unsupported-stream proof findings without changing its pure-parser boundary. The maintainer accepted `size:exception` for the final verified 823-line slice because a smaller split would create non-deliverable cross-commit dependencies.
- MEDIA-02B closes its typed-nil, canonical-path, staging-overflow, concurrent-capture, and command-isolation findings. The maintainer accepted `size:exception` for the final verified 1,212-line security unit because splitting before the hardening commit would retain known defects.

## Next step

Create the MEDIA-02C branch and implement a production cgroup-v2 Runner with delegated task, memory, CPU, descendant-termination, and cleanup enforcement. Do not add real media fixtures in that slice.
