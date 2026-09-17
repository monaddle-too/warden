import { describe, it, expect } from "vitest";
import { imageLoader, limiter, workspaceImagePath } from "./images";

describe("inline image sources", () => {
  it("accepts only plain relative workspace paths", () => {
    expect(workspaceImagePath("out/plot.png")).toBe("out/plot.png");
    expect(workspaceImagePath("./out/plot.png")).toBe("out/plot.png");
    expect(workspaceImagePath("././a.png")).toBe("a.png");
    expect(workspaceImagePath("my%20plot.png")).toBe("my plot.png");
    for (const src of [
      undefined,
      "",
      "https://example.com/a.png",
      "http://example.com/a.png",
      "HTTPS://example.com/a.png",
      "data:image/png;base64,AAAA",
      "javascript:alert(1)",
      "//example.com/a.png",
      "/etc/passwd",
      "\\\\host\\share\\a.png",
      "C:\\a.png",
      "../secret.png",
      "out/../../secret.png",
      "out//a.png",
      "out/",
      ".",
      "%zz",
      "a".repeat(1025),
    ])
      expect(workspaceImagePath(src), String(src)).toBeUndefined();
  });
});

describe("image loader", () => {
  it("caps the fetches in flight", async () => {
    const run = limiter(2);
    const started: number[] = [];
    const release: (() => void)[] = [];
    const results = [0, 1, 2, 3].map((i) =>
      run(
        () =>
          new Promise<number>((resolve) => {
            started.push(i);
            release.push(() => resolve(i));
          }),
      ),
    );
    await Promise.resolve();
    expect(started).toEqual([0, 1]);
    release[0]();
    await results[0];
    await Promise.resolve();
    expect(started).toEqual([0, 1, 2]);
    release[1]();
    release[2]();
    await Promise.all(results.slice(0, 3));
    await Promise.resolve();
    expect(started).toEqual([0, 1, 2, 3]);
    release[3]();
    expect(await Promise.all(results)).toEqual([0, 1, 2, 3]);
  });
  it("releases a slot when a fetch fails", async () => {
    const run = limiter(1);
    await expect(run(() => Promise.reject(new Error("no")))).rejects.toThrow(
      "no",
    );
    expect(await run(() => Promise.resolve("ok"))).toBe("ok");
  });
  it("shares a fetch per chat, entry and path, and forgets failures", async () => {
    const calls: string[] = [];
    let fail = true;
    const blob = {} as Blob;
    const load = imageLoader(
      (chatID, path) => {
        calls.push(`${chatID} ${path}`);
        return fail
          ? Promise.reject(new Error("missing"))
          : Promise.resolve(blob);
      },
      { limit: 2 },
    );
    await expect(load("c", "e1", "a.png")).rejects.toThrow("missing");
    fail = false;
    expect(await load("c", "e1", "a.png")).toBe(blob);
    expect(await load("c", "e1", "a.png")).toBe(blob);
    expect(calls).toEqual(["c a.png", "c a.png"]);
    // A later entry reads the file again: it may have changed.
    await load("c", "e2", "a.png");
    expect(calls).toHaveLength(3);
    // The cache is bounded; the oldest entry is fetched again.
    await load("c", "e3", "a.png");
    await load("c", "e1", "a.png");
    expect(calls).toHaveLength(5);
  });
});
