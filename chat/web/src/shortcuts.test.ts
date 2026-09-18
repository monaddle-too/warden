import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import {
  AREAS,
  SHORTCUTS,
  arrowStep,
  chordLabel,
  groupedShortcuts,
  isKey,
  keysLabel,
  shortcut,
  typingIn,
} from "./shortcuts";

/* The key handlers and the `?` overlay share one table (shortcuts.ts):
   this walks the components' sources for the ids they match through
   `isKey(event, "id")`, so a handler bound to a key the overlay does not
   list, or a listed key nothing handles, fails here — and a raw key
   comparison outside the table is refused, since that is how the two
   drift. The vendored document editor (documents/) is not the chat's. */
function sources(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) {
      if (name !== "documents") out.push(...sources(path));
    } else if (/\.tsx?$/.test(name) && !/\.test\.tsx?$/.test(name) && name !== "shortcuts.ts") {
      out.push(path);
    }
  }
  return out;
}
const root = new URL(".", import.meta.url).pathname;
const files = sources(root).map((path) => ({ path, text: readFileSync(path, "utf8") }));

const press = (
  key: string,
  mods: Partial<Record<"meta" | "ctrl" | "shift" | "alt", boolean>> = {},
) => ({
  key,
  metaKey: !!mods.meta,
  ctrlKey: !!mods.ctrl,
  shiftKey: !!mods.shift,
  altKey: !!mods.alt,
});

describe("the shortcut table", () => {
  it("has unique ids, an area for each, and nothing outside the areas", () => {
    const ids = SHORTCUTS.map((s) => s.id);
    expect(new Set(ids).size).toBe(ids.length);
    const areas = new Set(AREAS.map((a) => a.id));
    for (const s of SHORTCUTS) {
      expect(areas.has(s.area), s.id).toBe(true);
      expect(s.keys.length, s.id).toBeGreaterThan(0);
      expect(s.what, s.id).not.toBe("");
    }
    const grouped = groupedShortcuts();
    expect(grouped.flatMap((g) => g.rows).length).toBe(SHORTCUTS.length);
    for (const g of grouped) expect(g.rows.length, g.id).toBeGreaterThan(0);
  });
  it("is what the handlers match, and every handled key is listed", () => {
    const used = new Map<string, string[]>();
    for (const { path, text } of files) {
      for (const m of text.matchAll(/isKey\([^,)]+,\s*"([^"]+)"\)/g)) {
        used.set(m[1], [...(used.get(m[1]) || []), path]);
      }
    }
    for (const id of used.keys()) {
      expect(SHORTCUTS.some((s) => s.id === id), `${id} is matched in ${used.get(id)} but not in the table`).toBe(true);
    }
    for (const s of SHORTCUTS) {
      if (s.native) continue;
      expect(used.has(s.id), `${s.id} is in the table but no handler matches it`).toBe(true);
    }
    // The prefixes the composer documents are the ones composer.ts knows.
    const composer = readFileSync(join(root, "composer.ts"), "utf8");
    for (const prefix of ["/", "@", "!", "#"]) {
      expect(SHORTCUTS.some((s) => s.native && s.keys[0].key === prefix), prefix).toBe(true);
      expect(composer.includes(`"${prefix}"`), `${prefix} in composer.ts`).toBe(true);
    }
  });
  it("is the only place a key is compared", () => {
    for (const { path, text } of files) {
      const raw = text.match(/\.key\s*===?\s*"|\b(metaKey|ctrlKey|altKey|shiftKey)\b/g);
      expect(raw, `${path} compares keys itself: ${raw?.join(", ")}`).toBeNull();
    }
  });
  it("names every key the plan lists", () => {
    const labels = SHORTCUTS.map((s) => keysLabel(s, true)).join("\n");
    for (const want of ["⌘K", "⌘F", "Esc Esc", "↑", "⌘R", "⇧Tab", "⌘Enter", "?", "/", "@", "!", "#"]) {
      expect(labels, want).toContain(want);
    }
  });
});

