/* Which Warden this is (docs/host-dogfood-plan.md): a non-default
   instance is named in the sidebar header and the browser title, with the
   build it runs; the default instance shows nothing new. */
import type { InstanceInfo } from "./types";

/* The header's mark beside "Warden": "dogfood-a v0.0.0-dev.abc", or "" for
   the default instance (or none reported). */
export function instanceLabel(instance?: InstanceInfo): string {
  if (!instance || !instance.name || instance.name === "default") return "";
  return [instance.name, instance.version].filter(Boolean).join(" ");
}

/* The browser tab's title: "Warden · dogfood-a v0.0.0-dev.abc — Chats"
   for a non-default instance, the page's own title otherwise. */
export function documentTitle(base: string, instance?: InstanceInfo): string {
  const label = instanceLabel(instance);
  if (!label) return base;
  const [head, ...rest] = base.split(" — ");
  return [`${head} · ${label}`, ...rest].join(" — ");
}
