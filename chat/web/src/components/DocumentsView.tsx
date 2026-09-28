import {
  searchReferences,
  referenceFromURL,
  invalidateDocumentReferences,
} from "../references";
import { useEffect, useRef, useState } from "react";

export function documentRoute(path: string) {
  return /^\/(documents|designs)(\/[-\w]+)?\/?$/.test(path) ? path : "";
}
// The private editor shares our origin and authentication; Warden owns all
// global navigation. The source check prevents unrelated windows changing it.
export function DocumentsView({
  path,
  onNavigate,
  onReference,
}: {
  path: string;
  onNavigate: (path: string) => void;
  onReference: (url: string) => void;
}) {
  const frame = useRef<HTMLIFrameElement>(null);
  const [loading, setLoading] = useState(true);
  const currentPath = useRef(path);
  const [source, setSource] = useState({ path, generation: 0 });
  useEffect(() => {
    if (currentPath.current !== path) {
      currentPath.current = path;
      setLoading(true);
      setSource((previous) => ({ path, generation: previous.generation + 1 }));
    }
  }, [path]);
  useEffect(() => {
    const requests = new Map<string, AbortController>();
    const message = (event: MessageEvent) => {
      if (
        event.origin !== location.origin ||
        event.source !== frame.current?.contentWindow
      )
        return;
      const data = event.data;
      if (data?.type === "warden-references-changed") {
        invalidateDocumentReferences();
        return;
      }
      if (
        data?.type === "warden-reference-search" &&
        typeof data.id === "string" &&
        data.id.length < 100 &&
        typeof data.query === "string" &&
        data.query.length <= 160
      ) {
        // One iframe has one active picker. Abort old work before starting more.
        for (const request of requests.values()) request.abort();
        requests.clear();
        const controller = new AbortController();
        requests.set(data.id, controller);
        const source = event.source as Window;
        void searchReferences(data.query, controller.signal)
          .then((items) => {
            if (
              !controller.signal.aborted &&
              source === frame.current?.contentWindow
            )
              source.postMessage(
                { type: "warden-reference-results", id: data.id, items },
                location.origin,
              );
          })
          .catch(() => {
            if (!controller.signal.aborted)
              source.postMessage(
                {
                  type: "warden-reference-results",
                  id: data.id,
                  error: "Search unavailable. Try again.",
                },
                location.origin,
              );
          })
          .finally(() => requests.delete(data.id));
        return;
      }
      if (
        data?.type === "warden-reference-cancel" &&
        typeof data.id === "string"
      ) {
        requests.get(data.id)?.abort();
        requests.delete(data.id);
        return;
      }
      if (
        data?.type === "warden-reference-navigation" &&
        typeof data.url === "string" &&
        referenceFromURL(data.url)
      ) {
        onReference(data.url);
        return;
      }
      if (
        event.data?.type === "warden-documents-navigation" &&
        documentRoute(event.data.path || "")
      ) {
        if (location.pathname !== event.data.path)
          history.pushState(null, "", event.data.path);
        currentPath.current = event.data.path;
        onNavigate(event.data.path);
        setLoading(false);
      }
    };
    window.addEventListener("message", message);
    return () => {
      window.removeEventListener("message", message);
      for (const request of requests.values()) request.abort();
    };
  }, [onNavigate, onReference]);
  return (
    <section
      className="documents-view"
      aria-label="Documents and designs"
      style={{ position: "relative", flex: 1, minHeight: 0, display: "flex" }}
    >
      {loading && (
        <p role="status" style={{ position: "absolute", top: 12, left: 24 }}>
          Opening documents…
        </p>
      )}
      <iframe
        key={source.generation}
        ref={frame}
        title="Organization documents and designs"
        src={`/docs-app${source.path}`}
        onLoad={() => setLoading(false)}
        style={{
          border: 0,
          width: "100%",
          height: "100%",
          flex: 1,
          background: "var(--bg)",
        }}
      />
    </section>
  );
}
