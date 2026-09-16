import { useEffect, useState } from "react";
import { ExternalLink, Lock } from "lucide-react";
import { api } from "../api";
type Binding = {
  id: string;
  chatID: string;
  port: number;
  title: string;
  url: string;
  state: string;
};
export function Previews({
  chatID,
  open,
  onCount,
}: {
  chatID: string;
  open: boolean;
  onCount: (count: number) => void;
}) {
  const [bindings, setBindings] = useState<Binding[]>([]);
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    const poll = async () => {
      try {
        const res = await api<Binding[] | null>("ports");
        if (active)
          setBindings(
            (res || []).filter(
              (p) => p.chatID === chatID && p.state === "approved",
            ),
          );
      } catch (e) {
        if (active) setError(String(e));
      }
    };
    void poll();
    const timer = setInterval(poll, 2000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [chatID]);
  useEffect(() => {
    onCount(bindings.length);
    return () => onCount(0);
  }, [bindings.length, onCount]);
  async function revoke(id: string) {
    try {
      await api(`ports/${id}/revoke`, {});
      setBindings((old) => old.filter((p) => p.id !== id));
    } catch (e) {
      setError(String(e));
    }
  }
  if (!bindings.length || !open) return null;
  return (
    <aside className="warden-previews">
      <header>
        <strong>Published ports</strong>
      </header>
      {bindings.map((p) => (
        <section className="published-port" key={p.id}>
          <h3>{p.title}</h3>
          <p className="muted">
            Port {p.port} · <Lock size={12} /> Warden sign-in required
          </p>
          <a href={p.url} target="_blank" rel="noopener noreferrer">
            Open preview <ExternalLink size={13} />
          </a>
          <button onClick={() => revoke(p.id)}>Unpublish</button>
        </section>
      ))}
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
    </aside>
  );
}
