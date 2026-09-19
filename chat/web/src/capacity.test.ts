import { describe, expect, it } from "vitest";
import { capacitySummary, capacityWarning, memoryLabel } from "./capacity";
import type { Capacity } from "./types";

const limits = {
  default: { cpuMilli: 1000, memoryMB: 2048 },
  max: { cpuMilli: 14000, memoryMB: 36864 },
  cpuStepMilli: 1000,
  restart: true,
};
const mac: Capacity = {
  at: "2026-09-18T12:00:00Z",
  kind: "host",
  cpuMilli: 14000,
  memoryMB: 49152,
  load: [5.14, 5.94, 5.65],
  memoryAvailableMB: 21504,
  reserved: { cpuMilli: 3000, memoryMB: 6144 },
  running: 2,
  spares: 1,
  limits,
};

describe("capacitySummary", () => {
  it("says what the host has, its load, and what workspaces hold", () => {
    expect(capacitySummary(mac)).toBe(
      "Host: 14 CPUs, load 5.1 · 21 GiB of 48 GiB memory free · 2 running workspaces and 1 warm spare hold 3 CPUs · 6 GiB",
    );
  });
  it("prefers a CPU percentage and knows a cluster", () => {
    expect(
      capacitySummary({
        ...mac,
        kind: "cluster",
        cpuPercent: 49.6,
        load: undefined,
        cpuMilli: 8000,
        memoryMB: 32768,
        memoryAvailableMB: 16384,
        running: 0,
        spares: 0,
        reserved: { cpuMilli: 0, memoryMB: 0 },
      }),
    ).toBe(
      "Cluster: 8 CPUs, 50% busy · 16 GiB of 32 GiB memory free · no workspace is running",
    );
  });
  it("shows the whole without a use figure, and the error without a whole", () => {
    expect(
      capacitySummary({
        ...mac,
        load: undefined,
        memoryAvailableMB: undefined,
        running: 1,
        spares: 0,
        reserved: { cpuMilli: 1000, memoryMB: 1536 },
      }),
    ).toBe(
      "Host: 14 CPUs · 48 GiB memory · 1 running workspace holds 1 CPU · 1.5 GiB",
    );
    expect(
      capacitySummary({
        ...mac,
        cpuMilli: 0,
        memoryMB: 0,
        error: "server metrics require a Linux execution host",
        running: 0,
        spares: 0,
        reserved: { cpuMilli: 0, memoryMB: 0 },
      }),
    ).toBe(
      "Host capacity unavailable: server metrics require a Linux execution host · no workspace is running",
    );
  });
});

describe("capacityWarning", () => {
  it("is silent within what is free", () => {
    expect(capacityWarning(mac, { cpuMilli: 4000, memoryMB: 8192 })).toBeNull();
  });
  it("names memory beyond what is free and CPUs beyond the host", () => {
    expect(capacityWarning(mac, { cpuMilli: 16000, memoryMB: 24576 })).toBe(
      "This size asks for more memory than is free right now (21 GiB) and more CPUs than the host has (14 CPUs); the workspace may be slow to start or fail to.",
    );
  });
  it("cannot judge memory it does not know", () => {
    expect(
      capacityWarning(
        { ...mac, memoryAvailableMB: undefined },
        { cpuMilli: 1000, memoryMB: 65536 },
      ),
    ).toBeNull();
  });
});

describe("memoryLabel", () => {
  it("rounds GiB to a decimal and keeps MiB below one", () => {
    expect(memoryLabel(512)).toBe("512 MiB");
    expect(memoryLabel(1536)).toBe("1.5 GiB");
    expect(memoryLabel(49152)).toBe("48 GiB");
    expect(memoryLabel(21504)).toBe("21 GiB");
  });
});
