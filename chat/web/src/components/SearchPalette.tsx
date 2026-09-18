import { useEffect, useMemo, useRef, useState } from "react";
import { Archive, MessageSquare, Search, TextSearch } from "lucide-react";
import { authorLabel, localTime, providerName, senderLabel } from "../export";
import { subagentInput } from "../tools";
import {
  recentChats,
  searchChats,
  snippet,
  type Match,
  type Snippet,
} from "../search";
import type { Chat, Entry } from "../types";

type Row =
  | { kind: "find"; query: string }
  | { kind: "chat"; chat: Chat; match?: Match }
  | {
      kind: "entry";
      chat: Chat;
      entry: Entry;
      field: string;
      match: Match;
      parent?: Entry;
    };

const RECENT = 12;

function Marked({ before, match, after }: Snippet) {
  return (
    <>
      {before}
      <mark>{match}</mark>
      {after}
    </>
  );
}

/* Where an entry's matched text came from, for the row's label: a
   command the person ran is theirs, a subagent's entry names its agent. */
function fieldLabel(
  entry: Entry,
  provider: string | undefined,
  field: string,
  parent?: Entry,
) {
  let label: string;
  if (entry.role === "activity" && entry.sender)
    label = `Command by ${senderLabel(entry.sender)}`;
  else if (entry.role === "activity")
    label = field === "detail" ? "Tool output" : "Agent step";
  else if (entry.role === "thinking") label = "Thinking";
  else if (entry.role === "system") label = "System";
  else if (entry.role === "image") label = "Image";
  else if (entry.role === "aside") label = "Side question";
  else label = authorLabel(entry, provider);
  if (parent) {
    const type = subagentInput(parent.tool).type;
    label += ` · in ${type ? `${type} agent` : "subagent"}`;
  }
  return label;
}

/* ⌘K: a search over every chat's title and entries, from the state the
   browser already has, so nothing is asked of the service. With no query
   it lists the chats last worked on, so it also switches chats. A chat row
   opens the chat; an entry row opens its chat and lands on the entry with
   the query in the find bar; the first row finds the query in the open
   chat. */
export function SearchPalette({
  chats,
  current,
  onOpen,
  onFind,
  onClose,
}: {
  chats: Chat[];
  current?: Chat;
  onOpen: (chat: Chat, entryID?: string, query?: string) => void;
  onFind: (query: string) => void;
  onClose: () => void;
}) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const dialog = useRef<HTMLDialogElement>(null);
  const list = useRef<HTMLUListElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
  }, []);
  const asked = query.trim() !== "";
  const { rows, hits, more } = useMemo(() => {
    if (!asked)
      return {
        rows: recentChats(chats)
          .slice(0, RECENT)
          .map((chat): Row => ({ kind: "chat", chat })),
        hits: 0,
        more: 0,
      };
    const result = searchChats(chats, query);
    const rows: Row[] = current ? [{ kind: "find", query }] : [];
    return {
      rows: rows.concat(result.hits),
      hits: result.hits.length,
      more: result.more,
    };
  }, [chats, query, asked, current]);
  const selected = Math.min(active, Math.max(0, rows.length - 1));
  useEffect(() => {
    list.current
      ?.querySelector('[aria-selected="true"]')
      ?.scrollIntoView({ block: "nearest" });
  }, [selected]);
  function choose(row: Row) {
    if (row.kind === "find") onFind(row.query);
    else if (row.kind === "entry") onOpen(row.chat, row.entry.id, query);
    else onOpen(row.chat);
    dialog.current?.close();
  }
  return (
    <dialog
      ref={dialog}
      className="modal search-palette"
      aria-label="Search chats"
      onClose={onClose}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.preventDefault();
          dialog.current?.close();
        } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
          event.preventDefault();
          if (!rows.length) return;
          const by = event.key === "ArrowDown" ? 1 : -1;
          setActive((selected + by + rows.length) % rows.length);
        } else if (event.key === "Enter") {
          event.preventDefault();
          if (rows[selected]) choose(rows[selected]);
        }
      }}
    >
      <div className="search-input">
        <Search size={16} aria-hidden="true" />
        <input
          autoFocus
          type="text"
          role="combobox"
          aria-label="Search chats and messages"
          aria-expanded={rows.length > 0}
          aria-controls="search-results"
          aria-activedescendant={rows.length ? `search-row-${selected}` : ""}
          aria-autocomplete="list"
          placeholder="Search chats and messages…"
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
            setActive(0);
          }}
        />
      </div>
      <ul id="search-results" role="listbox" ref={list}>
        {rows.map((row, i) => (
          <li
            key={
              row.kind === "find"
                ? "find"
                : row.kind === "chat"
                  ? "chat:" + row.chat.id
                  : "entry:" + row.entry.id
            }
            id={`search-row-${i}`}
            role="option"
            aria-selected={i === selected}
            onMouseMove={() => {
              if (i !== selected) setActive(i);
            }}
            onClick={() => choose(row)}
          >
            {row.kind === "find" ? (
              <>
                <TextSearch size={16} aria-hidden="true" />
                <span className="search-main">
                  Find “{row.query.trim()}” in this chat
                </span>
              </>
            ) : row.kind === "chat" ? (
              <>
                <MessageSquare size={16} aria-hidden="true" />
                <span className="search-main">
                  <span className="search-snippet">
                    {row.match ? (
                      <Marked {...snippet(row.chat.title, row.match, 200)} />
                    ) : (
                      row.chat.title
                    )}
                  </span>
                  <small>
                    {providerName(row.chat.provider)}
                    {row.chat.archived && (
                      <>
                        {" · "}
                        <Archive size={11} aria-hidden="true" /> Archived
                      </>
                    )}
                  </small>
                </span>
              </>
            ) : (
              <>
                <MessageSquare size={16} aria-hidden="true" />
                <span className="search-main">
                  <small>
                    {row.chat.title}
                    {" · "}
                    {fieldLabel(
                      row.entry,
                      row.chat.provider,
                      row.field,
                      row.parent,
                    )}
                    {" · "}
                    {localTime(row.entry.createdAt)}
                    {row.chat.archived && " · Archived"}
                  </small>
                  <span className="search-snippet">
                    <Marked
                      {...snippet(
                        row.field === "detail"
                          ? row.entry.detail
                          : row.entry.text,
                        row.match,
                      )}
                    />
                  </span>
                </span>
              </>
            )}
          </li>
        ))}
      </ul>
      <p className="search-hint muted">
        {!asked
          ? "Type to search chat titles and messages · ↑↓ to move · Enter to open"
          : !hits
            ? "No matches in any chat"
            : more
              ? `${more} more · keep typing to narrow`
              : ""}
      </p>
    </dialog>
  );
}
