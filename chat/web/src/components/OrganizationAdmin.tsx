import { useEffect, useState, type FormEvent } from "react";
import { api, apiDeleteBody } from "../api";

type Organization = { id: string; name: string };
type Member = {
  id: string;
  email: string;
  name: string;
  role: "admin" | "user";
  fullAdmin: boolean;
};

export function OrganizationAdmin({
  organization,
  organizations,
  fullAdmin,
  onChanged,
  onSelect,
}: {
  organization?: Organization;
  organizations: Organization[];
  fullAdmin: boolean;
  onChanged: () => void;
  onSelect: (id: string) => void;
}) {
  const [orgs, setOrgs] = useState(organizations);
  const [selectedID, setSelectedID] = useState(
    organization?.id || organizations[0]?.id || "",
  );
  const selected = orgs.find((org) => org.id === selectedID);
  const [members, setMembers] = useState<Member[]>([]);
  const [revision, setRevision] = useState(0);
  const [loading, setLoading] = useState(false);
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<"admin" | "user">("user");
  const [newOrganization, setNewOrganization] = useState("");
  const [error, setError] = useState("");
  const [status, setStatus] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    setOrgs(organizations);
  }, [organizations]);
  useEffect(() => {
    let active = true;
    setMembers([]);
    if (!selectedID) return;
    setLoading(true);
    void api<Member[]>(`admin/organizations/${selectedID}/members`)
      .then((result) => {
        if (active) setMembers(result);
      })
      .catch((e) => {
        if (active) setError(String(e));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [selectedID, revision]);
  function manage(id: string) {
    setSelectedID(id);
    setError("");
    setStatus("");
  }
  async function add(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!selected) return;
    setBusy(true);
    setError("");
    setStatus("");
    try {
      const result = await api<{ invitationSent: boolean }>(
        `admin/organizations/${selected.id}/members`,
        {
          email,
          name,
          role,
        },
      );
      setStatus(
        `${name} added to ${selected.name} as ${role === "admin" ? "an organization admin" : "a user"}. ${result.invitationSent ? "Invitation email sent." : "Invitation email could not be sent. Add this same user again to retry; their access is already active."}`,
      );
      setEmail("");
      setName("");
      setRole("user");
      setRevision((value) => value + 1);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function remove(member: Member) {
    if (!selected || !confirm(`Remove ${member.name} from ${selected.name}?`))
      return;
    setBusy(true);
    setError("");
    setStatus("");
    try {
      await apiDeleteBody(`admin/organizations/${selected.id}/members`, {
        email: member.email,
      });
      setStatus(`${member.name} removed from ${selected.name}.`);
      setRevision((value) => value + 1);
      onChanged();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError("");
    setStatus("");
    try {
      const org = await api<Organization>("admin/organizations", {
        name: newOrganization,
      });
      setOrgs((current) => [...current, org]);
      setSelectedID(org.id);
      setStatus(`${org.name} created. Add its users and administrators below.`);
      setNewOrganization("");
      onChanged();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="organization-admin">
      <header>
        <h1>{fullAdmin ? "Platform admin" : "Organization admin"}</h1>
        <p>
          Current organization:{" "}
          <strong>{organization?.name || "None selected"}</strong>
        </p>
      </header>
      {status && (
        <p className="organization-status" role="status">
          {status}
        </p>
      )}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <div className="organization-admin-grid">
        <section className="organization-card">
          <h2>
            Organizations <small>({orgs.length})</small>
          </h2>
          <p>Select an organization to manage its users.</p>
          <ul className="organization-list">
            {orgs.map((org) => (
              <li key={org.id}>
                <button
                  className="ghost"
                  aria-pressed={org.id === selectedID}
                  disabled={busy}
                  onClick={() => manage(org.id)}
                >
                  <strong>{org.name}</strong>
                  {org.id === organization?.id && (
                    <small>Current organization</small>
                  )}
                </button>
              </li>
            ))}
          </ul>
          {orgs.length === 0 && <p>No organizations yet.</p>}
          {fullAdmin && (
            <form onSubmit={create}>
              <label htmlFor="organization-name">Create organization</label>
              <input
                id="organization-name"
                value={newOrganization}
                onChange={(e) => setNewOrganization(e.target.value)}
                required
                maxLength={120}
                placeholder="Organization name"
                disabled={busy}
              />
              <button disabled={busy}>Create organization</button>
            </form>
          )}
        </section>
        {selected && (
          <section className="organization-card">
            <h2>{selected.name}</h2>
            {selected.id !== organization?.id && (
              <button
                className="ghost"
                disabled={busy}
                onClick={() => onSelect(selected.id)}
              >
                Use this organization
              </button>
            )}
            <form onSubmit={add}>
              <h3>Add user</h3>
              <label htmlFor="member-organization">
                Assign to organization
              </label>
              <select
                id="member-organization"
                value={selectedID}
                disabled={busy}
                onChange={(e) => manage(e.target.value)}
              >
                {orgs.map((org) => (
                  <option key={org.id} value={org.id}>
                    {org.name}
                  </option>
                ))}
              </select>
              <label htmlFor="member-name">Name</label>
              <input
                id="member-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
                maxLength={120}
                disabled={busy}
              />
              <label htmlFor="member-email">Email</label>
              <input
                id="member-email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
                disabled={busy}
              />
              {fullAdmin && (
                <>
                  <label htmlFor="member-role">Role</label>
                  <select
                    id="member-role"
                    value={role}
                    disabled={busy}
                    onChange={(e) =>
                      setRole(e.target.value as "admin" | "user")
                    }
                  >
                    <option value="user">User</option>
                    <option value="admin">Organization admin</option>
                  </select>
                </>
              )}
              <button disabled={busy}>Add user to {selected.name}</button>
            </form>
            <h3>
              Users in {selected.name} {!loading && `(${members.length})`}
            </h3>
            {loading ? (
              <p role="status">Loading users…</p>
            ) : (
              <ul className="organization-members">
                {members.map((member) => (
                  <li key={member.id}>
                    <div>
                      <strong>{member.name}</strong>
                      <span>{member.email}</span>
                      <small>
                        {member.fullAdmin
                          ? "Platform admin"
                          : member.role === "admin"
                            ? "Organization admin"
                            : "User"}
                      </small>
                    </div>
                    <button
                      className="ghost"
                      disabled={
                        busy ||
                        member.fullAdmin ||
                        (!fullAdmin && member.role === "admin")
                      }
                      onClick={() => void remove(member)}
                    >
                      Remove
                    </button>
                  </li>
                ))}
              </ul>
            )}
            {!loading && members.length === 0 && (
              <p>No users in this organization.</p>
            )}
          </section>
        )}
      </div>
    </section>
  );
}
