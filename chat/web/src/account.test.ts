import { afterEach, describe, expect, it, vi } from "vitest";
import { accountError, enrollPasskey } from "./account";

afterEach(() => {
  vi.unstubAllGlobals();
});
describe("passkey enrollment", () => {
  function setup(
    create = vi.fn().mockResolvedValue({ toJSON: () => ({ id: "new-key" }) }),
  ) {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          id: "challenge",
          options: { publicKey: { challenge: "encoded" } },
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ status: "saved" }),
      });
    vi.stubGlobal("fetch", fetch);
    vi.stubGlobal("PublicKeyCredential", {
      parseCreationOptionsFromJSON: (options: unknown) => options,
    });
    vi.stubGlobal("navigator", { credentials: { create } });
    return fetch;
  }
  it("binds replacement to the server challenge and submits the created credential", async () => {
    const fetch = setup();
    await enrollPasskey("csrf", "Laptop", "old-key");
    expect(fetch).toHaveBeenNthCalledWith(
      1,
      "/auth/passkeys/register/start",
      expect.objectContaining({
        method: "POST",
        headers: expect.objectContaining({ "X-Warden-CSRF": "csrf" }),
        body: JSON.stringify({ name: "Laptop", replaceID: "old-key" }),
      }),
    );
    expect(fetch).toHaveBeenNthCalledWith(
      2,
      "/auth/passkeys/register/finish",
      expect.objectContaining({
        body: JSON.stringify({
          id: "challenge",
          credential: { id: "new-key" },
        }),
      }),
    );
  });
  it("does not finish or delete the existing key when device enrollment is cancelled", async () => {
    const fetch = setup(
      vi
        .fn()
        .mockRejectedValue(new DOMException("cancelled", "NotAllowedError")),
    );
    await expect(enrollPasskey("csrf", "Laptop", "old-key")).rejects.toThrow(
      "cancelled",
    );
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(
      accountError(new DOMException("cancelled", "NotAllowedError")),
    ).toContain("cancelled or timed out");
  });
  it("surfaces save failure instead of reporting enrollment success", async () => {
    setup();
    vi.mocked(fetch)
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ id: "challenge", options: { publicKey: {} } }),
      } as Response)
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: "unable to save passkey" }),
      } as Response);
    await expect(enrollPasskey("csrf", "Laptop")).rejects.toThrow(
      "unable to save passkey",
    );
  });
  it("keeps email sign-in available when the browser lacks passkey support", async () => {
    vi.stubGlobal("PublicKeyCredential", undefined);
    await expect(enrollPasskey("csrf", "Laptop")).rejects.toThrow(
      "keep signing in by email",
    );
  });
});
