import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { MediaPanel } from "./MediaPanel";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const tenantId = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const canvasId = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const usagePath = `/api/tenants/${tenantId}/canvases/${canvasId}/media/usage`;
const uploadsPath = `/api/tenants/${tenantId}/canvases/${canvasId}/media/uploads`;

const session = {
  accountId: "0123456789abcdef0123456789abcdef",
  email: "alice@example.com",
  csrfToken: "csrf-token-value",
  operator: false,
};

function usage(used: number, reserved = 0) {
  return {
    quota_bytes: 1_000_000_000,
    used_bytes: used,
    reserved_bytes: reserved,
    available_bytes: 1_000_000_000 - used - reserved,
  };
}

function uploaded(used: number): Reply {
  return json(201, {
    asset_id: "dddddddddddddddddddddddddddddddd",
    kind: "image",
    usage: usage(used),
  });
}

function renderPanel() {
  let operations = 0;
  let references = 0;
  return render(
    <MediaPanel
      session={session}
      tenantId={tenantId}
      canvasId={canvasId}
      newOperationId={() => `operation-${++operations}`}
      newReferenceId={() => `reference-${++references}`}
    />,
  );
}

function meter(): HTMLElement {
  return screen.getByRole("meter", { name: "Storage used" });
}

async function ready(text = "Used 0 of 1,000,000,000 bytes") {
  expect(await screen.findByText(text)).toBeInTheDocument();
}

function choose(name = "cat.png", content = "12345"): File {
  const file = new File([content], name, { type: "image/png" });
  fireEvent.change(screen.getByLabelText("File"), {
    target: { files: [file] },
  });
  return file;
}

function upload() {
  fireEvent.click(screen.getByRole("button", { name: "Upload" }));
}

async function settled() {
  await waitFor(() =>
    expect(screen.getByRole("button", { name: "Upload" })).not.toBeDisabled(),
  );
}