describe("isKey", () => {
  it("matches the platform's modifier either way and the exact shift state", () => {
    expect(isKey(press("k", { meta: true }), "search")).toBe(true);
    expect(isKey(press("K", { ctrl: true }), "search")).toBe(true);
    expect(isKey(press("k", { meta: true, shift: true }), "search")).toBe(false);
    expect(isKey(press("k", { meta: true, alt: true }), "search")).toBe(false);
    expect(isKey(press("k"), "search")).toBe(false);
    expect(isKey(press("f", { ctrl: true }), "find")).toBe(true);
    expect(isKey(press("r", { ctrl: true }), "history-search")).toBe(true);
    expect(isKey(press("r", { meta: true }), "history-search")).toBe(true);
    expect(isKey(press("Enter", { meta: true }), "send")).toBe(true);
    expect(isKey(press("Enter"), "send")).toBe(false);
  });
  it("tells Enter, Shift+Enter and Escape apart", () => {
    expect(isKey(press("Enter"), "find-next")).toBe(true);
    expect(isKey(press("Enter", { shift: true }), "find-next")).toBe(false);
    expect(isKey(press("Enter", { shift: true }), "find-prev")).toBe(true);
    expect(isKey(press("Enter"), "queued-save")).toBe(true);
    expect(isKey(press("Enter", { alt: true }), "queued-save")).toBe(false);
    expect(isKey(press("Escape"), "dialog-close")).toBe(true);
    expect(isKey(press("Escape", { shift: true }), "dialog-close")).toBe(false);
    expect(isKey(press("Tab", { shift: true }), "mode-cycle")).toBe(true);
    expect(isKey(press("Tab"), "mode-cycle")).toBe(false);
    expect(isKey(press("Tab"), "menu-pick")).toBe(true);
  });
  it("takes the ? key with or without Shift, and arrows either way", () => {
    expect(isKey(press("?", { shift: true }), "help")).toBe(true);
    expect(isKey(press("?"), "help")).toBe(true);
    expect(isKey(press("/", { shift: true }), "help")).toBe(false);
    expect(isKey(press("ArrowUp"), "menu-move")).toBe(true);
    expect(isKey(press("ArrowDown"), "menu-move")).toBe(true);
    expect(isKey(press("ArrowDown", { meta: true }), "menu-move")).toBe(false);
    expect(arrowStep(press("ArrowDown"))).toBe(1);
    expect(arrowStep(press("ArrowUp"))).toBe(-1);
    expect(arrowStep(press("Enter"))).toBe(0);
    expect(isKey(press("r", { ctrl: true }), "history-next")).toBe(true);
  });
  it("refuses an id the table lacks", () => {
    expect(() => shortcut("nope")).toThrow("no shortcut nope");
    expect(() => isKey(press("a"), "nope")).toThrow();
  });
});

describe("labels", () => {
  it("write the chords the platform's way", () => {
    expect(chordLabel({ key: "k", mod: true }, true)).toBe("⌘K");
    expect(chordLabel({ key: "k", mod: true }, false)).toBe("Ctrl+K");
    expect(chordLabel({ key: "Enter", shift: true }, true)).toBe("⇧Enter");
    expect(chordLabel({ key: "Enter", shift: true }, false)).toBe("Shift+Enter");
    expect(chordLabel({ key: "ArrowUp" }, false)).toBe("↑");
    expect(chordLabel({ key: "Escape" }, true)).toBe("Esc");
    expect(chordLabel({ key: "?", shift: "any" }, true)).toBe("?");
    expect(keysLabel(shortcut("rewind"), true)).toBe("Esc Esc");
    expect(keysLabel(shortcut("menu-pick"), false)).toBe("Enter or Tab");
    expect(keysLabel(shortcut("history-next"), false)).toBe("↓ or Ctrl+R");
  });
  it("knows where typing goes", () => {
    expect(typingIn(null)).toBe(false);
    expect(typingIn({} as EventTarget)).toBe(false);
    expect(typingIn({ tagName: "DIV" } as unknown as EventTarget)).toBe(false);
    expect(typingIn({ tagName: "TEXTAREA" } as unknown as EventTarget)).toBe(true);
    expect(typingIn({ tagName: "INPUT" } as unknown as EventTarget)).toBe(true);
    expect(typingIn({ tagName: "DIV", isContentEditable: true } as unknown as EventTarget)).toBe(true);
  });
});
