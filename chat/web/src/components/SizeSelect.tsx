import { useEffect, useState } from "react";
import { api } from "../api";
import { capacitySummary, capacityWarning } from "../capacity";
import { cpuChoices, memoryChoices } from "../sizes";
import type { Capacity, ResourceLimits, Resources } from "../types";

// useCapacity polls GET capacity while the picker is shown, so the
// person choosing a size sees what the host has right now.
export function useCapacity(enabled = true): Capacity | null {
  const [capacity, setCapacity] = useState<Capacity | null>(null);
  useEffect(() => {
    if (!enabled) return;
    let cancelled = false;
    const read = async () => {
      try {
        const c = await api<Capacity>("capacity");
        if (!cancelled) setCapacity(c);
      } catch {
        /* The line is informational; the limits still bound the choice. */
      }
    };
    void read();
    const timer = setInterval(read, 5000);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [enabled]);
  return capacity;
}

// Two selects for a workspace's size, offered within the runner's limits
// (the ladders in sizes.ts). Under them, what the host has now and what
// the other workspaces hold, and a warning when the choice exceeds what
// is free (capacity.ts).
export function SizeSelect({
  limits,
  value,
  onChange,
  disabled,
}: {
  limits: ResourceLimits;
  value: Resources;
  onChange: (r: Resources) => void;
  disabled?: boolean;
}) {
  const capacity = useCapacity();
  const summary = capacity && capacitySummary(capacity);
  const warning = capacity && capacityWarning(capacity, value);
  return (
    <div className="size-select-block">
      <div className="size-select">
        <label>
          CPUs
          <select
            value={value.cpuMilli}
            disabled={disabled}
            onChange={(e) =>
              onChange({ ...value, cpuMilli: Number(e.target.value) })
            }
          >
            {cpuChoices(limits, value.cpuMilli).map((m) => (
              <option key={m} value={m}>
                {m / 1000}
              </option>
            ))}
          </select>
        </label>
        <label>
          Memory
          <select
            value={value.memoryMB}
            disabled={disabled}
            onChange={(e) =>
              onChange({ ...value, memoryMB: Number(e.target.value) })
            }
          >
            {memoryChoices(limits, value.memoryMB).map((mb) => (
              <option key={mb} value={mb}>
                {mb % 1024 === 0 ? `${mb / 1024} GiB` : `${mb} MiB`}
              </option>
            ))}
          </select>
        </label>
      </div>
      {summary && (
        <p className="size-capacity muted" aria-live="polite">
          {summary}
        </p>
      )}
      {warning && (
        <p className="size-capacity warning" role="status">
          {warning}
        </p>
      )}
    </div>
  );
}
