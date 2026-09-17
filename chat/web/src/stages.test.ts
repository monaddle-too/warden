import { describe, expect, it } from "vitest";
import { chatStatusLabel, startupLine, stageLabel } from "./stages";

describe("stages", () => {
  it("names every stage and falls back", () => {
    expect(stageLabel("creating")).toBe("Creating the sandbox");
    expect(stageLabel("whatever")).toBe("Starting");
  });
  it("builds the status line with detail and elapsed time", () => {
    expect(startupLine({ stage: "creating", since: 100 }, 105)).toBe(
      "Creating the sandbox",
    );
    expect(
      startupLine(
        { stage: "creating", detail: "waiting for a node", since: 100 },
        130,
      ),
    ).toBe("Creating the sandbox · waiting for a node · 30 s");
  });
  it("prefers the stage over the status while starting", () => {
    expect(
      chatStatusLabel({
        status: "running",
        startup: { stage: "launching", since: 0 },
      }),
    ).toBe("Starting the agent");
    expect(chatStatusLabel({ status: "running" })).toBe("Agent is running");
    expect(chatStatusLabel({ status: "queued" })).toBe("Waiting to start");
    expect(
      chatStatusLabel({
        status: "idle",
        startup: { stage: "launching", since: 0 },
      }),
    ).toBe("Agent is idle");
  });
});

import { shortenTimestamp } from "./units";

describe("log timestamps", () => {
  it("shortens the kubelet's stamp and leaves other lines alone", () => {
    const line = "2026-09-17T18:15:16.823338941Z INFO started";
    const out = shortenTimestamp(line);
    expect(out.endsWith(" INFO started")).toBe(true);
    expect(out.length).toBeLessThan(line.length);
    expect(shortenTimestamp("plain line")).toBe("plain line");
  });
});
