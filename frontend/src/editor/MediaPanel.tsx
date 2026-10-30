import { type FormEvent, useEffect, useRef, useState } from "react";

import {
  getMediaUsage,
  type MediaError,
  type MediaRejection,
  type MediaUsage,
  type Session,
  type UploadedMedia,
  uploadMedia,
} from "../auth/api";

// Safe, fixed messages: the panel shows only the aggregate usage of the own
// canvas, never an asset, a file name, or server text.
const usageUnavailableMessage =
  "Storage usage is unavailable. Reload the page to try again.";
const chooseFileMessage = "Choose a file to upload.";
const uploadFailedMessage = "The file could not be uploaded. Try again.";
const rejectedMessage = "This file was rejected.";
const uploadMessages: Partial<Record<MediaError, string>> = {
  quota_exceeded: "This file does not fit in the remaining storage.",
  upload_in_progress:
    "This upload is still in progress. Try again in a moment.",
  upload_failed: "The file could not be stored or checked. Try again.",
  media_unavailable: "Uploads are not available on this server.",
  forbidden: "You are not allowed to upload files to this canvas.",
  rate_limited: "Too many uploads. Try again later.",
  reference_exists: "That upload was already used. Try again.",
  conflict: "That upload was already used. Try again.",
  bad_request: "This file cannot be uploaded.",
  length_required: "This file cannot be uploaded.",
};
// Each names the rule the file broke (US-011 S6).
const rejectionMessages: Partial<Record<MediaRejection, string>> = {
  unsupported_kind:
    "SVG and this format are not supported. Upload a PNG, JPEG, WebP, or GIF image, or an audio or video file.",
  invalid_metadata:
    "The file's type or dimensions could not be established from its content.",
  input_too_large: "This file is larger than the allowed size.",
  dimensions_exceeded: "This file's dimensions are larger than allowed.",
  duration_exceeded: "This file is longer than allowed.",
  frame_count_exceeded: "This file has more frames than allowed.",
  stream_count_exceeded: "This file has more streams than allowed.",
  type_mismatch: "The file content does not match its type.",
  decode_failed: "The file could not be decoded completely.",
  resources_exceeded: "The file needs more resources to check than allowed.",
};
// The image limits, named only for a file that declares an image type;
// audio and video have other limits.
const imageRejectionMessages: Partial<Record<MediaRejection, string>> = {
  input_too_large: "The file is larger than 10 000 000 bytes.",
  dimensions_exceeded:
    "Each image and GIF frame must be at most 4096×4096 pixels.",
};

// Answers after which the same operation may still be pending or was never
// recorded, so a retry keeps its IDs.
const retriable = new Set<MediaError>([
  "unavailable",
  "upload_in_progress",
  "rate_limited",
]);

const bytes = new Intl.NumberFormat("en-US");

type State =
  | { kind: "loading" }
  | { kind: "ready"; usage: MediaUsage }
  | { kind: "unavailable" };

interface PendingUpload {
  file: File;
  operationId: string;
  referenceId: string;
}

interface MediaPanelProps {
  session: Session;
  tenantId: string;
  canvasId: string;
  newOperationId?: () => string;
  newReferenceId?: () => string;
  // Called once per confirmed upload, so the canvas can show its object.
  onUploaded?: (upload: UploadedMedia) => void;
}

function uploadMessage(
  file: File,
  error: MediaError,
  reason?: MediaRejection,
): string {
  if (error === "media_rejected") {
    if (reason === undefined) return rejectedMessage;
    const image = file.type.toLowerCase().startsWith("image/");
    return (
      (image ? imageRejectionMessages[reason] : undefined) ??
      rejectionMessages[reason] ??
      rejectedMessage
    );
  }
  return uploadMessages[error] ?? uploadFailedMessage;
}

export function MediaPanel({
  session,
  tenantId,
  canvasId,
  newOperationId = () => crypto.randomUUID(),
  newReferenceId = () => crypto.randomUUID(),
  onUploaded,
}: MediaPanelProps) {
  const [state, setState] = useState<State>({ kind: "loading" });
  const [file, setFile] = useState<File | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const form = useRef<HTMLFormElement>(null);
  // An upload keeps its operation and reference IDs until it gets a
  // definitive answer, so a retry after a network failure is replayed
  // instead of reserving the file a second time.
  const pending = useRef<PendingUpload | null>(null);

  useEffect(() => {
    let active = true;
    void getMediaUsage(tenantId, canvasId).then((result) => {
      if (!active) return;
      setState(
        result.ok
          ? { kind: "ready", usage: result.value }
          : { kind: "unavailable" },
      );
    });
    return () => {
      active = false;
    };
  }, [tenantId, canvasId]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setNotice(null);
    if (file === null) {
      setError(chooseFileMessage);
      return;
    }
    setError(null);
    setBusy(true);
    try {
      if (pending.current?.file !== file) {
        pending.current = {
          file,
          operationId: newOperationId(),
          referenceId: newReferenceId(),
        };
      }
      const result = await uploadMedia(session.csrfToken, tenantId, canvasId, {
        operationId: pending.current.operationId,
        referenceId: pending.current.referenceId,
        file,
      });
      if (result.ok || !retriable.has(result.error)) {
        pending.current = null;
      }
      if (result.ok) {
        setState({ kind: "ready", usage: result.value.usage });
        setFile(null);
        form.current?.reset();
        setNotice("File uploaded.");
        onUploaded?.(result.value);
      } else {
        setError(uploadMessage(file, result.error, result.reason));
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <section aria-labelledby="storage-heading">
      <h2 id="storage-heading">Canvas storage</h2>
      {state.kind === "loading" && <p role="status">Loading storage usage…</p>}
      {state.kind === "ready" && (
        <>
          <label>
            Storage used
            <meter
              min={0}
              max={state.usage.quotaBytes}
              value={state.usage.usedBytes}
            />
          </label>
          <p>
            Used {bytes.format(state.usage.usedBytes)} of{" "}
            {bytes.format(state.usage.quotaBytes)} bytes
          </p>
          {state.usage.reservedBytes > 0 && (
            <p>
              Reserved by uploads in progress:{" "}
              {bytes.format(state.usage.reservedBytes)} bytes
            </p>
          )}
          <p>Available: {bytes.format(state.usage.availableBytes)} bytes</p>
          <form
            ref={form}
            aria-labelledby="upload-heading"
            onSubmit={(event) => void submit(event)}
            noValidate
          >
            <h3 id="upload-heading">Upload a file</h3>
            <label>
              File
              <input
                type="file"
                onChange={(event) => setFile(event.target.files?.[0] ?? null)}
              />
            </label>
            <button type="submit" disabled={busy}>
              Upload
            </button>
          </form>
          {notice && <p role="status">{notice}</p>}
          {error && <p role="alert">{error}</p>}
        </>
      )}
      {state.kind === "unavailable" && (
        <p role="alert">{usageUnavailableMessage}</p>
      )}
    </section>
  );
}
