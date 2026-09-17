// Pure helpers behind the links an agent writes into the transcript
// (RichText). The agent is untrusted, so a link's label is not evidence of
// where it goes: `[https://apple.com](https://evil.com)`, a host spelled with
// lookalike letters (`аpple.com` with a Cyrillic а) or a URL whose userinfo
// reads like a host (`https://apple.com@evil.com/`) all look right and go
// elsewhere. Every agent link therefore shows its real host: as ASCII
// (WHATWG URL parsing turns an internationalised host into its `xn--`
// punycode form, which is what the browser connects to), in the hover
// title, and next to the label whenever the label would mislead.

/* What the transcript shows for an agent's http(s) link. */
export type AgentLink = {
  /* Where the link goes: the URL normalised, credentials removed. */
  href: string;
  /* The ASCII host, with the port when it is not the scheme's default. */
  host: string;
  /* The hover text: the URL as `href`, cut when very long. */
  title: string;
  /* Whether the host must be shown next to the label: the label names a
     different host, the host is punycode, or the URL carried userinfo. */
  hint: boolean;
};

/* Longest title shown on hover; a longer URL is cut with an ellipsis. */
export const TITLE_LIMIT = 200;

/* A label that reads as a URL (has `://`) or, as a whole, as a host
   (dotted, with a letters-only last label, an optional port and path), with
   the host it names as written, not parsed: a lookalike host in the label
   must not compare equal to the ASCII host it maps to. */
const URL_HOST = /[a-z][a-z0-9+.-]*:\/\/(?:[^\s/?#@]*@)?([^\s/?#:]+)/gi;
const BARE_HOST =
  /^(?:[^\s/?#@]*@)?((?:[^\s/?#:.@]+\.)+[a-z]{2,})(?::\d+)?(?:[/?#]\S*)?$/i;

/* Hosts a label names, lower-cased and as written. */
export function labelHosts(label: string): string[] {
  const hosts: string[] = [];
  const text = label.trim();
  for (const match of text.matchAll(URL_HOST)) hosts.push(match[1]);
  if (!hosts.length) {
    const bare = BARE_HOST.exec(text);
    if (bare) hosts.push(bare[1]);
  }
  return hosts.map((h) => h.toLowerCase());
}

/* Whether a host has an internationalised label; the ASCII form is what
   the reader sees, and it is worth pointing out. */
export function isPunycode(hostname: string): boolean {
  return /(^|\.)xn--/i.test(hostname);
}

/* How to show an agent's link, or undefined when it is not an http(s) URL
   the browser can parse (such a link stays as text). */
export function agentLink(
  href: string | undefined,
  label: string,
): AgentLink | undefined {
  if (!href || !/^https?:\/\//i.test(href)) return undefined;
  let url: URL;
  try {
    url = new URL(href);
  } catch {
    return undefined;
  }
  const credentials = url.username !== "" || url.password !== "";
  url.username = "";
  url.password = "";
  const hostname = url.hostname;
  const hint =
    credentials ||
    isPunycode(hostname) ||
    labelHosts(label).some((h) => h !== hostname);
  const full = url.href;
  const title =
    full.length > TITLE_LIMIT ? full.slice(0, TITLE_LIMIT - 1) + "…" : full;
  return { href: full, host: url.host, title, hint };
}
