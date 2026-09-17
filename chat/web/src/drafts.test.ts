import { describe, it, expect } from "vitest";
import {
  readAttempt,
  messageAttempt,
  resendAttempt,
  canResend,
} from "./drafts";
describe("uncertain message delivery", () => {
  it("reuses the persisted ID after reload, but gives edited messages a new ID", () => {
    const old = { id: "a".repeat(32), text: "Send this" };
    const recovered = readAttempt(
      { getItem: () => JSON.stringify(old) },
      "draft",
    );
    expect(
      messageAttempt(recovered, "Send this", () => "b".repeat(32)),
    ).toEqual(old);
    expect(messageAttempt(recovered, "Edited", () => "b".repeat(32)).id).toBe(
      "b".repeat(32),
    );
  });
  it("treats a change of attachments as a new message", () => {
    const files = ["c".repeat(32), "d".repeat(32)];
    const sent = messageAttempt(undefined, "Look", () => "a".repeat(32), files);
    expect(sent).toEqual({
      id: "a".repeat(32),
      text: "Look",
      attachments: files,
    });
    expect(messageAttempt(sent, "Look", () => "b".repeat(32), files)).toBe(
      sent,
    );
    expect(
      messageAttempt(sent, "Look", () => "b".repeat(32), [files[0]]).id,
    ).toBe("b".repeat(32));
    expect(messageAttempt(sent, "Look", () => "b".repeat(32)).id).toBe(
      "b".repeat(32),
    );
    const recovered = readAttempt(
      { getItem: () => JSON.stringify(sent) },
      "draft",
    );
    expect(recovered).toEqual(sent);
    expect(
      readAttempt(
        {
          getItem: () => JSON.stringify({ ...sent, attachments: ["../x", 5] }),
        },
        "draft",
      ),
    ).toEqual({ id: sent.id, text: "Look" });
  });
  it("tolerates corrupt or unavailable draft storage", () => {
    expect(readAttempt({ getItem: () => "{bad" }, "draft")).toBeUndefined();
    expect(
      readAttempt(
        {
          getItem: () => {
            throw Error("denied");
          },
        },
        "draft",
      ),
    ).toBeUndefined();
  });
  it("resends a transcript entry as a new message with its uploads", () => {
    const files = [{ id: "c".repeat(32) }, { id: "d".repeat(32) }];
    expect(
      resendAttempt({ text: "  Look again \n", attachments: files }, () =>
        "e".repeat(32),
      ),
    ).toEqual({
      id: "e".repeat(32),
      text: "Look again",
      attachments: files.map((f) => f.id),
    });
    expect(resendAttempt({ text: "Plain" }, () => "f".repeat(32))).toEqual({
      id: "f".repeat(32),
      text: "Plain",
    });
  });
  it("offers retry and edit only when a send would be accepted", () => {
    const idle = { archived: false, status: "idle" };
    const sent = { role: "user", delivery: "sent" };
    expect(canResend(sent, idle, true)).toBe(true);
    expect(canResend({ role: "user", delivery: "failed" }, idle, true)).toBe(
      true,
    );
    expect(canResend(sent, { ...idle, status: "running" }, true)).toBe(true);
    expect(canResend(sent, idle, false)).toBe(false);
    expect(canResend(sent, { ...idle, archived: true }, true)).toBe(false);
    expect(canResend(sent, { ...idle, status: "queued" }, true)).toBe(false);
    expect(canResend(sent, { ...idle, status: "stopping" }, true)).toBe(false);
    expect(canResend({ role: "user", delivery: "queued" }, idle, true)).toBe(
      false,
    );
    expect(canResend({ role: "assistant" }, idle, true)).toBe(false);
  });
});
