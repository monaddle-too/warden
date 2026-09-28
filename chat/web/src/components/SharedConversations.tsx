import { useEffect, useState } from "react";
import { externalRequest } from "./ExternalAgents";
import { accountError } from "../account";
import { RichText } from "./RichText";
type Shared = {
  id: string;
  title: string;
  author: string;
  agent: string;
  organizationName: string;
  createdAt: string;
  messages?: { role: string; content: string; name?: string }[];
};
export function sharedRoute(path: string) {
  return /^\/shared-conversations(?:\/[a-f0-9]{32})?\/?$/.test(path)
    ? path
    : "";
}
export function SharedConversations({
  path,
  onNavigate,
}: {
  path: string;
  onNavigate: (path: string) => void;
}) {
  const id = path.split("/")[2] || "";
  const [items, setItems] = useState<Shared[]>([]),
    [detail, setDetail] = useState<Shared>(),
    [next, setNext] = useState(""),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(true);
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let active = true;
    setBusy(true);
    setError("");
    setDetail(undefined);
    setItems([]);
    setNext("");
    const load = id
      ? externalRequest<Shared>(`shared-conversations/${id}`).then((v) => {
          if (active) setDetail(v);
        })
      : externalRequest<{ items: Shared[]; next: string }>(
          "shared-conversations",
        ).then((v) => {
          if (active) {
            setItems(v.items);
            setNext(v.next);
          }
        });
    void load
      .catch((e) => {
        if (active) setError(accountError(e));
      })
      .finally(() => {
        if (active) setBusy(false);
      });
    return () => {
      active = false;
    };
  }, [id, revision]);
  const navigate = (to: string) => {
    history.pushState(null, "", to);
    onNavigate(to);
  };
  async function more() {
    setBusy(true);
    try {
      const v = await externalRequest<{ items: Shared[]; next: string }>(
        `shared-conversations?before=${next}`,
      );
      setItems((old) => [...old, ...v.items]);
      setNext(v.next);
    } catch (e) {
      setError(accountError(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="shared-conversations">
      <header>
        {id && (
          <button
            className="ghost"
            onClick={() => navigate("/shared-conversations")}
          >
            ← Shared conversations
          </button>
        )}
        <h1>{detail?.title || "Shared conversations"}</h1>
        <p>
          {detail
            ? `${detail.organizationName} · Shared by ${detail.author} using ${detail.agent} · ${new Date(detail.createdAt).toLocaleString()}`
            : "Conversations shared with your organization by connected agents."}
        </p>
      </header>
      {error && (
        <p role="alert">
          {error}{" "}
          <button onClick={() => setRevision((v) => v + 1)}>Try again</button>
        </p>
      )}
      {busy && <p role="status">Loading…</p>}
      {detail ? (
        <>
          <p className="settings-hint">
            Shared snapshot · Visible only to this organization · Message names
            and roles were supplied by the uploader.
          </p>
          <button
            className="ghost"
            onClick={() => {
              void navigator.clipboard
                .writeText(location.href)
                .catch(() =>
                  setError("Copy the link from your browser’s address bar."),
                );
            }}
          >
            Copy conversation link
          </button>
          <div className="shared-messages">
            {detail.messages?.map((m, i) => (
              <article className={`shared-message shared-${m.role}`} key={i}>
                <header>
                  <strong>
                    {m.role === "user"
                      ? "Person"
                      : m.role === "assistant"
                        ? "Assistant"
                        : m.role === "tool"
                          ? "Tool"
                          : "System"}
                  </strong>
                  {m.name && <span> · {m.name}</span>}
                </header>
                <RichText text={m.content} agent />
              </article>
            ))}
          </div>
        </>
      ) : (
        !id && (
          <>
            {!busy && !error && !items.length && (
              <div className="settings-card">
                <h2>No shared conversations yet</h2>
                <p>
                  Connect your agent in Personal settings, then ask it to share
                  a conversation with Warden.
                </p>
              </div>
            )}
            <ul className="shared-list">
              {items.map((item) => (
                <li key={item.id}>
                  <a
                    href={`/shared-conversations/${item.id}`}
                    onClick={(e) => {
                      e.preventDefault();
                      navigate(`/shared-conversations/${item.id}`);
                    }}
                  >
                    <strong>{item.title}</strong>
                    <span>
                      {item.author} · {item.agent} ·{" "}
                      {new Date(item.createdAt).toLocaleDateString()}
                    </span>
                  </a>
                </li>
              ))}
            </ul>
            {next && (
              <button disabled={busy} onClick={() => void more()}>
                Load more
              </button>
            )}
          </>
        )
      )}
    </section>
  );
}
