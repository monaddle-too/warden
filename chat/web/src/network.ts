/* A workspace's network access: what its sandbox may reach through the
   inspecting gateway. "" follows the install's setting (warden.json, or the
   admin console's switch), "restricted" is the policy template's
   destination list, "open" is any public HTTP/HTTPS website, credentials
   still injected only for approved requests. The owner chooses it at
   creation and from the workspace panel; agents cannot ask for it. */
export type NetworkMode = "" | "restricted" | "open";

/* The install's setting as sharing/egress reports it. */
export type InstallNetwork = {
  mode: "restricted" | "open";
  source: "config" | "console";
  overrides?: number;
};

export const networkModes: NetworkMode[] = ["", "restricted", "open"];

export const networkTitle = (mode: "restricted" | "open") =>
  mode === "open" ? "Open" : "Restricted";

/* The option label for a choice, naming the install's setting when the
   choice is to follow it (and it is known). */
export function networkLabel(
  mode: NetworkMode,
  install?: InstallNetwork,
): string {
  if (mode === "") {
    return install
      ? `Install setting (${networkTitle(install.mode).toLowerCase()})`
      : "Install setting";
  }
  return networkTitle(mode);
}

/* What a mode means, for the line under a control. */
export function networkText(mode: NetworkMode, install?: InstallNetwork) {
  const effective = mode || install?.mode;
  switch (effective) {
    case "open":
      return "Any public HTTP or HTTPS website. Granted GitHub, Google Docs and Figma requests are brokered as before; ungranted ones go through anonymously, with no credential attached.";
    case "restricted":
      return "The AI providers, package registries and the policy template's destination list. GitHub, Google Docs and Figma only through a grant; the agent can ask for one more host at a time.";
    default:
      return "Whatever the admin console's Network access is set to.";
  }
}

/* The mode in effect for a workspace: its own, else the install's. */
export function effectiveNetwork(
  mode: NetworkMode | undefined,
  install?: InstallNetwork,
): "restricted" | "open" | undefined {
  return mode || install?.mode;
}
