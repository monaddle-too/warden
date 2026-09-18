/* The message for a failed request: Warden's own JSON error when there is
   one, else the body as text. An HTML page is not Warden answering but the
   ingress or a proxy in front of it, during a redeploy for instance; its
   title (or the status line) is the message, never its markup. */
export function failureMessage(
  status: number,
  statusText: string,
  body: string,
): string {
  try {
    const parsed = JSON.parse(body);
    if (parsed && typeof parsed.error === "string" && parsed.error)
      return parsed.error;
  } catch {}
  const statusLine = `${status} ${statusText}`.trim();
  if (/^\s*<(!doctype|html)\b/i.test(body)) {
    const title = /<title>([^<]*)<\/title>/i.exec(body)?.[1]?.trim();
    return title || statusLine;
  }
  return body.trim() || statusLine;
}
