import { useState, type FormEvent } from "react";

type Props = { onSignedIn: () => void };

export function EmailLogin({ onSignedIn }: Props) {
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setMessage("");
    try {
      const response = await fetch(sent ? "/auth/email/verify" : "/auth/email/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(sent ? { email, code } : { email }),
      });
      const result = (await response.json()) as { error?: string };
      if (!response.ok) throw new Error(result.error || "Sign-in unavailable");
      if (sent) onSignedIn();
      else {
        setSent(true);
        setMessage("If this address has an account, a sign-in code is on its way.");
      }
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "Sign-in unavailable");
    } finally {
      setBusy(false);
    }
  }

  async function passkey() {
    setBusy(true);
    setMessage("");
    try {
      if (!email) throw new Error("Enter your email first.");
      if (!window.PublicKeyCredential?.parseRequestOptionsFromJSON) throw new Error("This browser does not support passkeys.");
      const start = await fetch("/auth/passkeys/login/start", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ email }),
      });
      const challenge = await start.json() as { id: string; options: { publicKey: PublicKeyCredentialRequestOptionsJSON }; error?: string };
      if (!start.ok) throw new Error(challenge.error || "Passkey unavailable");
      const credential = await navigator.credentials.get({ publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(challenge.options.publicKey) }) as PublicKeyCredential | null;
      if (!credential) throw new Error("Passkey verification was cancelled.");
      const finish = await fetch("/auth/passkeys/login/finish", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: challenge.id, credential: credential.toJSON() }),
      });
      const result = await finish.json() as { error?: string };
      if (!finish.ok) throw new Error(result.error || "Passkey verification failed");
      onSignedIn();
    } catch (error) {
      setMessage(error instanceof Error ? error.message : "Passkey unavailable");
    } finally { setBusy(false); }
  }

  return (
    <form className="email-login" onSubmit={submit}>
      <label htmlFor="signin-email">Email address</label>
      <input id="signin-email" type="email" autoComplete="email" required value={email} onChange={(event) => setEmail(event.target.value)} disabled={sent || busy} />
      {sent && <>
        <label htmlFor="signin-code">One-time code</label>
        <input id="signin-code" inputMode="numeric" pattern="[0-9]{8}" autoComplete="one-time-code" required value={code} onChange={(event) => setCode(event.target.value)} disabled={busy} />
      </>}
      <button type="submit" disabled={busy}>{busy ? "Please wait…" : sent ? "Sign in" : "Email me a code"}</button>
      <button type="button" className="ghost" onClick={() => void passkey()} disabled={busy}>Sign in with a passkey</button>
      {sent && <button type="button" className="ghost" onClick={() => { setSent(false); setCode(""); setMessage(""); }}>Use another email</button>}
      {message && <p role="status">{message}</p>}
    </form>
  );
}
