export type Passkey = {
  id: string;
  name: string;
  createdAt: string;
  lastUsedAt: string | null;
};
export type Profile = { name: string; email: string; passkeys: Passkey[] };

export async function accountRequest<T>(
  csrf: string,
  path: string,
  method = "GET",
  body?: unknown,
): Promise<T> {
  const response = await fetch(`/auth/${path}`, {
    method,
    headers: { "Content-Type": "application/json", "X-Warden-CSRF": csrf },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const result = await response.json();
  if (!response.ok)
    throw new Error(
      result.error || "Unable to update your account. Please try again.",
    );
  return result as T;
}
export function accountError(error: unknown) {
  return error instanceof DOMException && error.name === "NotAllowedError"
    ? "Passkey setup was cancelled or timed out. You can try again."
    : error instanceof Error
      ? error.message
      : "Unable to update your account.";
}
export async function enrollPasskey(
  csrf: string,
  name: string,
  replaceID?: string,
) {
  if (!globalThis.PublicKeyCredential?.parseCreationOptionsFromJSON)
    throw new Error(
      "This browser does not support passkey setup. You can keep signing in by email.",
    );
  const challenge = await accountRequest<{
    id: string;
    options: { publicKey: PublicKeyCredentialCreationOptionsJSON };
  }>(csrf, "passkeys/register/start", "POST", { name, replaceID });
  const credential = (await navigator.credentials.create({
    publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(
      challenge.options.publicKey,
    ),
  })) as PublicKeyCredential | null;
  if (!credential) throw new Error("Passkey setup was cancelled.");
  await accountRequest(csrf, "passkeys/register/finish", "POST", {
    id: challenge.id,
    credential: credential.toJSON(),
  });
}
