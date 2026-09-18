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
    const thinking = (isStreaming: boolean) => ({
      status: "running",
      conversation: {
        entries: [
          {
            id: "t",
            role: "thinking",
            text: "",
            detail: "",
            createdAt: 0,
            isStreaming,
            delivery: "",
          },
        ],
      },
    });
    expect(chatStatusLabel(thinking(true))).toBe("Agent is thinking");
    expect(chatStatusLabel(thinking(false))).toBe("Agent is running");
    expect(chatStatusLabel({ status: "queued" })).toBe("Waiting to start");
    expect(
      chatStatusLabel({
        status: "idle",
        startup: { stage: "launching", since: 0 },
      }),
    ).toBe("Agent is idle");
  });
  it("counts the messages queued behind the turn, or held after a stop", () => {
    const queued = (status: string, n: number) => ({
      status,
      conversation: {
        entries: Array.from({ length: n }, (_, i) => ({
          id: "q" + i,
          role: "user",
          text: "later",
          detail: "",
          createdAt: 0,
          isStreaming: false,
          delivery: "queued",
        })),
      },
    });
    expect(chatStatusLabel(queued("running", 2))).toBe(
      "Agent is running · 2 queued",
    );
    expect(chatStatusLabel(queued("interrupted", 1))).toBe(
      "interrupted · 1 message held",
    );
    expect(chatStatusLabel(queued("idle", 0))).toBe("Agent is idle");
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
