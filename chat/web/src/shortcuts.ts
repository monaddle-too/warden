/* Every keyboard shortcut of the web chat, in one table (docs/claude-parity.md,
   R2.20). The key handlers match their keys through `isKey(event, id)`
   against this table, and the `?` overlay (ShortcutsDialog.tsx) lists the
   same table, so what the help says and what the keys do cannot drift;
   `shortcuts.test.ts` walks the components' sources for the ids they use
   and refuses a raw key comparison outside this file. Rows marked
   `native` are listed but not matched: the element or the browser does
   them (a textarea's Enter, a dialog's Esc), or they are text prefixes. */

export type Area = "composer" | "transcript" | "approvals" | "navigation";

export const AREAS: { id: Area; title: string }[] = [
  { id: "composer", title: "Composer" },
  { id: "transcript", title: "Transcript" },
  { id: "approvals", title: "Approvals and permission mode" },
  { id: "navigation", title: "Navigation and dialogs" },
];

/* One key combination: `key` is KeyboardEvent.key ("k", "Enter",
   "ArrowUp", "?"); `mod` is ⌘ on a Mac and Ctrl elsewhere (a handler
   accepts either, so ⌘K and Ctrl+K both open the search); `shift`
   and `alt` must be held when true, released when false or absent, and
   are ignored when "any" (the `?` key itself needs Shift on most
   layouts). */
export type Chord = {
  key: string;
  mod?: boolean;
  shift?: boolean | "any";
  alt?: boolean;
};

export type Shortcut = {
  id: string;
  area: Area;
  /* The alternatives that trigger it (or, with `twice`, the one key
     pressed twice in a row). */
  keys: Chord[];
  twice?: boolean;
  what: string;
  when?: string;
  /* Listed, not matched: done by the element or the browser, or a text
     prefix rather than a key. */
  native?: boolean;
};

export const SHORTCUTS: Shortcut[] = [
  // Composer
  { id: "send", area: "composer", keys: [{ key: "Enter", mod: true }], what: "Send the message (or run the command, or ask the side question)" },
  { id: "newline", area: "composer", keys: [{ key: "Enter" }], what: "New line", native: true },
  { id: "history-older", area: "composer", keys: [{ key: "ArrowUp" }], what: "Recall the previous prompt", when: "the caret is on the draft's first line" },
  { id: "history-newer", area: "composer", keys: [{ key: "ArrowDown" }], what: "The next prompt, then the draft again", when: "the caret is on the draft's last line" },
  { id: "edit-queued", area: "composer", keys: [{ key: "ArrowUp" }], what: "Edit your last queued message on its card", when: "the composer is empty and a message of yours is queued" },
  { id: "rewind", area: "composer", keys: [{ key: "Escape" }], twice: true, what: "Rewind: choose a message of yours to edit and resend", when: "the composer is not editing a message" },
  { id: "edit-cancel", area: "composer", keys: [{ key: "Escape" }], what: "Stop editing the message; the draft comes back", when: "editing a message" },
  { id: "mode-cycle", area: "approvals", keys: [{ key: "Tab", shift: true }], what: "Cycle the permission mode: auto → ask → plan", when: "a Claude chat" },
  { id: "menu-move", area: "composer", keys: [{ key: "ArrowUp" }, { key: "ArrowDown" }], what: "Move in the / or @ menu", when: "the menu is open" },
  { id: "menu-pick", area: "composer", keys: [{ key: "Enter" }, { key: "Tab" }], what: "Take the highlighted item", when: "the menu is open" },
  { id: "menu-close", area: "composer", keys: [{ key: "Escape" }], what: "Put the menu away until the caret leaves the trigger", when: "the menu is open" },
  { id: "prefix-command", area: "composer", keys: [{ key: "/" }], what: "Commands: the chat's and the agent's (/btw asks a side question)", native: true },
  { id: "prefix-file", area: "composer", keys: [{ key: "@" }], what: "Mention a workspace file", native: true },
  { id: "prefix-shell", area: "composer", keys: [{ key: "!" }], what: "Run a shell command in the workspace yourself (not the agent)", native: true },
  { id: "prefix-note", area: "composer", keys: [{ key: "#" }], what: "Append a note to the workspace's CLAUDE.md", native: true },
  // Transcript
  { id: "find", area: "transcript", keys: [{ key: "f", mod: true }], what: "Find in the transcript", when: "the transcript or the composer has focus" },
  { id: "find-next", area: "transcript", keys: [{ key: "Enter" }], what: "Next match", when: "in the find bar" },
  { id: "find-prev", area: "transcript", keys: [{ key: "Enter", shift: true }], what: "Previous match", when: "in the find bar" },
  { id: "find-close", area: "transcript", keys: [{ key: "Escape" }], what: "Close the find bar", when: "in the find bar" },
  { id: "queued-save", area: "transcript", keys: [{ key: "Enter" }], what: "Save the queued message in its place", when: "editing a queued message on its card" },
  { id: "queued-newline", area: "transcript", keys: [{ key: "Enter", shift: true }], what: "New line in the queued message", when: "editing a queued message on its card", native: true },
  { id: "queued-cancel", area: "transcript", keys: [{ key: "Escape" }], what: "Leave the queued message as it was", when: "editing a queued message on its card" },
  // Navigation and dialogs
  { id: "search", area: "navigation", keys: [{ key: "k", mod: true }], what: "Search chats and messages (again closes the palette)" },
  { id: "search-move", area: "navigation", keys: [{ key: "ArrowUp" }, { key: "ArrowDown" }], what: "Move in the results", when: "the palette is open" },
  { id: "search-open", area: "navigation", keys: [{ key: "Enter" }], what: "Open the highlighted chat or message", when: "the palette is open" },
  { id: "help", area: "navigation", keys: [{ key: "?", shift: "any" }], what: "This list", when: "outside an input" },
  { id: "dialog-save", area: "navigation", keys: [{ key: "Enter", mod: true }], what: "Save", when: "editing your instructions or a memory file" },
  { id: "dialog-close", area: "navigation", keys: [{ key: "Escape" }], what: "Close the dialog, palette or overlay" },
];