describe("MediaPanel", () => {
  it("shows an empty canvas storage with exact byte counts", async () => {
    replies.push(json(200, usage(0)));
    renderPanel();

    await ready();
    expect(
      screen.getByRole("region", { name: "Canvas storage" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Available: 1,000,000,000 bytes"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Reserved/)).not.toBeInTheDocument();
    expect(meter()).toHaveAttribute("min", "0");
    expect(meter()).toHaveAttribute("max", "1000000000");
    expect(meter()).toHaveAttribute("value", "0");
    expect(calls).toEqual([
      {
        method: "GET",
        path: usagePath,
        headers: {},
        body: undefined,
        raw: undefined,
      },
    ]);
  });

  it("shows a full canvas storage at exactly the quota", async () => {
    replies.push(json(200, usage(1_000_000_000)));
    renderPanel();

    await ready("Used 1,000,000,000 of 1,000,000,000 bytes");
    expect(screen.getByText("Available: 0 bytes")).toBeInTheDocument();
    expect(meter()).toHaveAttribute("value", "1000000000");
  });

  it("shows the bytes reserved by uploads in progress", async () => {
    replies.push(json(200, usage(250_000_000, 1)));
    renderPanel();

    await ready("Used 250,000,000 of 1,000,000,000 bytes");
    expect(
      screen.getByText("Reserved by uploads in progress: 1 bytes"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Available: 749,999,999 bytes"),
    ).toBeInTheDocument();
  });

  it.each([
    json(403, { error: "forbidden", detail: "tenant cccc" }),
    json(500, { error: "internal", detail: "boom" }),
    json(200, { quota_bytes: 1 }),
    "network-error" as const,
  ])(
    "shows a fixed message when the usage cannot be read: %o",
    async (reply) => {
      replies.push(reply);
      renderPanel();

      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Storage usage is unavailable. Reload the page to try again.",
      );
      expect(document.body.textContent).not.toContain("cccc");
      expect(document.body.textContent).not.toContain("boom");
      expect(
        screen.queryByRole("button", { name: "Upload" }),
      ).not.toBeInTheDocument();
    },
  );

  it("asks for a file before any upload request", async () => {
    replies.push(json(200, usage(0)));
    renderPanel();
    await ready();

    upload();

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Choose a file to upload.",
    );
    expect(calls).toHaveLength(1);
  });

  it("uploads the raw file and shows the usage the server confirmed", async () => {
    replies.push(json(200, usage(0)));
    renderPanel();
    await ready();

    const file = choose();
    replies.push(uploaded(1_000_000_000));
    upload();

    expect(await screen.findByRole("status")).toHaveTextContent(
      "File uploaded.",
    );
    expect(
      screen.getByText("Used 1,000,000,000 of 1,000,000,000 bytes"),
    ).toBeInTheDocument();
    expect(screen.getByText("Available: 0 bytes")).toBeInTheDocument();
    expect(meter()).toHaveAttribute("value", "1000000000");
    expect(calls[1]).toEqual({
      method: "POST",
      path: `${uploadsPath}?operation_id=operation-1&reference_id=reference-1`,
      headers: {
        "X-CSRF-Token": "csrf-token-value",
        "Content-Type": "application/octet-stream",
      },
      body: undefined,
      raw: file,
    });
    expect(calls).toHaveLength(2);
    // Server data such as the asset ID is never shown.
    expect(document.body.innerHTML).not.toContain("dddddddd");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it.each([
    [
      "quota_exceeded",
      413,
      undefined,
      "This file does not fit in the remaining storage.",
    ],
    [
      "media_rejected",
      422,
      "unsupported_kind",
      "This file type is not supported. Upload an image, GIF, audio, or video file.",
    ],
    [
      "media_rejected",
      422,
      "invalid_metadata",
      "This file could not be read as an image, GIF, audio, or video file.",
    ],
    [
      "media_rejected",
      422,
      "input_too_large",
      "This file is larger than the allowed size.",
    ],
    [
      "media_rejected",
      422,
      "dimensions_exceeded",
      "This file's dimensions are larger than allowed.",
    ],
    [
      "media_rejected",
      422,
      "duration_exceeded",
      "This file is longer than allowed.",
    ],
    [
      "media_rejected",
      422,
      "frame_count_exceeded",
      "This file has more frames than allowed.",
    ],
    [
      "media_rejected",
      422,
      "stream_count_exceeded",
      "This file has more streams than allowed.",
    ],
    ["media_rejected", 422, "invalid_limits", "This file was rejected."],
    ["media_rejected", 422, "something_new", "This file was rejected."],
    ["media_rejected", 422, undefined, "This file was rejected."],
    [
      "upload_in_progress",
      409,
      undefined,
      "This upload is still in progress. Try again in a moment.",
    ],
    [
      "upload_failed",
      502,
      undefined,
      "The file could not be stored or checked. Try again.",
    ],
    [
      "media_unavailable",
      503,
      undefined,
      "Uploads are not available on this server.",
    ],
    [
      "forbidden",
      403,
      undefined,
      "You are not allowed to upload files to this canvas.",
    ],
    ["rate_limited", 429, undefined, "Too many uploads. Try again later."],
    [
      "reference_exists",
      409,
      undefined,
      "That upload was already used. Try again.",
    ],
    ["conflict", 409, undefined, "That upload was already used. Try again."],
    ["bad_request", 400, undefined, "This file cannot be uploaded."],
    ["length_required", 411, undefined, "This file cannot be uploaded."],
    ["internal", 500, undefined, "The file could not be uploaded. Try again."],
    [
      "unauthorized",
      401,
      undefined,
      "The file could not be uploaded. Try again.",
    ],
  ])(
    "maps %s (%s, reason %s) to a fixed message and keeps the usage",
    async (code, status, reason, message) => {
      replies.push(json(200, usage(500, 7)));
      renderPanel();
      await ready("Used 500 of 1,000,000,000 bytes");

      choose("secret-plan.png");
      replies.push(
        json(status, {
          error: code,
          reason,
          detail: "tenant 2222 of mallory@example.com",
          correlation_id: "corr-3333",
          usage: usage(999),
        }),
      );
      upload();

      expect(await screen.findByRole("alert")).toHaveTextContent(message);
      expect(
        screen.getByText("Used 500 of 1,000,000,000 bytes"),
      ).toBeInTheDocument();
      expect(
        screen.getByText("Reserved by uploads in progress: 7 bytes"),
      ).toBeInTheDocument();
      expect(
        screen.getByText("Available: 999,999,493 bytes"),
      ).toBeInTheDocument();
      expect(meter()).toHaveAttribute("value", "500");
      expect(screen.queryByRole("status")).not.toBeInTheDocument();
      expect(document.body.textContent).not.toContain("mallory@example.com");
      expect(document.body.textContent).not.toContain("2222");
      expect(document.body.textContent).not.toContain("corr-3333");
      expect(document.body.textContent).not.toContain("secret-plan.png");
      expect(calls).toHaveLength(2);
    },
  );

  it("shows a network failure as a fixed message and keeps the usage", async () => {
    replies.push(json(200, usage(500)));
    renderPanel();
    await ready("Used 500 of 1,000,000,000 bytes");

    choose();
    replies.push("network-error");
    upload();

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The file could not be uploaded. Try again.",
    );
    expect(
      screen.getByText("Used 500 of 1,000,000,000 bytes"),
    ).toBeInTheDocument();
  });

  it("retries a file with the same IDs until it gets a definitive answer", async () => {
    replies.push(json(200, usage(0)));
    renderPanel();
    await ready();

    const first = choose("one.png");
    replies.push("network-error");
    upload();
    await screen.findByRole("alert");
    await settled();
    replies.push(json(409, { error: "upload_in_progress" }));
    upload();
    await screen.findByText(
      "This upload is still in progress. Try again in a moment.",
    );
    await settled();
    replies.push(json(500, { error: "internal" }));
    upload();
    await waitFor(() => expect(calls).toHaveLength(4));
    await settled();
    for (const call of calls.slice(1, 4)) {
      expect(call.path).toBe(
        `${uploadsPath}?operation_id=operation-1&reference_id=reference-1`,
      );
      expect(call.raw).toBe(first);
    }

    // A definitive answer ends the intent: the same file starts over.
    replies.push(json(502, { error: "upload_failed" }));
    upload();
    await screen.findByText(
      "The file could not be stored or checked. Try again.",
    );
    await settled();
    expect(calls[4].path).toBe(
      `${uploadsPath}?operation_id=operation-1&reference_id=reference-1`,
    );
    replies.push(uploaded(5));
    upload();
    await screen.findByText("File uploaded.");
    expect(calls[5].path).toBe(
      `${uploadsPath}?operation_id=operation-2&reference_id=reference-2`,
    );
  });

  it("gives a newly chosen file new IDs", async () => {
    replies.push(json(200, usage(0)));
    renderPanel();
    await ready();

    choose("one.png");
    replies.push("network-error");
    upload();
    await screen.findByRole("alert");
    await settled();

    const second = choose("two.png", "1234567");
    replies.push(uploaded(7));
    upload();
    await screen.findByText("File uploaded.");

    expect(calls[1].path).toBe(
      `${uploadsPath}?operation_id=operation-1&reference_id=reference-1`,
    );
    expect(calls[2].path).toBe(
      `${uploadsPath}?operation_id=operation-2&reference_id=reference-2`,
    );
    expect(calls[2].raw).toBe(second);
    expect(
      screen.getByText("Used 7 of 1,000,000,000 bytes"),
    ).toBeInTheDocument();
  });

  it("clears the chosen file after a confirmed upload", async () => {
    replies.push(json(200, usage(0)));
    renderPanel();
    await ready();

    choose();
    replies.push(uploaded(5));
    upload();
    await screen.findByText("File uploaded.");
    await settled();
    upload();

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Choose a file to upload.",
    );
    expect(calls).toHaveLength(2);
  });

  it("encodes the tenant and canvas IDs in the upload URL", async () => {
    replies.push(json(200, usage(0)));
    render(
      <MediaPanel
        session={session}
        tenantId="a/b"
        canvasId="c?d"
        newOperationId={() => "op+1"}
        newReferenceId={() => "ref&1"}
      />,
    );
    await ready();

    choose();
    replies.push(uploaded(5));
    upload();
    await screen.findByText("File uploaded.");

    expect(calls[0].path).toBe("/api/tenants/a%2Fb/canvases/c%3Fd/media/usage");
    expect(calls[1].path).toBe(
      "/api/tenants/a%2Fb/canvases/c%3Fd/media/uploads?operation_id=op%2B1&reference_id=ref%261",
    );
  });
});
