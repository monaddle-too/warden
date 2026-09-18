export type Entry = {
  id: string;
  role: string;
  text: string;
  detail: string;
  createdAt: number;
  /* When a thinking entry stopped streaming; unset for other entries. */
  endedAt?: number;
  isStreaming: boolean;
  delivery: string;
  /* The agent turn this entry belongs to; a user message gets it once the
     agent accepts the message. */
  turnID?: string;
  sender?: { email?: string; name?: string; principalID: string };
  attachments?: Attachment[];
  /* The tool call an activity entry records; with it, `text` is the
     call's title and `detail` its output alone (a command's output, a
     search's hits, a unified diff per changed file). Absent on entries
     from before it was recorded, whose detail starts with a status line. */
  tool?: Tool;
  /* The subagent this entry belongs to: the ID of the task entry (the
     Agent tool call) whose subagent produced it, so the transcript nests
     it under that card. Absent on the conversation's own entries. */
  parentID?: string;
  /* What a compaction entry records: the agent compacted its context
     here; `detail` is the summary it continues from, when given. */
  compaction?: Compaction;
};
/* One compaction of the agent's context (conversation.Compaction):
   `trigger` is "manual" (the owner's /compact) or "auto" (the agent near
   its window), `preTokens` the context before it and `postTokens` the
   summary it came down to (absent when not reported); `status` running,
   completed or failed with `error`. */
export type Compaction = {
  trigger?: string;
  preTokens?: number;
  postTokens?: number;
  status: string;
  error?: string;
};
/* How full the agent's context is (conversation.Context): `used` is what
   its latest model call was given (or its estimate right after a
   compaction), `window` the model's context window, in tokens. */
export type Context = { used: number; window: number; model?: string };
/* One agent tool call as the service records it (conversation.Tool):
   `kind` is what the card renders by, `name` the tool as the agent names
   it, `server` an MCP tool's server, `status` running, completed or failed
   (or an agent's own word, such as Codex's declined), `description` what
   the agent said the call is for, `paths` the workspace files it names,
   `query` a search's pattern or a fetch's URL, `input` the call's input
   where the card shows it as given (a todo list's items), `background`
   a command or subagent the agent runs in the background, whose card
   stays running until the task reports back. */
export type Tool = {
  kind: ToolKind;
  name?: string;
  server?: string;
  status: string;
  description?: string;
  paths?: string[];
  query?: string;
  input?: Record<string, unknown>;
  background?: boolean;
};
export type ToolKind =
  | "command"
  | "edit"
  | "read"
  | "search"
  | "fetch"
  | "webSearch"
  | "mcp"
  | "task"
  | "todo"
  | "other";
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
/* A tool permission ask (method item/tool/requestPermission): the tool,
   its input, the call as a transcript entry (a command, a diff), what
   "Allow always" would remember, and, for ExitPlanMode, the plan. */
export type PermissionParams = {
  tool: string;
  input?: Record<string, unknown>;
  entry?: Entry;
  always?: string;
  description?: string;
  plan?: string;
};
export type Approval = {
  id: string;
  method: string;
  state: string;
  params: Record<string, unknown> & { questions?: Question[] };
};
// A workspace's size: CPU in millicores (1000 = one CPU), memory in MiB.
export type Resources = { cpuMilli: number; memoryMB: number };
// The runner's size offer: the default a fresh workspace gets, the most
// any workspace may have, the CPU step the platform takes (1000 on SBX,
// 250 on Kubernetes) and whether a resize restarts the sandbox.
export type ResourceLimits = {
  default: Resources;
  max: Resources;
  cpuStepMilli: number;
  restart: boolean;
};
export type Chat = {
  provider?: string;
  model?: string;
  /* A Claude chat's permission mode (auto when absent) and its
     allow-always rules: the tool (Bash, edit for any file tool, or a
     tool's name) and, for Bash, the command prefix. */
  mode?: string;
  allowed?: { tool: string; command?: string }[];
  id: string;
  title: string;
  sandboxID: string;
  repository: string;
  resources?: Resources;
  status: string;
  archived: boolean;
  error?: string;
  conversation: {
    threadID?: string;
    activeTurnID?: string;
    entries: Entry[];
    turns?: Turn[];
    context?: Context;
  };
  approvals: Approval[];
  /* Slash commands the agent's session offers (Claude Code's built-ins and
     the workspace's own commands and skills); "/name …" is sent as text
     and the agent expands it. Absent for Codex. */
  commands?: AgentCommand[];
  /* What the agent reported when its session started. */
  session?: { model?: string; permissionMode?: string; outputStyle?: string };
  typing?: { principalID: string; name: string; until: number }[];
  startup?: Startup;
};
export type AgentCommand = { name: string; description?: string };
/* Where a chat's start is while its message waits for the agent: the
   stage (stages.ts names them), the runtime's detail for it, and when the
   stage began (unix seconds). Absent once the turn is running. */
export type Startup = { stage: string; detail?: string; since: number };
export type State = {
  version: number;
  chats: Chat[];
  sandboxes?: ResourceLimits;
};
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
/* CPU in millicores and memory in bytes as the cluster reports them; 0 is
   unset. A workspace's size is Resources. */
export type Amounts = { cpuMilli: number; memoryBytes: number };
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
  requests: Amounts;
  limits: Amounts;
  usage: Amounts | null;
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
  capacity: Amounts;
  allocatable: Amounts;
  usage: Amounts | null;
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
  resources?: Resources;
  /* A resize in flight, or how the last one ended. */
  resizing?: {
    target: Resources;
    started: number;
    done: boolean;
    error?: string;
  };
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
