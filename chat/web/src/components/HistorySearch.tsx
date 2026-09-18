// Ctrl-R in the composer: an incremental reverse search over this
// person's earlier prompts in the chat (history.ts). The query box takes
// the focus; Up/Down (or Ctrl-R again) move through the matches, Enter
// puts the chosen prompt into the composer, Escape gives the draft back.
import { useEffect, useMemo, useRef, useState } from "react";
import { isKey } from "../shortcuts";
import { History } from "lucide-react";
import { promptLine, searchHistory } from "../history";
import { Suggest, type Suggestion } from "./Suggest";

export function HistorySearch({
  history,
  onPick,
  onClose,
}: {
  history: string[];
  onPick: (text: string) => void;
  onClose: () => void;
}) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const matches = useMemo(
    () => searchHistory(history, query),
    [history, query],
  );
  useEffect(() => {
    input.current?.focus();
  }, []);
  useEffect(() => {
    setActive(0);
  }, [matches]);
  const items = matches.map(
    (text, i): Suggestion => ({
      id: "history:" + i,
      label: promptLine(text),
      icon: <History size={15} />,
    }),
  );
  const selected = Math.min(active, items.length - 1);
  return (
    <div className="history-search">
      <Suggest
        id="composer-history"
        items={items}
        active={selected}
        note={
          !history.length
            ? "No earlier prompts in this chat"
            : !items.length
              ? "No prompt matches"
              : undefined
        }
        onHover={setActive}
        onPick={(item) => onPick(matches[Number(item.id.slice(8))])}
      />
      <div className="history-query">
        <span className="muted">reverse-i-search</span>
        <input
          ref={input}
          type="text"
          value={query}
          placeholder="Search your earlier prompts"
          aria-label="Search your earlier prompts"
          aria-controls="composer-history"
          aria-activedescendant={
            selected >= 0 ? `composer-history-${selected}` : undefined
          }
          onChange={(e) => setQuery(e.target.value)}
          onBlur={onClose}
          onKeyDown={(e) => {
            if (isKey(e, "history-close")) {
              e.preventDefault();
              onClose();
            } else if (isKey(e, "history-next")) {
              // Down the list is further back in time; Ctrl-R again too.
              e.preventDefault();
              if (items.length)
                setActive((a) => (a + 1 + items.length) % items.length);
            } else if (isKey(e, "history-prev")) {
              e.preventDefault();
              if (items.length)
                setActive((a) => (a - 1 + items.length) % items.length);
            } else if (isKey(e, "history-pick")) {
              e.preventDefault();
              if (selected >= 0) onPick(matches[selected]);
              else onClose();
            }
          }}
        />
      </div>
    </div>
  );
}
