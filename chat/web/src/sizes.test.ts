import { describe, expect, it } from "vitest";
import { cpuChoices, memoryChoices } from "./sizes";

const limits = {
  default: { cpuMilli: 1000, memoryMB: 2048 },
  max: { cpuMilli: 14000, memoryMB: 36864 },
  cpuStepMilli: 1000,
  restart: true,
};

describe("size choices", () => {
  it("offer the ladder up to and including the ceiling", () => {
    expect(cpuChoices(limits, 0)).toEqual([
      1000, 2000, 3000, 4000, 6000, 8000, 12000, 14000,
    ]);
    expect(memoryChoices(limits, 0)).toEqual([
      512, 1024, 1536, 2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576, 32768,
      36864,
    ]);
  });
  it("keep the current size even beyond the ceiling, and quarter steps where taken", () => {
    expect(cpuChoices(limits, 16000)).toContain(16000);
    expect(
      cpuChoices(
        {
          ...limits,
          cpuStepMilli: 250,
          max: { cpuMilli: 2000, memoryMB: 4096 },
        },
        0,
      ),
    ).toEqual([250, 500, 750, 1000, 1500, 2000]);
  });
});
