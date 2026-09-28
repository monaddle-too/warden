import { expect, it } from "vitest";
import { recordingRoute, duration, transcriptionLabel } from "./recordings";
it("recognizes recording routes without capturing unrelated paths", () => {
  expect(recordingRoute("/recordings/devices")).toBe("/recordings/devices");
  expect(recordingRoute("/recordings/" + "a".repeat(32))).not.toBe("");
  expect(recordingRoute("/recordings/../../admin")).toBe("");
  expect(recordingRoute("/documents")).toBe("");
});
it("shows audio duration and distinct processing state", () => {
  expect(duration(32000 * 65)).toBe("1:05");
  expect(transcriptionLabel("transcribing")).toBe("Transcription in progress");
  expect(transcriptionLabel("failed")).toContain("retry");
});
