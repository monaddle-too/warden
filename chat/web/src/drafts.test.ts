import { describe, it, expect } from "vitest";
import { readAttempt, messageAttempt } from "./drafts";
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
});
