import { plainPrimaryClick } from "../shortcuts";
import {
  recordingRoute,
  duration,
  transcriptionLabel,
  devicePythonScript,
} from "../recordings";
import { useEffect, useState } from "react";
import { api } from "../api";
import "../recordings.css";
type Device = {
  id: string;
  name: string;
  creator: string;
  createdAt: string;
  lastSeenAt: string | null;
  revoked: boolean;
  canManage: boolean;
};
type Recording = {
  id: string;
  title: string;
  deviceName: string;
  status: string;
  transcription: string;
  bytes: number;
  nextSeq: number;
  archivedBytes: number;
  provisional: string;
  createdAt: string;
  error?: string;
  segments?: {
    offsetBytes: number;
    size: number;
    text: string;
    state: string;
  }[];
};
export function Recordings({
  path,
  onNavigate,
}: {
  path: string;
  onNavigate: (path: string) => void;
}) {
  const part = path.split("/")[2] || "",
    devices = part === "devices",
    id = devices ? "" : part;
  const [items, setItems] = useState<Recording[]>([]),
    [detail, setDetail] = useState<Recording>(),
    [deviceList, setDevices] = useState<Device[]>([]),
    [name, setName] = useState(""),
    [secret, setSecret] = useState(""),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [connected, setConnected] = useState(true),
    [confirm, setConfirm] = useState<{
      id: string;
      action: string;
      name: string;
    }>(),
    [copied, setCopied] = useState(false),
    [scriptCopied, setScriptCopied] = useState(false);
  const navigate = (to: string) => {
    history.pushState(null, "", to);
    onNavigate(to);
    setSecret("");
    setError("");
    setConfirm(undefined);
  };
  const refreshDevices = () =>
    api<{ items: Device[] }>("devices").then((v) => setDevices(v.items));
  useEffect(() => {
    setError("");
    setDetail(undefined);
    setItems([]);
    setSecret("");
    let active = true;
    if (devices) {
      void refreshDevices().catch((e) => {
        if (active) setError(e.message);
      });
      return () => {
        active = false;
      };
    }
    const source = new EventSource(
      "/api/recordings/events" + (id ? "?id=" + id : ""),
    );
    source.onmessage = (e) => {
      if (!active) return;
      setConnected(true);
      const v = JSON.parse(e.data);
      if (id) setDetail(v);
      else setItems(v.items);
    };
    source.onerror = () => {
      if (active) setConnected(false);
    };
    void api<Recording | { items: Recording[] }>(
      "recordings" + (id ? "/" + id : ""),
    )
      .then((v) => {
        if (active) {
          if ("items" in v) setItems(v.items);
          else setDetail(v);
        }
      })
      .catch((e) => {
        if (active) setError(e.message);
      });
    return () => {
      active = false;
      source.close();
    };
  }, [path]);
  const run = async (work: () => Promise<void>) => {
    setBusy(true);
    setError("");
    try {
      await work();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Please retry.");
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="recordings-view">
      <header className="recordings-heading">
        <div>
          {(id || devices) && (
            <button className="ghost" onClick={() => navigate("/recordings")}>
              ← Recordings
            </button>
          )}
          <h1>{devices ? "Devices" : detail?.title || "Recordings"}</h1>
          <p>
            {devices
              ? "Connect hardware to this organization."
              : "Audio and transcripts shared with your organization."}
          </p>
        </div>
        <div className="recordings-actions">
          {!devices && (
            <button onClick={() => navigate("/recordings/devices")}>
              Manage devices
            </button>
          )}
          <a href="/recording-api/" target="_blank" rel="noreferrer">
            API guide & Python client ↗
          </a>
        </div>
      </header>
      {error && (
        <p className="recordings-error" role="alert">
          {error}
        </p>
      )}
      {devices ? (
        <>
          <form
            className="device-register"
            onSubmit={(e) => {
              e.preventDefault();
              void run(async () => {
                const v = await api<{ secret: string }>("devices", { name });
                setSecret(v.secret);
                setCopied(false);
                setScriptCopied(false);
                setName("");
                await refreshDevices();
              });
            }}
          >
            <label>
              Device name
              <input
                value={name}
                onChange={(e) => setName(e.target.value)}
                maxLength={120}
                placeholder="XIAO desk recorder"
                required
              />
            </label>
            <button disabled={busy || !name.trim()}>Register device</button>
          </form>
          {secret && (
            <section className="device-secret" aria-label="New device secret">
              <h2>Your device key</h2>
              <p>
                Copy this now. It will not be shown again. It can submit
                recordings for this organization.
              </p>
              <input
                aria-label="Device secret"
                readOnly
                value={secret}
                spellCheck={false}
                autoComplete="off"
              />
              <div className="recordings-actions">
                <button
                  onClick={() =>
                    void navigator.clipboard
                      .writeText(secret)
                      .then(() => setCopied(true))
                      .catch(() =>
                        setError("Select and copy the key manually."),
                      )
                  }
                >
                  {copied ? "Copied" : "Copy key"}
                </button>
                <button onClick={() => setSecret("")}>
                  I’ve saved the key
                </button>
              </div>
              <h3>Ready-to-run Python script</h3>
              <p>
                Your key and server address are included. Save as{" "}
                <strong>warden_record.py</strong>. Keep this script private;
                anyone with it can use your device key.
              </p>
              <button
                onClick={() =>
                  void navigator.clipboard
                    .writeText(devicePythonScript(secret, location.origin))
                    .then(() => setScriptCopied(true))
                    .catch(() =>
                      setError("Select and copy the script manually."),
                    )
                }
              >
                {scriptCopied ? "Script copied" : "Copy Python script"}
              </button>
              <details className="device-script">
                <summary>View script</summary>
                <textarea
                  aria-label="Python script with device key"
                  readOnly
                  spellCheck={false}
                  value={devicePythonScript(secret, location.origin)}
                />
              </details>
              <p>
                Install the streaming dependency, then send a 16 kHz mono PCM16
                WAV:
              </p>
              <code>python3 -m pip install websockets==15.0.1</code>
              <code>python3 warden_record.py stream recording.wav</code>
              <p>
                Use <code className="inline-code">upload</code> instead of{" "}
                <code className="inline-code">stream</code> to send a completed
                recording.
              </p>
            </section>
          )}
          <div className="device-list">
            {deviceList.map((d) => (
              <article key={d.id}>
                <div>
                  <h2>{d.name}</h2>
                  <p>
                    Registered by {d.creator} ·{" "}
                    {d.revoked
                      ? "Revoked"
                      : d.lastSeenAt
                        ? "Last connected " +
                          new Date(d.lastSeenAt).toLocaleString()
                        : "Not connected yet"}
                  </p>
                </div>
                {d.canManage && !d.revoked && (
                  <div className="recordings-actions">
                    <button
                      disabled={busy}
                      onClick={() =>
                        setConfirm({ id: d.id, action: "rotate", name: d.name })
                      }
                    >
                      Replace key
                    </button>
                    <button
                      disabled={busy}
                      onClick={() =>
                        setConfirm({ id: d.id, action: "revoke", name: d.name })
                      }
                    >
                      Revoke
                    </button>
                  </div>
                )}
              </article>
            ))}
          </div>
          {confirm && (
            <section className="device-confirm" role="alert">
              <p>
                {confirm.action === "revoke" ? "Revoke" : "Replace the key for"}{" "}
                <strong>{confirm.name}</strong>? Its current key will stop
                working immediately.
              </p>
              <button disabled={busy} onClick={() => setConfirm(undefined)}>
                Cancel
              </button>{" "}
              <button
                disabled={busy}
                onClick={() =>
                  void run(async () => {
                    const v = await api<{ secret: string }>(
                      `devices/${confirm.id}/${confirm.action}`,
                      {},
                    );
                    setSecret(v.secret);
                    setCopied(false);
                    setScriptCopied(false);
                    setConfirm(undefined);
                    await refreshDevices();
                  })
                }
              >
                Confirm{" "}
                {confirm.action === "revoke" ? "revocation" : "replacement"}
              </button>
            </section>
          )}
        </>
      ) : (
        <>
          {!connected && <p role="status">Reconnecting to live updates…</p>}
          {id ? (
            detail && (
              <>
                <div className="recording-status">
                  <span className={"recording-state " + detail.status}>
                    {detail.status === "live"
                      ? "● Live"
                      : detail.status === "interrupted"
                        ? "Connection interrupted"
                        : detail.status === "finished"
                          ? "Finished"
                          : "Uploading"}
                  </span>
                  <span>{duration(detail.bytes)}</span>
                  <span>{detail.deviceName}</span>
                  <span>{transcriptionLabel(detail.transcription)}</span>
                </div>
                {detail.error && <p role="status">{detail.error}</p>}
                <div className="recordings-actions">
                  {detail.status !== "finished" && (
                    <button
                      disabled={busy}
                      onClick={() =>
                        void run(async () => {
                          await api(`recordings/${id}/finish`, {
                            nextSeq: detail.nextSeq,
                          });
                        })
                      }
                    >
                      Finish recording
                    </button>
                  )}
                  {detail.transcription === "failed" && (
                    <button
                      disabled={busy}
                      onClick={() =>
                        void run(async () => {
                          await api(`recordings/${id}/retry`, {});
                        })
                      }
                    >
                      Retry transcription
                    </button>
                  )}
                </div>
                {detail.status === "finished" &&
                  detail.bytes > 0 &&
                  (detail.archivedBytes === detail.bytes ? (
                    <audio
                      controls
                      preload="none"
                      src={`/api/recordings/${id}/audio`}
                    >
                      Audio playback is not supported.
                    </audio>
                  ) : (
                    <p>Saving audio for playback…</p>
                  ))}
                <article className="recording-transcript">
                  <h2>Transcript</h2>
                  {!detail.segments?.length && !detail.provisional && (
                    <p className="muted">
                      {detail.bytes
                        ? "Listening and transcribing…"
                        : detail.status === "finished"
                          ? "No audio was recorded."
                          : "Waiting for the device to send audio."}
                    </p>
                  )}
                  {detail.segments?.map((s) => (
                    <div className="transcript-segment" key={s.offsetBytes}>
                      <time>{duration(s.offsetBytes)}</time>
                      <p>
                        {s.state === "failed"
                          ? "Transcription failed for this segment. Audio is saved."
                          : s.state === "waiting"
                            ? "Transcribing…"
                            : s.text || "No speech detected."}
                      </p>
                    </div>
                  ))}
                  {detail.provisional && (
                    <div className="transcript-segment provisional">
                      <time>{duration(detail.archivedBytes)}</time>
                      <p>
                        {detail.provisional}
                        <small>Provisional · may change</small>
                      </p>
                    </div>
                  )}
                </article>
              </>
            )
          ) : (
            <div className="recording-list">
              {items.length === 0 ? (
                <div className="recordings-empty">
                  <h2>Ready for your first recording</h2>
                  <p>
                    Register a device, then send audio with the Python client or
                    your hardware.
                  </p>
                  <button onClick={() => navigate("/recordings/devices")}>
                    Register a device
                  </button>
                </div>
              ) : (
                items.map((v) => (
                  <a
                    key={v.id}
                    href={"/recordings/" + v.id}
                    onClick={(e) => {
                      if (!plainPrimaryClick(e.nativeEvent)) return;
                      e.preventDefault();
                      navigate("/recordings/" + v.id);
                    }}
                  >
                    <div>
                      <h2>{v.title}</h2>
                      <p>
                        {v.deviceName} ·{" "}
                        {new Date(v.createdAt).toLocaleString()}
                      </p>
                    </div>
                    <div>
                      <span className={"recording-state " + v.status}>
                        {v.status === "live"
                          ? "● Live"
                          : v.status === "interrupted"
                            ? "Interrupted"
                            : v.status === "finished"
                              ? "Finished"
                              : "Uploading"}
                      </span>
                      <span>{duration(v.bytes)}</span>
                      <small>{transcriptionLabel(v.transcription)}</small>
                    </div>
                  </a>
                ))
              )}
            </div>
          )}
        </>
      )}
    </section>
  );
}
