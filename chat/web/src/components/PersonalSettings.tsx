import { ConnectedAgents } from "./ExternalAgents";
import { useEffect, useState, type FormEvent } from "react";
import {
  accountError,
  accountRequest,
  enrollPasskey,
  type Passkey,
  type Profile,
} from "../account";

export function PersonalSettings({
  csrf,
  onChanged,
}: {
  csrf: string;
  onChanged: () => void;
}) {
  const [profile, setProfile] = useState<Profile>();
  const [name, setName] = useState("");
  const [keyName, setKeyName] = useState("My passkey");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("");
  const [error, setError] = useState("");
  const [revision, setRevision] = useState(0);
  useEffect(() => {
    let active = true;
    void accountRequest<Profile>(csrf, "profile")
      .then((value) => {
        if (active) {
          setProfile(value);
          setName(value.name);
        }
      })
      .catch((e) => {
        if (active) setError(accountError(e));
      });
    return () => {
      active = false;
    };
  }, [csrf, revision]);
  async function refresh() {
    setProfile(await accountRequest<Profile>(csrf, "profile"));
    onChanged();
  }
  async function action(work: () => Promise<void>, success: string) {
    setBusy(true);
    setError("");
    setStatus("");
    try {
      await work();
      setStatus(success);
    } catch (e) {
      setError(accountError(e));
    } finally {
      setBusy(false);
    }
  }
  function saveName(event: FormEvent) {
    event.preventDefault();
    void action(async () => {
      const result = await accountRequest<{ name: string }>(
        csrf,
        "profile",
        "POST",
        { name },
      );
      setName(result.name);
      await refresh();
    }, "Your name has been saved.");
  }
  function enroll(replacement?: Passkey) {
    void action(
      async () => {
        setStatus("Follow your browser’s prompt to save a passkey.");
        await enrollPasskey(
          csrf,
          replacement?.name || keyName,
          replacement?.id,
        );
        await refresh();
      },
      replacement
        ? "Passkey updated. The previous passkey no longer signs in to Warden."
        : "Passkey saved. You can use it next time you sign in.",
    );
  }
  function remove(key: Passkey) {
    if (
      !confirm(
        `Remove “${key.name}” from Warden? You can still sign in using an emailed code. This does not delete it from your device’s password manager.`,
      )
    )
      return;
    void action(async () => {
      await accountRequest(
        csrf,
        `passkeys/${encodeURIComponent(key.id)}`,
        "DELETE",
      );
      await refresh();
    }, "Passkey removed. Email sign-in is still available.");
  }
  return (
    <section className="personal-settings">
      <header>
        <h1>Personal settings</h1>
        <p>Your profile and sign-in preferences apply across organizations.</p>
      </header>
      {status && (
        <p role="status" className="settings-status">
          {status}
        </p>
      )}
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      {!profile ? (
        <>
          <p>Loading your settings…</p>
          {error && (
            <button
              onClick={() => {
                setError("");
                setRevision((value) => value + 1);
              }}
            >
              Try again
            </button>
          )}
        </>
      ) : (
        <>
          <ConnectedAgents csrf={csrf} />
          <section className="settings-card">
            <h2>Profile</h2>
            <p>{profile.email}</p>
            <form onSubmit={saveName}>
              <label htmlFor="personal-name">Your name</label>
              <input
                id="personal-name"
                autoComplete="name"
                required
                maxLength={120}
                value={name}
                disabled={busy}
                onChange={(e) => setName(e.target.value)}
              />
              <button disabled={busy || name.trim() === profile.name}>
                Save name
              </button>
            </form>
          </section>
          <section className="settings-card">
            <h2>Passkeys</h2>
            <p>
              Sign in conveniently with your device or password manager. You can
              always use an emailed code instead.
            </p>
            {profile.passkeys.length === 0 ? (
              <p>No passkey set up yet.</p>
            ) : (
              <ul className="settings-passkeys">
                {profile.passkeys.map((key) => (
                  <li key={key.id}>
                    <PasskeyDetails
                      passkey={key}
                      busy={busy}
                      onRename={(newName) =>
                        void action(async () => {
                          await accountRequest(
                            csrf,
                            `passkeys/${encodeURIComponent(key.id)}`,
                            "PATCH",
                            { name: newName },
                          );
                          await refresh();
                        }, "Passkey name saved.")
                      }
                    />
                    <div className="settings-actions">
                      <button
                        className="ghost"
                        disabled={busy}
                        onClick={() => enroll(key)}
                      >
                        Update passkey
                      </button>
                      <button
                        className="ghost"
                        disabled={busy}
                        onClick={() => remove(key)}
                      >
                        Remove passkey
                      </button>
                    </div>
                    <p className="settings-hint">
                      Updating creates a replacement. This passkey stays active
                      until its replacement is saved.
                    </p>
                  </li>
                ))}
              </ul>
            )}
            <form
              onSubmit={(e) => {
                e.preventDefault();
                enroll();
              }}
            >
              <label htmlFor="new-passkey-name">Passkey name</label>
              <input
                id="new-passkey-name"
                value={keyName}
                required
                maxLength={120}
                disabled={busy}
                onChange={(e) => setKeyName(e.target.value)}
                placeholder="For example, my laptop"
              />
              <button disabled={busy}>
                {busy
                  ? "Please wait…"
                  : profile.passkeys.length
                    ? "Add another passkey"
                    : "Add passkey"}
              </button>
            </form>
          </section>
        </>
      )}
    </section>
  );
}
function PasskeyDetails({
  passkey,
  busy,
  onRename,
}: {
  passkey: Passkey;
  busy: boolean;
  onRename: (name: string) => void;
}) {
  const [name, setName] = useState(passkey.name);
  useEffect(() => {
    setName(passkey.name);
  }, [passkey.name]);
  return (
    <>
      <form
        className="passkey-name-form"
        onSubmit={(e) => {
          e.preventDefault();
          onRename(name);
        }}
      >
        <label htmlFor={`key-${passkey.id}`}>Passkey name</label>
        <input
          id={`key-${passkey.id}`}
          value={name}
          required
          maxLength={120}
          disabled={busy}
          onChange={(e) => setName(e.target.value)}
        />
        <button
          className="ghost"
          disabled={busy || name.trim() === passkey.name}
        >
          Save passkey name
        </button>
      </form>
      <p className="settings-hint">
        Added {new Date(passkey.createdAt).toLocaleDateString()}
        {passkey.lastUsedAt
          ? ` · Last used ${new Date(passkey.lastUsedAt).toLocaleDateString()}`
          : " · Not used yet"}
      </p>
    </>
  );
}

