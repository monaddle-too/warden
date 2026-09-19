/* The tab's icon: Warden's mark, with a badge counting what arrived while
   the tab was hidden (a turn's end, an agent's request), cleared once the
   tab is looked at. Drawn on a canvas, since the page ships no icon file
   and a badge needs one drawn anyway. */

/* The badge's text: the count, "9+" past nine, "" for none. */
export function badgeText(count: number): string {
  if (count <= 0) return "";
  return count > 9 ? "9+" : String(count);
}

let link: HTMLLinkElement | undefined;

/* Draws the icon with `count` on it (0 for the plain mark) and installs
   it as the page's icon. A no-op outside a browser. */
export function setFaviconBadge(count: number) {
  if (typeof document === "undefined") return;
  const canvas = document.createElement("canvas");
  canvas.width = 32;
  canvas.height = 32;
  const ctx = canvas.getContext("2d");
  if (!ctx) return;
  // The mark: a rounded square with a "W".
  ctx.fillStyle = "#2b5b4f";
  ctx.beginPath();
  ctx.roundRect(1, 1, 30, 30, 7);
  ctx.fill();
  ctx.fillStyle = "#fbfaf7";
  ctx.font = "bold 19px system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";
  ctx.fillText("W", 16, 17);
  const text = badgeText(count);
  if (text) {
    ctx.fillStyle = "#d33a2c";
    ctx.beginPath();
    ctx.arc(23, 9, 8.5, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = "#fff";
    ctx.font = "bold 11px system-ui, sans-serif";
    ctx.fillText(text, 23, 10);
  }
  if (!link) {
    link =
      (document.querySelector('link[rel="icon"]') as HTMLLinkElement | null) ||
      undefined;
    if (!link) {
      link = document.createElement("link");
      link.rel = "icon";
      document.head.appendChild(link);
    }
  }
  link.type = "image/png";
  link.href = canvas.toDataURL("image/png");
}
