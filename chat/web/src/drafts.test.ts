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
