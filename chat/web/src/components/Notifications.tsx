import { useCallback, useEffect, useRef, useState } from "react";
import { setFaviconBadge } from "../favicon";
import {
  notifyEvents,
  readNotifyPreference,
  shouldNotify,
  writeNotifyPreference,
  type NotifyPreference,
} from "../notify";
import type { Chat } from "../types";

/* Desktop notifications and the tab's badge (notify.ts decides what
   deserves one). While the tab is hidden, each event shows a browser
   Notification that opens the chat when clicked, and counts on the
   favicon; the count clears when the tab is looked at. The reader turns
   them on from the chat menu, which asks the browser's permission once;
   the choice is kept in localStorage. */
export function useNotifications(
  chats: Chat[],
  open: (chatID: string) => void,
) {
  const supported = typeof Notification !== "undefined";
  const storage = (() => {
    try {
      return localStorage;
    } catch {
      return undefined;
    }
  })();
  const [preference, setPreference] = useState<NotifyPreference>(() =>
    readNotifyPreference(storage),
  );
  const [permission, setPermission] = useState(() =>
    supported ? Notification.permission : "denied",
  );
  const previous = useRef<Chat[] | undefined>(undefined);
  const unread = useRef(0);
  const openRef = useRef(open);
  openRef.current = open;
  // The badge clears when the tab is seen again.
  useEffect(() => {
    const onVisible = () => {
      if (document.visibilityState !== "visible" || unread.current === 0)
        return;
      unread.current = 0;
      setFaviconBadge(0);
    };
    document.addEventListener("visibilitychange", onVisible);
    window.addEventListener("focus", onVisible);
    setFaviconBadge(0);
    return () => {
      document.removeEventListener("visibilitychange", onVisible);
      window.removeEventListener("focus", onVisible);
    };
  }, []);
  useEffect(() => {
    const events = notifyEvents(previous.current, chats);
    previous.current = chats;
    if (!events.length) return;
    const hidden = document.visibilityState === "hidden";
    if (!hidden) return;
    unread.current += events.length;
    setFaviconBadge(unread.current);
    if (!shouldNotify(preference, permission, hidden)) return;
    for (const event of events) {
      try {
        const n = new Notification(event.title, {
          body: event.body,
          tag: event.tag,
        });
        n.onclick = () => {
          window.focus();
          openRef.current(event.chatID);
          n.close();
        };
      } catch {
        /* a browser that refuses the constructor; the badge still counts */
      }
    }
  }, [chats, preference, permission]);
  const enabled = preference === "on" && permission === "granted";
  const toggle = useCallback(async () => {
    if (enabled) {
      writeNotifyPreference(storage, false);
      setPreference("off");
      return;
    }
    if (!supported) return;
    let granted = Notification.permission;
    if (granted !== "granted") {
      try {
        granted = await Notification.requestPermission();
      } catch {
        granted = "denied";
      }
    }
    setPermission(granted);
    writeNotifyPreference(storage, granted === "granted");
    setPreference(granted === "granted" ? "on" : "off");
  }, [enabled, storage, supported]);
  const hint = !supported
    ? "This browser has no notifications"
    : permission === "denied"
      ? "Notifications are blocked for this site in the browser's settings"
      : enabled
        ? "While this tab is hidden: when the agent finishes, asks, or fails"
        : "Get a desktop notification while this tab is hidden when the agent finishes, asks, or fails";
  return { enabled, supported, hint, toggle };
}
