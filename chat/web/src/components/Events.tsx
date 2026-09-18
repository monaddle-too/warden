import { useState } from "react";
import type { ClusterEvent } from "../types";
import { age } from "../units";

/* Kubernetes events for the owner (docs/startup-detail-events-plan.md):
   what the scheduler, the autoscaler and the kubelet said, newest first.
   A pod's own on the workspace panel and the cluster page; the cluster's
   recent ones on the admin console, where each row also names its
   object. The hint is the owner's-words reading, with the reporter's
   reason and message under it. */
export function EventList({
  events,
  now,
  objects = false,
  limit = 8,
  onObject,
}: {
  events: ClusterEvent[];
  now: number;
  /* Name the object each event is about (the cluster-wide list). */
  objects?: boolean;
  /* Rows shown before "Show all"; 0 for no limit. */
  limit?: number;
  /* Filters the list to that object when an object name is clicked. */
  onObject?: (e: ClusterEvent) => void;
}) {
  const [all, setAll] = useState(false);
  if (!events.length) return null;
  const shown =
    all || !limit || events.length <= limit ? events : events.slice(0, limit);
  return (
    <>
      <ul className={`cluster-events${objects ? " with-objects" : ""}`}>
        {shown.map((e, i) => (
          <li
            key={`${e.namespace}/${e.name}/${e.reason}/${e.at}/${i}`}
            className={e.type === "Warning" ? "warning" : ""}
          >
            <span
              className="cluster-event-when"
              title={new Date(e.at).toLocaleString()}
            >
              {age(e.at, now) || "now"}
              {e.count > 1 && (
                <small className="muted" title={`Repeated ${e.count} times`}>
                  {" "}
                  ×{e.count}
                </small>
              )}
            </span>
            {objects && (
              <span className="cluster-event-object">
                {onObject ? (
                  <button
                    type="button"
                    className="link"
                    title={`Only ${e.kind} ${e.name}`}
                    onClick={() => onObject(e)}
                  >
                    {e.kind} <code>{e.name}</code>
                  </button>
                ) : (
                  <>
                    {e.kind} <code>{e.name}</code>
                  </>
                )}
              </span>
            )}
            <span className="cluster-event-what">
              {e.hint ? (
                <>
                  <span>{e.hint}</span>
                  <small className="muted">
                    {e.reason}
                    {e.message && ` · ${e.message}`}
                    {e.source && ` · ${e.source}`}
                  </small>
                </>
              ) : (
                <>
                  <span>
                    <strong>{e.reason}</strong>
                    {e.message && ` ${e.message}`}
                  </span>
                  {e.source && <small className="muted">{e.source}</small>}
                </>
              )}
            </span>
          </li>
        ))}
      </ul>
      {!all && shown.length < events.length && (
        <button type="button" className="ghost" onClick={() => setAll(true)}>
          Show all {events.length}
        </button>
      )}
    </>
  );
}
