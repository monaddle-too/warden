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
  startup?: Startup;
};
/* Where a chat's start is while its message waits for the agent: the
   stage (stages.ts names them), the runtime's detail for it, and when the
   stage began (unix seconds). Absent once the turn is running. */
export type Startup = { stage: string; detail?: string; since: number };
export type State = { version: number; chats: Chat[] };
export type EnvironmentChat = {
  id: string;
  title: string;
  status: string;
  archived: boolean;
  stage?: string;
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
/* What the sandbox was given and what it is using, as the guest reports it.
   The used figures only mean something while running; cpuPercent needs two
   samples and is null until then. Totals of 0 are unknown (stopped disk). */
export type SandboxUsage = {
  at: string;
  running: boolean;
  cpus: number;
  memoryTotal: number;
  diskTotal: number;
  cpuPercent: number | null;
  memoryUsed: number;
  diskUsed: number;
};
/* CPU in millicores and memory in bytes; 0 is unset. */
export type Resources = { cpuMilli: number; memoryBytes: number };
/* One pod as the owner sees it (Kubernetes). usage is null without a
   metrics server. */
export type PodInfo = {
  namespace: string;
  name: string;
  uid?: string;
  node?: string;
  phase: string;
  reason: string;
  ready: boolean;
  ip?: string;
  started?: string;
  restarts: number;
  runtimeClass?: string;
  containers: string[];
  sandboxID?: string;
  spare?: boolean;
  component?: string;
  requests: Resources;
  limits: Resources;
  usage: Resources | null;
};
export type NodeInfo = {
  name: string;
  ready: boolean;
  roles: string[];
  kubeletVersion?: string;
  containerRuntime?: string;
  os?: string;
  architecture?: string;
  created?: string;
  unschedulable?: boolean;
  capacity: Resources;
  allocatable: Resources;
  usage: Resources | null;
  sandboxPods: number;
};
export type Cluster = {
  available: boolean;
  at: string;
  server?: string;
  sandboxNamespace?: string;
  serviceNamespace?: string;
  tier?: string;
  runtimeClass?: string;
  metrics: boolean;
  metricsError?: string;
  nodes: NodeInfo[];
  nodesError?: string;
  sandboxPods: PodInfo[];
  servicePods: PodInfo[];
  servicePodsError?: string;
  workspaces: Record<string, string>;
};
export type PodLogs = {
  namespace: string;
  pod: string;
  container: string;
  containers: string[];
  lines: string[];
  truncated: boolean;
  at: string;
};
export type Environment = {
  id: string;
  name: string;
  repository: string;
  chats: EnvironmentChat[];
  runtime: { state: string; runtimeName: string } | null;
  usage: SandboxUsage | null;
  pod: PodInfo | null;
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
