import { useEffect, useState } from "react";
import { imageBlob } from "../api";
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
        <a href={url} target="_blank" rel="noreferrer">
          <img src={url} alt={caption} />
        </a>
      ) : (
        <p>{error ? "Image unavailable" : "Loading image…"}</p>
      )}
      <figcaption>{caption}</figcaption>
    </figure>
  );
}
