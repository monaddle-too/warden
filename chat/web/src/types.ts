export type Entry = {
  id: string;
  role: string;
  text: string;
  detail: string;
  createdAt: number;
  /* When a thinking entry stopped streaming; unset for other entries. */
  endedAt?: number;
  isStreaming: boolean;
  /* A user message's way to the agent: "queued" (held until the turn
     ends), "sending" (handed over, the turn not yet confirmed), "sent",
     "failed" (never delivered, or unconfirmed when the run ended: detail
     says which). Empty for everything else. */
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
  /* What a fork marker records: the chat this one was forked from and
     the message the copy stops before. */
  fork?: Fork;
  /* What an aside entry records: a side question (`text`, by `sender`)
     answered from a copy of the agent's session (`detail`), its cost,
     never sent to the session. */
  aside?: Aside;
  /* What a rewind marker records: the message the chat went back to
     before, the scope, how the session followed and the checkpoint of
     the workspace as it was before a code rewind (`before`; rewind.ts). */
  rewind?: RewindMark;
};
export type RewindMark = {
  messageID: string;
  what: "code" | "conversation" | "both";
  conversation?: "rewound" | "pending" | "fresh" | "";
  before?: string;
};
export type Fork = {
  chatID: string;
  title?: string;
  messageID?: string;
  /* The fork took a copy of the workspace too. */
  workspace?: boolean;
  /* The marker on the source: chatID and title name the fork. */
  into?: boolean;
};
export type Aside = {
  /* "starting" while the chat's released session is brought up for the
     question (the chat shows its startup stages meanwhile). */
  status: "starting" | "running" | "completed" | "failed";
  error?: string;
  costUSD?: number;
  input?: number;
  output?: number;
  durationMS?: number;
  /* The user message the question was asked in chat as ("Ask in chat"). */
  promoted?: string;
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
   its latest model call was given (or the agent's own account of it),
   `window` the model's context window, `threshold` where the agent
   compacts on its own (Claude Code keeps a buffer free below the window;
   absent when not reported), in tokens. */
export type Context = {
  used: number;
  window: number;
  threshold?: number;
  model?: string;
};
/* One agent tool call as the service records it (conversation.Tool):
   `kind` is what the card renders by, `name` the tool as the agent names
   it, `server` an MCP tool's server, `status` running, completed or failed
   (or an agent's own word, such as Codex's declined), `description` what
   the agent said the call is for, `paths` the workspace files it names,
   `query` a search's pattern or a fetch's URL, `input` the call's input
   where the card shows it as given (a todo list's items), `background`
   a command or subagent the agent runs in the background, whose card
   stays running until the task reports back, `progress` a running
   subagent's own account of its work as the agent reports it. */
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
  /* "host" for a jailbroken workspace's host_* tool: the call acted on
     the owner's machine, and the card says so (chats/host.go). */
  target?: "host";
  /* What a read of an image, a PDF or a notebook carried (conversation
     Read); absent for a text read. */
  read?: ToolRead;
  progress?: Progress;
};
export type ToolRead = {
  kind: "image" | "pdf" | "notebook";
  /* The stored copy of an image read (chats/{id}/images/{image}); absent
     when it could not be stored. */
  image?: string;
  width?: number;
  height?: number;
  bytes?: number;
  pages?: number;
  cells?: { type: string; language?: string; text: string }[];
};
/* What a running subagent has done so far (conversation.Progress): what
   it is doing now in the agent's words, the tool calls it made, the tool
   it used last, its time and tokens. */
export type Progress = {
  activity?: string;
  toolCalls: number;
  lastTool?: string;
  durationMS?: number;
  tokens?: number;
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
  /* What "Allow always" remembers: the label and the rule pattern. */
  always?: string;
  rule?: string;
  description?: string;
  plan?: string;
};

/* A permission rule (chats/rules.go): allow, deny or ask by a tool
   pattern in Claude Code's syntax (Bash(git *), Edit(src/**), Read,
   WebFetch(domain:x), mcp__warden__*). origin is "editor" for one typed
   into the rules editor, "always" for an "Allow always" answer (chatID
   that chat when the rule is the workspace's); by is who added it. */
export type Rule = {
  id: string;
  kind: "allow" | "deny" | "ask";
  pattern: string;
  origin?: string;
  chatID?: string;
  by?: Actor;
  at?: number;
};
export type Actor = { principalID: string; email?: string; name?: string };

/* One decision on a tool ask (the chat's permission history): how it was
   decided — "auto" by the mode, "rule" by a rule (rule, scope), "card" by
   the person (by), with the rule an "Allow always" made and a denial's
   message. */
export type PermissionEvent = {
  id: string;
  at: number;
  tool: string;
  summary: string;
  decision: "allow" | "deny";
  how: "auto" | "rule" | "card";
  rule?: Rule;
  scope?: "chat" | "workspace";
  by?: Actor;
  message?: string;
};

/* What environments/{id}/rules and chats/{id}/rules answer. */
export type RulesView = {
  workspace: string;
  rules: Rule[];
  chats: { id: string; title: string; rules: Rule[] }[];
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
// What the execution host (or the cluster's ready nodes) has and what the
// runner's sandboxes already hold of it; GET capacity. Availability
// fields are absent when the platform cannot report them; error explains
// a whole that could not be read while the reservations still count.
export type Capacity = {
  at: string;
  kind: "host" | "cluster";
  cpuMilli: number;
  memoryMB: number;
  cpuPercent?: number;
  load?: number[];
  memoryAvailableMB?: number;
  diskMB?: number;
  diskAvailableMB?: number;
  reserved: Resources;
  running: number;
  spares: number;
  limits: ResourceLimits;
  error?: string;
};
export type Chat = {
  provider?: string;
  model?: string;
  /* A Claude chat's permission mode (auto when absent) and its own
     permission rules ("Allow always" answers kept to this chat, and rules
     added to it); the workspace's are on Environment.rules. */
  mode?: string;
  rules?: Rule[];
  /* A Claude chat's session settings (chats/settings.go): the thinking
     budget ("" the agent's default, "off", or tokens), the effort level
     ("" the model's default) and fast mode. */
  thinking?: string;
  effort?: string;
  fast?: boolean;
  id: string;
  title: string;
  sandboxID: string;
  repository: string;
  resources?: Resources;
  /* The workspace's own network access (network.ts); absent when it
     follows the install's setting. */
  network?: "restricted" | "open";
  /* The workspace has host access (chats/host.go): its agent runs
     commands on this machine as the owner. */
  jailbroken?: boolean;
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
  /* What the agent reported when its session started: the model it
     resolved (the truth after a live model change), its permission mode,
     output style, whether fast mode serves ("on", "off", "cooldown") and
     the auto-memory directory its CLI keeps for the workspace. */
  session?: {
    model?: string;
    permissionMode?: string;
    outputStyle?: string;
    fastMode?: string;
    autoMemory?: string;
  };
  /* Who created the chat (their instructions reach the agent with the
     senders'). */
  creator?: { principalID: string; email?: string; name?: string };
  /* A Claude chat's output style for its next launch ("" or absent: the
     CLI's default); `session.outputStyle` is what the running one has. */
  outputStyle?: string;
  /* The chat was forked from another and its first run still has to copy
     the source's session. */
  forkSession?: boolean;
  /* The rewind marker whose conversation rewind can still be undone (the
     removed transcript is kept until the next turn; rewind.ts). */
  undoRewind?: string;
  /* How the chat got its title: absent while it still has the default one
     and waits to be named from its first exchange, "auto" once it was,
     "manual" once a person named it (chats/title.go). */
  titled?: string;
  /* What the chat's turns took so far, summed by the service from its
     turn records (spend.ts). */
  spend?: Spend;
  typing?: { principalID: string; name: string; until: number }[];
  startup?: Startup;
};
/* The sum of some turns: how many, their tokens, the provider's cost
   estimate where it gave one (priced says whether any turn did; Codex
   reports none). */
export type Spend = {
  turns: number;
  input: number;
  output: number;
  total: number;
  costUSD: number;
  priced: boolean;
  /* Answered side questions, counted in the totals above and marked
     here (their share of costUSD in asideCostUSD). */
  asides?: number;
  asideCostUSD?: number;
};
/* The admin console's totals (GET spend): today, the last seven days and
   all time, each the sum, by provider, and the chats that had a turn. */
export type SpendPeriod = Spend & {
  chats: number;
  providers: Record<string, Spend>;
};
export type SpendReport = {
  today: SpendPeriod;
  week: SpendPeriod;
  all: SpendPeriod;
  at: number;
};
export type AgentCommand = { name: string; description?: string };
/* Where a chat's start is while its message waits for the agent: the
   stage (stages.ts names them), the runtime's detail for it, and when the
   stage began (unix seconds). Absent once the turn is running. */
export type Startup = { stage: string; detail?: string; since: number };
/* A change to a Claude chat's session settings: each field applies when
   present (chats/{id}/settings). */
export type SessionSettings = {
  thinking?: string;
  effort?: string;
  fast?: boolean;
};
/* One row of a provider's model catalog as its CLI reported it
   (chats/catalog.go): the value a chat's model is set to, what it
   resolves to, its name and blurb, the effort levels it takes (none: no
   effort setting), and whether it has adaptive thinking and fast mode. */
export type CatalogModel = {
  value: string;
  resolved?: string;
  label: string;
  description?: string;
  efforts?: string[];
  adaptiveThinking?: boolean;
  fastMode?: boolean;
};
/* The costlier Claude features this Warden allows (config
   providers.claude.allowFastMode, allowLongContext) and each provider's
   model catalog, by provider (absent until its CLI reported one). */
export type AgentOptions = {
  fastMode: boolean;
  longContext: boolean;
  models?: Record<string, CatalogModel[]>;
  /* The model a chat of each provider starts with (chats/defaults.go). */
  defaults?: Record<string, string>;
  /* The composer may attach files from this computer by path (a local
     install, where the chat service runs on the owner's machine). */
  localFiles?: boolean;
  /* Which Warden this is: present for a non-default instance only (its
     state directory's name and the build it runs), so a person with
     several Wardens on one machine knows which one they are looking at. */
  instance?: InstanceInfo;
  /* The owner may give a workspace host access (dogfood.jailbreak;
     chats/host.go). */
  jailbreak?: boolean;
};
export type InstanceInfo = { name: string; version: string };
export type State = {
  version: number;
  chats: Chat[];
  sandboxes?: ResourceLimits;
  agentOptions?: AgentOptions;
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
  /* The pod's recent events, newest first. */
  events: ClusterEvent[];
};
/* One Kubernetes event: what the scheduler, the autoscaler or the kubelet
   said about an object, with the owner's-words hint for the ones that
   decide a start. */
export type ClusterEvent = {
  at: string;
  type: "Normal" | "Warning" | string;
  reason: string;
  message: string;
  hint?: string;
  count: number;
  kind: string;
  namespace: string;
  name: string;
  source?: string;
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
  /* The newest events of the sandbox and service namespaces. */
  events: ClusterEvent[];
  eventsError?: string;
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
  network?: "restricted" | "open";
  jailbroken?: boolean;
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
  /* The workspace's permission rules, applied to every chat of it. */
  rules?: Rule[];
  deleted: boolean;
  archived: boolean;
  /* Set on a workspace created as a copy of another (a fork with "copy
     the workspace"): which one, the chat it was forked from, and when. */
  copiedFrom?: { sandboxID: string; name: string; chatID: string; at: number };
};

/* A person's standing instructions for the agent (me/instructions). */
export type Instructions = {
  text: string;
  updatedAt?: number;
  name?: string;
};
/* One of the workspace's instruction or memory files (chats/{id}/memory):
   scope "workspace" is a path under the workspace root (CLAUDE.md, rules),
   "auto" a path under the CLI's auto-memory directory. */
export type MemoryFile = {
  scope: "workspace" | "auto";
  path: string;
  size: number;
  text: string;
  truncated?: boolean;
};
export type MemoryView = {
  root: string;
  autoDir: string;
  autoDirExists: boolean;
  files: MemoryFile[];
  /* Whether the agent's launch reads these files, and the sentence about it. */
  read: boolean;
  hint?: string;
};
