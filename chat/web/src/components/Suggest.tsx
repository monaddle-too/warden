import {
  Fragment,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { workspacePaths } from "../api";

/* One row of the composer's suggestion list. */
export type Suggestion = {
  id: string;
  label: ReactNode;
  hint?: string;
  icon?: ReactNode;
  disabled?: boolean;
  /* Paths are shown in the monospace face. */
  mono?: boolean;
  /* Rows with a group are listed under its heading, when the list has
     more than one group (the chat's commands, then the agent's). */
  group?: string;
};

/* A list floating over the composer: the textarea keeps the focus and the
   keys (it is the combobox), so rows take their click on mousedown without
   moving the focus. `note` is a line under the rows, or in their place:
   a failure, "no matches", or how to narrow a long list. */
export function Suggest({
  id,
  items,
  active,
  note,
  onHover,
  onPick,
}: {
  id: string;
  items: Suggestion[];
  active: number;
  note?: string;
  onHover: (index: number) => void;
  onPick: (item: Suggestion) => void;
}) {
  const list = useRef<HTMLUListElement>(null);
  useEffect(() => {
    list.current
      ?.querySelector('[aria-selected="true"]')
      ?.scrollIntoView({ block: "nearest" });
  }, [active]);
  const grouped = new Set(items.map((item) => item.group)).size > 1;
  return (
    <div className="suggest" onMouseDown={(event) => event.preventDefault()}>
      {items.length > 0 && (
        <ul id={id} role="listbox" ref={list}>
          {items.map((item, i) => (
            <Fragment key={item.id}>
              {grouped && item.group && item.group !== items[i - 1]?.group && (
                <li role="presentation" className="suggest-group">
                  {item.group}
                </li>
              )}
              <li
                id={`${id}-${i}`}
                role="option"
                aria-selected={i === active}
                aria-disabled={item.disabled || undefined}
                onMouseMove={() => {
                  if (i !== active && !item.disabled) onHover(i);
                }}
                onClick={() => {
                  if (!item.disabled) onPick(item);
                }}
              >
                {item.icon}
                <span className={item.mono ? "suggest-path" : "suggest-label"}>
                  {item.label}
                </span>
                {item.hint && <small>{item.hint}</small>}
              </li>
            </Fragment>
          ))}
        </ul>
      )}
      {note && <p className="suggest-note">{note}</p>}
    </div>
  );
}

const CACHE_LIMIT = 200;
const DEBOUNCE = 120;

/* Workspace paths for a mention's prefix. The lookup is debounced, an
   answer that arrives after the prefix changed is dropped, and answers are
   remembered per chat so backspacing through a path does not ask again
   (a bounded cache; a failure is not remembered, so the next keystroke
   retries once the workspace is running). */
export function usePathCompletion(chatID: string, query: string | undefined) {
  const cache = useRef(new Map<string, string[]>());
  const [state, setState] = useState<{
    query: string;
    paths?: string[];
    error?: string;
  }>({ query: "" });
  useEffect(() => {
    cache.current = new Map();
  }, [chatID]);
  useEffect(() => {
    if (query === undefined) return;
    const known = cache.current.get(query);
    if (known) {
      setState({ query, paths: known });
      return;
    }
    let stale = false;
    const timer = setTimeout(() => {
      void workspacePaths(chatID, query).then(
        (paths) => {
          if (stale) return;
          const store = cache.current;
          if (store.size >= CACHE_LIMIT)
            store.delete(store.keys().next().value as string);
          store.set(query, paths);
          setState({ query, paths });
        },
        (e: unknown) => {
          if (!stale)
            setState({
              query,
              error: e instanceof Error ? e.message : String(e),
            });
        },
      );
    }, DEBOUNCE);
    return () => {
      stale = true;
      clearTimeout(timer);
    };
  }, [chatID, query]);
  if (query === undefined) return { paths: undefined, error: undefined };
  // While a lookup is in flight the last answer stays up, so the list does
  // not blink between keystrokes.
  return {
    paths: state.paths,
    error: state.query === query ? state.error : undefined,
  };
}
