import { describe, it, expect } from "vitest";
import {
  NOT_BROWSING,
  onFirstLine,
  onLastLine,
  promptHistory,
  recallNewer,
  recallOlder,
} from "./history";

const entries = [
  { role: "user", text: "first prompt" },
  { role: "assistant", text: "an answer" },
  { role: "user", text: "second prompt", sender: { principalID: "owner" } },
  { role: "user", text: "someone else's", sender: { principalID: "u2" } },
  { role: "activity", text: "ls" },
  { role: "user", text: "  first prompt  " },
  { role: "user", text: "third\nprompt", sender: { principalID: "owner" } },
  { role: "user", text: "   " },
];

describe("prompt history", () => {
  it("lists this person's prompts newest first, once each", () => {
    expect(promptHistory(entries, "owner")).toEqual([
      "third\nprompt",
      "first prompt",
      "second prompt",
    ]);
    expect(promptHistory(entries, "u2")).toEqual(["someone else's"]);
    expect(promptHistory([], "owner")).toEqual([]);
  });

  it("recalls older prompts from the draft and comes back to it", () => {
    const history = ["newest", "older", "oldest"];
    const up1 = recallOlder(NOT_BROWSING, history, "my draft")!;
    expect(up1).toEqual({
      recall: { index: 0, draft: "my draft" },
      text: "newest",
    });
    const up2 = recallOlder(up1.recall, history, up1.text)!;
    expect(up2.text).toBe("older");
    expect(up2.recall.draft).toBe("my draft");
    const up3 = recallOlder(up2.recall, history, up2.text)!;
    expect(up3.text).toBe("oldest");
    expect(recallOlder(up3.recall, history, up3.text)).toBeUndefined();
    const down1 = recallNewer(up3.recall, history)!;
    expect(down1.text).toBe("older");
    const down2 = recallNewer(down1.recall, history)!;
    expect(down2.text).toBe("newest");
    const down3 = recallNewer(down2.recall, history)!;
    expect(down3).toEqual({ recall: NOT_BROWSING, text: "my draft" });
    expect(recallNewer(NOT_BROWSING, history)).toBeUndefined();
    expect(recallOlder(NOT_BROWSING, [], "x")).toBeUndefined();
  });

  it("knows the draft's edges", () => {
    expect(onFirstLine("one line", 4)).toBe(true);
    expect(onLastLine("one line", 4)).toBe(true);
    expect(onFirstLine("two\nlines", 2)).toBe(true);
    expect(onLastLine("two\nlines", 2)).toBe(false);
    expect(onFirstLine("two\nlines", 6)).toBe(false);
    expect(onLastLine("two\nlines", 6)).toBe(true);
  });
});