const byID = new Map(SHORTCUTS.map((s) => [s.id, s]));

/* The shortcut with that id; a wrong id is a programming error (the
   test catches it too). */
export function shortcut(id: string): Shortcut {
  const s = byID.get(id);
  if (!s) throw new Error(`no shortcut ${id}`);
  return s;
}

/* Whether the event is one of the shortcut's chords. A `twice` shortcut
   matches each press; the caller keeps the timing. */
export function isKey(
  event: Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">,
  id: string,
): boolean {
  return shortcut(id).keys.some((c) => matches(event, c));
}

/* A shifted symbol some senders report as its unshifted key with Shift
   held ("/" + Shift for "?"): both spellings match. */
const SHIFTED: Record<string, string> = { "?": "/" };

function matches(
  e: Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">,
  c: Chord,
): boolean {
  if (c.key.length === 1) {
    const same = e.key.toLowerCase() === c.key.toLowerCase();
    const shifted = e.shiftKey && SHIFTED[c.key] === e.key;
    if (!same && !shifted) return false;
  } else if (e.key !== c.key) {
    return false;
  }
  const mod = e.metaKey || e.ctrlKey;
  if (!!c.mod !== mod) return false;
  if (c.shift !== "any" && !!c.shift !== e.shiftKey) return false;
  if (!!c.alt !== e.altKey) return false;
  return true;
}

/* For a shortcut whose keys are ↑ and ↓: which way the event moves (1
   down, -1 up, 0 for neither). */
export function arrowStep(event: Pick<KeyboardEvent, "key">): 1 | -1 | 0 {
  return event.key === "ArrowDown" ? 1 : event.key === "ArrowUp" ? -1 : 0;
}

/* Whether the platform's primary modifier is ⌘ (a Mac) rather than Ctrl. */
export const MAC =
  typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

/* The modifier's name as the platform writes it, with the joiner the
   labels use after it ("⌘" or "Ctrl+"). */
export const modifierKey = MAC ? "⌘" : "Ctrl+";

/* A chord as the overlay prints it: "⌘K", "Ctrl+K", "Shift+Enter",
   "↑", "Esc". */
export function chordLabel(c: Chord, mac: boolean = MAC): string {
  const parts: string[] = [];
  if (c.mod) parts.push(mac ? "⌘" : "Ctrl");
  if (c.alt) parts.push(mac ? "⌥" : "Alt");
  if (c.shift === true) parts.push(mac ? "⇧" : "Shift");
  parts.push(keyName(c.key));
  return parts.join(mac ? "" : "+");
}

function keyName(key: string): string {
  switch (key) {
    case "ArrowUp":
      return "↑";
    case "ArrowDown":
      return "↓";
    case "ArrowLeft":
      return "←";
    case "ArrowRight":
      return "→";
    case "Escape":
      return "Esc";
    case " ":
      return "Space";
    default:
      return key.length === 1 ? key.toUpperCase() : key;
  }
}

/* A shortcut's keys as one label: the alternatives joined by " or ", a
   `twice` one as "Esc Esc". */
export function keysLabel(s: Shortcut, mac: boolean = MAC): string {
  if (s.twice) return s.keys.map((c) => chordLabel(c, mac)).join(" ") + " " + chordLabel(s.keys[0], mac);
  return s.keys.map((c) => chordLabel(c, mac)).join(" or ");
}

/* The table grouped for the overlay, in AREAS order. */
export function groupedShortcuts(): { id: Area; title: string; rows: Shortcut[] }[] {
  return AREAS.map((a) => ({ ...a, rows: SHORTCUTS.filter((s) => s.area === a.id) }));
}

/* Whether the event's target is somewhere typing goes (an input, a
   textarea, an editable element), where a bare letter is text, not a
   shortcut. */
export function typingIn(target: EventTarget | null): boolean {
  const el = target as { tagName?: string; isContentEditable?: boolean } | null;
  if (!el || typeof el.tagName !== "string") return false;
  return (
    el.tagName === "INPUT" ||
    el.tagName === "TEXTAREA" ||
    el.tagName === "SELECT" ||
    !!el.isContentEditable
  );
}
