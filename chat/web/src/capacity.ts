import type { Capacity, Resources } from "./types";

// The size picker's capacity line: what the host has now, in the words of
// the panel's other resource rows, and a warning when the chosen size is
// more than is free at this moment.

export function memoryLabel(mb: number): string {
  if (mb >= 1024) {
    const gib = mb / 1024;
    return `${Number.isInteger(gib) ? gib : gib.toFixed(1)} GiB`;
  }
  return `${mb} MiB`;
}

function cpuLabel(milli: number): string {
  const cpus = milli / 1000;
  return cpus === 1 ? "1 CPU" : `${cpus} CPUs`;
}

// capacitySummary is one line: the whole and its use, then what running
// workspaces (and warm spares) already hold. Null when nothing is known.
export function capacitySummary(c: Capacity): string | null {
  const parts: string[] = [];
  if (c.error && !c.cpuMilli) {
    parts.push(
      `${c.kind === "cluster" ? "Cluster" : "Host"} capacity unavailable: ${c.error}`,
    );
  } else if (c.cpuMilli || c.memoryMB) {
    const use =
      c.cpuPercent !== undefined
        ? `${Math.round(c.cpuPercent)}% busy`
        : c.load?.length
          ? `load ${c.load[0].toFixed(1)}`
          : "";
    const memory =
      c.memoryAvailableMB !== undefined
        ? `${memoryLabel(c.memoryAvailableMB)} of ${memoryLabel(c.memoryMB)} memory free`
        : `${memoryLabel(c.memoryMB)} memory`;
    parts.push(
      `${c.kind === "cluster" ? "Cluster" : "Host"}: ${cpuLabel(c.cpuMilli)}${use ? `, ${use}` : ""} · ${memory}`,
    );
  }
  const held = c.running + c.spares;
  if (held > 0) {
    const who = [
      c.running
        ? `${c.running} running workspace${c.running === 1 ? "" : "s"}`
        : "",
      c.spares ? `${c.spares} warm spare${c.spares === 1 ? "" : "s"}` : "",
    ]
      .filter(Boolean)
      .join(" and ");
    parts.push(
      `${who} ${held === 1 ? "holds" : "hold"} ${cpuLabel(c.reserved.cpuMilli)} · ${memoryLabel(c.reserved.memoryMB)}`,
    );
  } else if (parts.length) {
    parts.push("no workspace is running");
  }
  return parts.length ? parts.join(" · ") : null;
}

// capacityWarning says when the chosen size exceeds what is free now: the
// sandbox may still be created (the ceiling is the limits'), but it will
// contend for memory, or on SBX may fail to boot.
export function capacityWarning(c: Capacity, chosen: Resources): string | null {
  const problems: string[] = [];
  if (
    c.memoryAvailableMB !== undefined &&
    chosen.memoryMB > c.memoryAvailableMB
  )
    problems.push(
      `more memory than is free right now (${memoryLabel(c.memoryAvailableMB)})`,
    );
  if (c.cpuMilli && chosen.cpuMilli > c.cpuMilli)
    problems.push(
      `more CPUs than the ${c.kind === "cluster" ? "cluster" : "host"} has (${cpuLabel(c.cpuMilli)})`,
    );
  if (!problems.length) return null;
  return `This size asks for ${problems.join(" and ")}; the workspace may be slow to start or fail to.`;
}
