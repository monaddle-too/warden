/* Resource quantities as the cluster reports them: CPU in millicores,
   memory in bytes. */
export const cpu = (milli: number) =>
  milli <= 0
    ? "—"
    : milli < 1000
      ? `${milli}m`
      : `${(milli / 1000).toFixed(milli % 1000 === 0 ? 0 : 2)} CPU`;

export const memory = (bytes: number) => {
  if (bytes <= 0) return "—";
  if (bytes < 1024 ** 2) return `${Math.round(bytes / 1024)} KiB`;
  if (bytes < 1024 ** 3) return `${Math.round(bytes / 1024 ** 2)} MiB`;
  return `${(bytes / 1024 ** 3).toFixed(1)} GiB`;
};

export const percent = (used: number, total: number) =>
  total > 0 ? Math.max(0, Math.min(100, Math.round((100 * used) / total))) : 0;

/* "3 m", "2 h", "5 d" since a timestamp. */
export const age = (since?: string, now = Date.now()) => {
  if (!since) return "";
  const s = Math.max(0, Math.floor((now - new Date(since).getTime()) / 1000));
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)} m`;
  if (s < 86400) return `${Math.floor(s / 3600)} h`;
  return `${Math.floor(s / 86400)} d`;
};

/* The kubelet stamps each line with an RFC 3339 time; show it as the
   local clock time instead. */
export function shortenTimestamp(line: string): string {
  const space = line.indexOf(" ");
  if (space < 20) return line;
  const stamp = line.slice(0, space);
  if (!/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d/.test(stamp)) return line;
  const t = new Date(stamp);
  if (Number.isNaN(t.getTime())) return line;
  return t.toLocaleTimeString() + " " + line.slice(space + 1);
}
