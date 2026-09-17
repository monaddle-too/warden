export type Entry = {
  id: string;
  role: string;
  text: string;
  detail: string;
  createdAt: number;
  isStreaming: boolean;
  delivery: string;
  /* The agent turn this entry belongs to; a user message gets it once the
     agent accepts the message. */
  turnID?: string;
  sender?: { email?: string; name?: string; principalID: string };
  attachments?: Attachment[];
};
/* The token usage of one turn as the service records it: `input` counts
   every input token (`cached` and `cacheWrite` are parts of it),
   `reasoning` is part of `output`, `costUSD` is the provider's own
   estimate (Claude) or absent. */
export type Usage = {
  input: number;
  cached: number;
  cacheWrite?: number;
  output: number;
  reasoning?: number;
  total: number;
  costUSD?: number;
};
/* The service's record of one agent turn: when it began (the agent
   accepting the message) and ended (0 while it runs), and its usage once
   the provider reports it. */
export type Turn = {
  id: string;
  startedAt?: number;
  endedAt?: number;
  usage?: Usage;
};
/* A file sent with a user message: `kind` is "image" for a PNG/JPEG the
   service normalised to PNG, "file" for anything else; `path` is where the
   agent finds it in the workspace. */
export type Attachment = {
  id: string;
  name: string;
  path: string;
  kind: "image" | "file";
  size: number;
};
export type Question = {
  id: string;
  header?: string;
  question: string;
  options?: { label: string; description?: string }[];
};
export type Approval = {
  id: string;
  method: string;
  state: string;
  params: Record<string, unknown> & { questions?: Question[] };
};
export type Chat = {
  provider?: string;
  model?: string;
  id: string;
  title: string;
  sandboxID: string;
  repository: string;
  status: string;
  archived: boolean;
  error?: string;
  conversation: {
    threadID?: string;
    activeTurnID?: string;
    entries: Entry[];
    turns?: Turn[];
  };
  approvals: Approval[];
  typing?: { principalID: string; name: string; until: number }[];
};
export type State = { version: number; chats: Chat[] };
export type EnvironmentChat = {
  id: string;
  title: string;
  status: string;
  archived: boolean;
};
export type DocumentGrant = {
  request_id: string;
  chatID: string;
  sandboxID: string;
  status: string;
  access: string;
  title: string;
  expires_at: number | null;
  expired?: boolean;
  documents: { id: string; title: string; url: string }[];
};
export type AccessEvent = {
  kind: string; // document_request | repositories_selected | github_disconnected
  status?: string;
  created_at: number;
  resolved_at?: number | null;
  resolved_by?: string;
  expired?: boolean;
  expires_at?: number | null;
  access?: string;
  title?: string;
  reason?: string;
  documents?: { id: string; title: string; url: string }[];
  repositories?: Record<string, string> | null;
};
export type Environment = {
  id: string;
  name: string;
  repository: string;
  chats: EnvironmentChat[];
  runtime: { state: string; runtimeName: string } | null;
  documents: DocumentGrant[];
  repositories: {
    id: number;
    full_name: string;
    url: string;
    access?: string[];
    access_summary?: string;
  }[];
  ports: { id: string; port: number; title: string; url: string }[];
  deleted: boolean;
  archived: boolean;
};
