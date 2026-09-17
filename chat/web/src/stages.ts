import type { Chat, Startup } from "./types";

/* The startup stages the chat and the runner report
   (docs/warden-startup-visibility-plan.md), in the owner's words. */
const labels: Record<string, string> = {
  queued: "Waiting to start",
  binding: "Registering the chat",
  preparing: "Preparing the workspace",
  waiting: "Waiting for the runner",
  creating: "Creating the sandbox",
  resuming: "Resuming the sandbox",
  attesting: "Checking the sandbox's network",
  probing: "Reading the sandbox",
  installing: "Installing the agent runtime",
  cloning: "Fetching the repository",
  launching: "Starting the agent",
  initializing: "Waiting for the agent to answer",
  connecting: "Connecting to the agent",
  sending: "Sending your message",
  firstResponse: "Waiting for the first reply",
};

export const stageLabel = (stage: string) => labels[stage] || "Starting";

/* One line for the status bar: the stage, its detail, and how long the
   stage has taken once that is worth saying. */
export function startupLine(s: Startup, now = Date.now() / 1000): string {
  const elapsed = Math.max(0, Math.floor(now - s.since));
  const parts = [stageLabel(s.stage)];
  if (s.detail) parts.push(s.detail);
  if (elapsed >= 15) parts.push(`${elapsed} s`);
  return parts.join(" · ");
}

/* What a chat's status means for people: the startup stage while it is
   starting, otherwise the status itself. */
export function chatStatusLabel(
  c: Pick<Chat, "status" | "startup"> & Partial<Pick<Chat, "conversation">>,
): string {
  if (c.startup && (c.status === "running" || c.status === "queued"))
    return stageLabel(c.startup.stage);
  switch (c.status) {
    case "running": {
      const entries = c.conversation?.entries ?? [];
      const last = entries[entries.length - 1];
      if (
        last?.role === "activity" &&
        last.isStreaming &&
        last.text === "Thinking…"
      )
        return "Agent is thinking";
      return "Agent is running";
    }
    case "queued":
      return "Waiting to start";
    case "stopping":
      return "Stopping…";
    case "idle":
      return "Agent is idle";
    default:
      return c.status;
  }
}
