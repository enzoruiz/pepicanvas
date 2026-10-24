import { afterEach, describe, expect, it } from "vitest";

import {
  acceptOperationKey,
  clearInvitationFragment,
  readInvitationToken,
} from "./invitationLink";

const token = `pci_${"aZ09-_".repeat(7)}Q`;

afterEach(() => {
  window.history.replaceState(null, "", "/");
});

describe("readInvitationToken", () => {
  it("reads a pci_ token with 43 base64url characters", () => {
    expect(token).toHaveLength(47);
    expect(readInvitationToken(`#${token}`)).toBe(token);
  });

  it.each([
    ["an empty fragment", ""],
    ["a lone hash", "#"],
    ["a token without the hash", token],
    ["42 characters", `#${token.slice(0, -1)}`],
    ["44 characters", `#${token}A`],
    ["a padding character", `#${token.slice(0, -1)}=`],
    ["a standard base64 character", `#${token.slice(0, -1)}+`],
    ["another prefix", `#pcx_${token.slice(4)}`],
    ["trailing text", `#${token}&x=1`],
    ["leading text", `#x${token}`],
  ])("ignores %s", (_, hash) => {
    expect(readInvitationToken(hash)).toBeNull();
  });
});

describe("clearInvitationFragment", () => {
  it("drops only the fragment from the address", () => {
    window.history.replaceState(null, "", `/editor/?lang=en#${token}`);
    const before = window.history.length;

    clearInvitationFragment();

    expect(window.location.pathname).toBe("/editor/");
    expect(window.location.search).toBe("?lang=en");
    expect(window.location.hash).toBe("");
    expect(window.location.href).not.toContain("pci_");
    expect(window.history.length).toBe(before);
  });
});

describe("acceptOperationKey", () => {
  it("derives the key from the token's SHA-256, never the token", async () => {
    // sha256sum of the token, computed outside the browser.
    const digest =
      "18bfe98ec10def30dbd602ee13793acb5cc4d3604a389b8e722fdb8aa709a367";

    const key = await acceptOperationKey(token);

    expect(key).toBe(`pepicanvas.invitation-accept.${digest}`);
    expect(key).not.toContain(token.slice(4));
  });

  it("gives different tokens different keys", async () => {
    const other = `pci_${"B".repeat(43)}`;

    expect(await acceptOperationKey(token)).not.toBe(
      await acceptOperationKey(other),
    );
  });
});
