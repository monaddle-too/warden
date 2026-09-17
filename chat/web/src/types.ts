export type Entry = {
  id: string;
  role: string;
  text: string;
  detail: string;
  createdAt: number;
  isStreaming: boolean;
  delivery: string;
  sender?: { email?: string; name?: string; principalID: string };
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
  conversation: { threadID?: string; entries: Entry[] };
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
