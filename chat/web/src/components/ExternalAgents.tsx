import { useEffect, useState } from "react";
import { accountError } from "../account";

export async function externalRequest<T>(
  path: string,
  csrf = "",
  method = "GET",
  body?: unknown,
): Promise<T> {
  const response = await fetch(`/auth/${path}`, {
    method,
    headers: { "Content-Type": "application/json", "X-Warden-CSRF": csrf },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) throw new Error(await response.text());
  return response.status === 204 ? (undefined as T) : response.json();
}
type Connection = {
  id: string;
  name: string;
  organizationName: string;
  createdAt: string;
  lastUsedAt: string | null;
};
export function ConnectedAgents({ csrf }: { csrf: string }) {
  const [data, setData] = useState<{
    url: string;
    connections: Connection[];
  }>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [status, setStatus] = useState("");
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let active = true;
    externalRequest<{ url: string; connections: Connection[] }>(
      "agent-connections",
    )
      .then((v) => {
        if (active) setData(v);
      })
      .catch((e) => {
        if (active) setError(accountError(e));
      });
    return () => {
      active = false;
    };
  }, [revision]);
  async function disconnect(connection: Connection) {
    setBusy(connection.id);
    setError("");
    try {
      await externalRequest(
        `agent-connections/${connection.id}`,
        csrf,
        "DELETE",
      );
      setRevision((v) => v + 1);
      setStatus(
        `${connection.name} disconnected from ${connection.organizationName}.`,
      );
    } catch (e) {
      setError(accountError(e));
    } finally {
      setBusy("");
    }
  }
  return (
    <section className="settings-card">
      <h2>Connected agents</h2>
      <p>
        Bring your own agent to Warden. Add this URL as a remote MCP server in
        your agent’s settings, then sign in and choose an organization.
      </p>
      <label htmlFor="mcp-server-url">MCP server URL</label>
      <div className="settings-actions">
        <input
          id="mcp-server-url"
          readOnly
          value={data?.url || `${location.origin}/mcp`}
        />
        <button
          onClick={() => {
            void navigator.clipboard
              .writeText(data?.url || `${location.origin}/mcp`)
              .then(
                () => setStatus("Server URL copied."),
                () => setError("Select and copy the server URL above."),
              );
          }}
        >
          Copy URL
        </button>
      </div>
      <p className="settings-hint">
        Use a client that supports remote HTTP MCP and OAuth sign-in. A
        connection can read, create, edit, comment on and suggest changes to all
        documents in its organization, and upload conversations you choose to
        share. Switching organizations here does not change an existing
        connection.
      </p>
      {status && <p role="status">{status}</p>}
      {error && (
        <p role="alert">
          {error}{" "}
          <button
            onClick={() => {
              setError("");
              setRevision((v) => v + 1);
            }}
          >
            Try again
          </button>
        </p>
      )}
      {!data ? (
        <p>Loading connections…</p>
      ) : data.connections.length === 0 ? (
        <p>No agents connected yet.</p>
      ) : (
        <ul className="settings-passkeys">
          {data.connections.map((c) => (
            <li key={c.id}>
              <strong>{c.name}</strong>
              <p>{c.organizationName}</p>
              <p className="settings-hint">
                Connected {new Date(c.createdAt).toLocaleDateString()} ·{" "}
                {c.lastUsedAt
                  ? `Last used ${new Date(c.lastUsedAt).toLocaleString()}`
                  : "Not used yet"}
              </p>
              <button disabled={!!busy} onClick={() => void disconnect(c)}>
                {busy === c.id ? "Disconnecting…" : "Disconnect"}
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
export function AgentConsent({
  csrf,
  organizations,
  selected,
}: {
  csrf: string;
  organizations: { id: string; name: string }[];
  selected?: string;
}) {
  const id = new URLSearchParams(location.search).get("request") || "";
  const [info, setInfo] = useState<{
    clientName: string;
    redirectUri: string;
  }>();
  const [organization, setOrganization] = useState(
    selected || (organizations.length === 1 ? organizations[0].id : ""),
  );
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let active = true;
    externalRequest<{ clientName: string; redirectUri: string }>(
      `agent-authorization/${encodeURIComponent(id)}`,
    )
      .then((v) => {
        if (active) setInfo(v);
      })
      .catch((e) => {
        if (active) setError(accountError(e));
      });
    return () => {
      active = false;
    };
  }, [id]);
  async function decide(allow: boolean) {
    setBusy(true);
    setError("");
    try {
      const result = await externalRequest<{ redirect: string }>(
        `agent-authorization/${encodeURIComponent(id)}`,
        csrf,
        "POST",
        { allow, organizationId: organization },
      );
      location.assign(result.redirect);
    } catch (e) {
      setError(accountError(e));
      setBusy(false);
    }
  }
  return (
    <main className="signin">
      <section className="agent-consent">
        <h1>Connect an agent</h1>
        {info ? (
          <>
            <p>
              <strong>{info.clientName}</strong> wants to connect to Warden.
            </p>
            <p className="settings-hint">
              Client names are supplied by the agent. After connecting, your
              browser returns to{" "}
              <strong>{new URL(info.redirectUri).host}</strong>.
            </p>
            <label htmlFor="connect-organization">Organization</label>
            <select
              id="connect-organization"
              value={organization}
              disabled={busy}
              onChange={(e) => setOrganization(e.target.value)}
            >
              <option value="" disabled>
                Choose an organization
              </option>
              {organizations.map((o) => (
                <option key={o.id} value={o.id}>
                  {o.name}
                </option>
              ))}
            </select>
            <p>This agent will be able to:</p>
            <ul>
              <li>Read, create and edit all documents in this organization.</li>
              <li>Add comments and propose suggestions for your review.</li>
              <li>
                Upload conversations you choose to share with everyone in this
                organization.
              </li>
            </ul>
            <p>
              You can disconnect it anytime in Personal settings. Shared
              conversations remain available to your organization.
            </p>
            <div className="settings-actions">
              <button
                disabled={busy || !organization}
                onClick={() => void decide(true)}
              >
                {busy ? "Please wait…" : "Connect agent"}
              </button>
              <button
                className="ghost"
                disabled={busy}
                onClick={() => void decide(false)}
              >
                Cancel
              </button>
            </div>
          </>
        ) : (
          !error && <p>Loading connection request…</p>
        )}
        {error && <p role="alert">{error}</p>}
      </section>
    </main>
  );
}
