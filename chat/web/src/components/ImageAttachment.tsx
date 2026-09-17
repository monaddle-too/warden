import { useEffect, useState } from "react";
import { imageBlob } from "../api";
import { Lightbox } from "./Lightbox";
export function ImageAttachment({
  chatID,
  id,
  caption = "Image attachment",
}: {
  chatID: string;
  id: string;
  caption?: string;
}) {
  const [url, setURL] = useState("");
  const [error, setError] = useState(false);
  const [open, setOpen] = useState(false);
  useEffect(() => {
    let stopped = false,
      object = "";
    setURL("");
    setError(false);
    void imageBlob(chatID, id)
      .then((blob) => {
        if (!stopped) {
          object = URL.createObjectURL(blob);
          setURL(object);
        }
      })
      .catch(() => {
        if (!stopped) setError(true);
      });
    return () => {
      stopped = true;
      if (object) URL.revokeObjectURL(object);
    };
  }, [chatID, id]);
  return (
    <figure className="image-attachment">
      {url ? (
        <button
          type="button"
          className="inline-image"
          title="View full size"
          onClick={() => setOpen(true)}
        >
          <img src={url} alt={caption} />
        </button>
      ) : (
        <p>{error ? "Image unavailable" : "Loading image…"}</p>
      )}
      <figcaption>{caption}</figcaption>
      {open && url && (
        <Lightbox url={url} alt={caption} onClose={() => setOpen(false)} />
      )}
    </figure>
  );
}