export function PasskeyInvitation({
  csrf,
  onDone,
}: {
  csrf: string;
  onDone: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState("");
  async function choose(setup: boolean) {
    setBusy(true);
    setError("");
    try {
      if (setup) {
        await enrollPasskey(csrf, "My passkey");
        setSaved(true);
      } else {
        await accountRequest(csrf, "profile/passkey-prompt", "POST");
        onDone();
      }
    } catch (e) {
      setError(accountError(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="signin">
      <section className="passkey-invitation">
        <h1>
          {saved ? "Your passkey is ready" : "Make your next sign-in easier"}
        </h1>
        <p>
          {saved
            ? "You can now sign in with your passkey. Manage it anytime in Personal settings."
            : "Would you like to set up a passkey? Use your device or password manager to sign in without waiting for an email."}
        </p>
        {!saved && (
          <p>
            This is optional. You can keep using emailed codes and set up a
            passkey later in Personal settings.
          </p>
        )}
        <div className="settings-actions">
          {saved ? (
            <button onClick={onDone}>Continue</button>
          ) : (
            <>
              <button disabled={busy} onClick={() => void choose(true)}>
                {busy ? "Please wait…" : "Set up a passkey"}
              </button>
              <button
                className="ghost"
                disabled={busy}
                onClick={() => void choose(false)}
              >
                Not now
              </button>
            </>
          )}
        </div>
        {busy && (
          <p role="status">
            Follow your browser’s prompt if you chose passkey setup.
          </p>
        )}
        {error && <p role="alert">{error}</p>}
      </section>
    </main>
  );
}
