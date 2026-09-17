import { describe, it, expect } from "vitest";
import {
  attachmentError,
  formatSize,
  hasFiles,
  pastedName,
  transferFiles,
  MAX_ATTACHMENTS,
} from "./attachments";

const file = (name: string, size = 10) =>
  ({ name, size, type: "" }) as unknown as File;

describe("composer attachments", () => {
  it("reads files from a paste (items) or a drop (files), never text", () => {
    const shot = file("image.png");
    const paste = {
      types: ["text/plain", "Files"],
      items: [
        { kind: "string", getAsFile: () => null },
        { kind: "file", getAsFile: () => shot },
      ],
      files: [shot],
    };
    expect(transferFiles(paste)).toEqual([shot]);
    const drop = { types: ["Files"], files: [file("a.txt"), file("b.txt")] };
    expect(transferFiles(drop).map((f) => f.name)).toEqual(["a.txt", "b.txt"]);
    expect(transferFiles({ types: ["text/plain"], items: [] })).toEqual([]);
    expect(transferFiles(null)).toEqual([]);
    expect(hasFiles(paste)).toBe(true);
    expect(hasFiles({ types: ["text/plain"] })).toBe(false);
    expect(hasFiles(undefined)).toBe(false);
  });
  it("refuses empty, oversized and surplus files before uploading", () => {
    expect(attachmentError(file("ok.txt"), 0)).toBe("");
    expect(attachmentError(file("empty.txt", 0), 0)).toMatch(/empty/);
    expect(attachmentError(file("big.bin", 8 * 1024 * 1024 + 1), 0)).toMatch(
      /8 MiB/,
    );
    expect(attachmentError(file("big.bin", 8 * 1024 * 1024), 0)).toBe("");
    expect(attachmentError(file("one.txt"), MAX_ATTACHMENTS)).toMatch(
      /at most/,
    );
  });
  it("formats sizes for chips", () => {
    expect(formatSize(11)).toBe("11 B");
    expect(formatSize(2048)).toBe("2 KB");
    expect(formatSize(3.5 * 1024 * 1024)).toBe("3.5 MB");
  });
  it("names a pasted screenshot by time and keeps real names", () => {
    const at = new Date(2026, 8, 17, 10, 20, 30);
    expect(pastedName({ name: "image.png", type: "image/png" }, at)).toBe(
      "pasted-20260917T102030.png",
    );
    expect(pastedName({ name: "image.jpeg", type: "image/jpeg" }, at)).toBe(
      "pasted-20260917T102030.jpg",
    );
    expect(pastedName({ name: "shot.png", type: "image/png" }, at)).toBe(
      "shot.png",
    );
  });
});
