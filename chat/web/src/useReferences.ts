import { useEffect, useState } from "react";
import {
  localReferences,
  searchReferences,
  type Reference,
} from "./references";
export function useReferences(query: string | undefined) {
  const [state, setState] = useState<{
    query?: string;
    items: Reference[];
    note?: string;
  }>({ items: [] });
  useEffect(() => {
    if (query === undefined) {
      setState({ items: [] });
      return;
    }
    const controller = new AbortController();
    setState({ query, items: localReferences(query), note: "Searching…" });
    const timer = setTimeout(() => {
      void searchReferences(query, controller.signal)
        .then((items) => {
          if (!controller.signal.aborted)
            setState({
              query,
              items,
              note: items.length
                ? undefined
                : "No matching documents or conversations",
            });
        })
        .catch((error) => {
          if (!controller.signal.aborted)
            setState({ query, items: [], note: error.message });
        });
    }, 140);
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [query]);
  return state.query === query ? state : { items: [], note: "Searching…" };
}
