import { useCallback, useEffect, useRef, useState } from "react";

/* A copy button's state: `copy` writes `text()` to the clipboard and
   `copied` holds for 1.5 s so the button can say so. The clipboard is
   unavailable outside secure contexts; the button then simply stays as it
   was and the reader selects the text by hand. */
export function useCopy(text: () => string) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const copy = useCallback(() => {
    void navigator.clipboard?.writeText(text()).then(
      () => {
        setCopied(true);
        clearTimeout(timer.current);
        timer.current = setTimeout(() => setCopied(false), 1500);
      },
      () => {},
    );
  }, [text]);
  return { copied, copy };
}
