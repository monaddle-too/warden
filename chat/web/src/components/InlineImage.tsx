import { useEffect, useState } from "react";
import { workspaceImageBlob } from "../api";
import { imageLoader } from "../images";
import { Lightbox } from "./Lightbox";

const load = imageLoader(workspaceImageBlob);

/* An `![alt](path)` whose source is a workspace file: fetched through the
   chat service (which re-encodes it), shown as a thumbnail that opens in a
   lightbox. Until it arrives, and if the file is missing or not an image,
   the alt text stands in, like a source that was not a workspace path. */
export function InlineImage({
  chatID,
  entryID,
  path,
  alt,
}: {
  chatID: string;
  entryID: string;
  path: string;
  alt?: string;
}) {
  const [url, setURL] = useState("");
  const [error, setError] = useState(false);
  const [open, setOpen] = useState(false);
  useEffect(() => {
    let stopped = false,
      object = "";
    setURL("");
    setError(false);
    void load(chatID, entryID, path).then(
      (blob) => {
        if (stopped) return;
        object = URL.createObjectURL(blob);
        setURL(object);
      },
      () => {
        if (!stopped) setError(true);
      },
    );
    return () => {
      stopped = true;
      if (object) URL.revokeObjectURL(object);
    };
  }, [chatID, entryID, path]);
  const label = alt || path.split("/").pop() || "Image";
  if (error)
    return (
      <span
        className="inline-image-missing"
        title={`${path}: image unavailable`}
      >
        {label}
      </span>
    );
  if (!url) return <span className="inline-image-pending">{label}</span>;
  return (
    <>
      <button
        type="button"
        className="inline-image"
        title={path}
        onClick={() => setOpen(true)}
      >
        <img src={url} alt={label} />
      </button>
      {open && (
        <Lightbox url={url} alt={label} onClose={() => setOpen(false)} />
      )}
    </>
  );
}
