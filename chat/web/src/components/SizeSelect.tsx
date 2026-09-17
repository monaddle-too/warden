import type { ResourceLimits, Resources } from "../types";

// Two selects for a workspace's size, offered within the runner's limits.
// The choices are the sizes people reach for (fractions only where the
// platform takes them, then 1, 1.5, 2, 3, 4, 6, 8 … CPUs; 512 MiB steps up
// to 2 GiB, then 3, 4, 6, 8 … GiB) plus the current value, so a size set
// elsewhere always shows.
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
  return (
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
  );
}

const cpuLadder = [1500, 2000, 3000, 4000, 6000, 8000, 12000, 16000, 24000, 32000, 48000, 64000];
const memoryLadder = [
  512, 1024, 1536, 2048, 3072, 4096, 6144, 8192, 12288, 16384, 24576, 32768,
  49152, 65536,
];

export function cpuChoices(limits: ResourceLimits, current: number) {
  const step = limits.cpuStepMilli || 1000;
  const out = new Set<number>();
  for (let m = step; m <= 1000; m += step) out.add(m);
  for (const m of cpuLadder) if (m % step === 0) out.add(m);
  out.add(limits.default.cpuMilli);
  if (current) out.add(current);
  return [...out]
    .filter((m) => m <= limits.max.cpuMilli || m === current)
    .sort((a, b) => a - b);
}

export function memoryChoices(limits: ResourceLimits, current: number) {
  const out = new Set<number>(memoryLadder);
  out.add(limits.default.memoryMB);
  if (current) out.add(current);
  return [...out]
    .filter((mb) => mb <= limits.max.memoryMB || mb === current)
    .sort((a, b) => a - b);
}

export function sameSize(a: Resources, b: Resources) {
  return a.cpuMilli === b.cpuMilli && a.memoryMB === b.memoryMB;
}
