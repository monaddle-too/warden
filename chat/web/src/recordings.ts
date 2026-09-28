import pythonClient from "../../internal/recordings/docs/warden_record.py?raw";

export const devicePythonScript = (secret: string, origin: string) =>
  pythonClient
    .replace('DEVICE_KEY = ""', () => `DEVICE_KEY = ${JSON.stringify(secret)}`)
    .replace('SERVER_URL = ""', () => `SERVER_URL = ${JSON.stringify(origin)}`);

export const recordingRoute = (path: string) =>
  /^\/recordings(?:\/(?:devices|[a-f0-9]{32}))?\/?$/.test(path) ? path : "";
export const duration = (bytes: number) => {
  const seconds = Math.floor(bytes / 32000);
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
};
export const transcriptionLabel = (status: string) =>
  ({
    waiting: "Waiting for audio",
    transcribing: "Transcription in progress",
    complete: "Transcription complete",
    failed: "Transcription needs a retry",
  })[status] || status;
