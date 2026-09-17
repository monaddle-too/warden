import { useCallback, useEffect, useRef, useState } from "react";
import { RefreshCw, ScrollText, Server, X } from "lucide-react";
import { api } from "../api";
import type { Cluster, NodeInfo, PodInfo, PodLogs, Amounts } from "../types";
import { age, cpu, memory, percent, shortenTimestamp } from "../units";

/* The admin console's Cluster section (docs/warden-startup-visibility-
   plan.md): the nodes, the sandbox pods, the Warden service pods and a log
   viewer. Rendered only on the Kubernetes shape, where GET cluster says
   available; it polls every few seconds while shown. */
const REFRESH_MS = 5000;
const LOG_REFRESH_MS = 3000;
const TAILS = [100, 200, 500, 1000, 2000];

type LogTarget = { namespace: string; pod: string; container?: string };

function Meter({
  used,
  total,
  label,
}: {
  used: number;
  total: number;
  label: string;
}) {
  return (
    <meter
      min={0}
      max={100}
      value={percent(used, total)}
      aria-label={label}
      className="cluster-meter"
    />
  );
}

/* "12m of 500m" with a bar, or the limit alone without metrics. */
function Usage({
  usage,
  of,
  kind,
}: {
  usage: Amounts | null;
  of: number;
  kind: "cpu" | "memory";
}) {
  const format = kind === "cpu" ? cpu : memory;
  const used = usage
    ? kind === "cpu"
      ? usage.cpuMilli
      : usage.memoryBytes
    : 0;
  return (
    <span className="cluster-usage">
      <small>
        {usage ? `${format(used)} of ${format(of)}` : `${format(of)} limit`}
      </small>
      {usage && of > 0 && (
        <Meter used={used} total={of} label={`${kind} used`} />
      )}
    </span>
  );
}

function state(p: PodInfo): string {
  if (p.ready) return "Running";
  return p.reason ? `${p.phase} · ${p.reason}` : p.phase;
}

function NodesTable({ nodes, now }: { nodes: NodeInfo[]; now: number }) {
  return (
    <table className="cluster-table">
      <thead>
        <tr>
          <th>Node</th>
          <th>Status</th>
          <th>Roles</th>
          <th>Version</th>
          <th>CPU</th>
          <th>Memory</th>
          <th>Sandboxes</th>
          <th>Age</th>
        </tr>
      </thead>
      <tbody>
        {nodes.map((n) => (
          <tr key={n.name}>
            <td>
              <strong>{n.name}</strong>
              {n.os && <small className="muted"> · {n.os}</small>}
            </td>
            <td>
              <span className={`status-dot ${n.ready ? "running" : "error"}`} />{" "}
              {n.ready ? "Ready" : "Not ready"}
              {n.unschedulable && " · cordoned"}
            </td>
            <td>{n.roles.join(", ") || "worker"}</td>
            <td>
              {n.kubeletVersion}
              {n.containerRuntime && (
                <small className="muted"> · {n.containerRuntime}</small>
              )}
            </td>
            <td>
              <Usage usage={n.usage} of={n.allocatable.cpuMilli} kind="cpu" />
            </td>
            <td>
              <Usage
                usage={n.usage}
                of={n.allocatable.memoryBytes}
                kind="memory"
              />
            </td>
            <td>{n.sandboxPods}</td>
            <td>{age(n.created, now)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function PodsTable({
  pods,
  workspaces,
  service,
  now,
  onLogs,
}: {
  pods: PodInfo[];
  workspaces: Record<string, string>;
  service: boolean;
  now: number;
  onLogs: (t: LogTarget) => void;
}) {
  return (
    <table className="cluster-table">
      <thead>
        <tr>
          <th>{service ? "Service" : "Workspace"}</th>
          <th>Pod</th>
          <th>State</th>
          <th>Node</th>
          <th>CPU</th>
          <th>Memory</th>
          <th>Age</th>
          <th>Restarts</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {pods.map((p) => (
          <tr key={p.namespace + "/" + p.name}>
            <td>
              {service ? (
                <strong>{p.component}</strong>
              ) : p.spare ? (
                <span className="muted">spare (not bound yet)</span>
              ) : (
                <strong>
                  {(p.sandboxID && workspaces[p.sandboxID]) ||
                    (p.sandboxID ? "unknown workspace" : "—")}
                </strong>
              )}
            </td>
            <td>
              <code>{p.name}</code>
              {p.runtimeClass && (
                <small className="muted"> · {p.runtimeClass}</small>
              )}
            </td>
            <td>
              <span
                className={`status-dot ${
                  p.ready
                    ? "running"
                    : ["Failed", "Unknown"].includes(p.phase)
                      ? "error"
                      : "starting"
                }`}
              />{" "}
              {state(p)}
            </td>
            <td>{p.node || <span className="muted">unscheduled</span>}</td>
            <td>
              <Usage
                usage={p.usage}
                of={p.limits.cpuMilli || p.requests.cpuMilli}
                kind="cpu"
              />
            </td>
            <td>
              <Usage
                usage={p.usage}
                of={p.limits.memoryBytes || p.requests.memoryBytes}
                kind="memory"
              />
            </td>
            <td>{age(p.started, now)}</td>
            <td>{p.restarts}</td>
            <td>
              <button
                className="ghost"
                onClick={() =>
                  onLogs({
                    namespace: p.namespace,
                    pod: p.name,
                    container: p.containers[0],
                  })
                }
              >
                <ScrollText size={14} />
                Logs
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function LogViewer({
  target,
  onClose,
}: {
  target: LogTarget;
  onClose: () => void;
}) {
  const [container, setContainer] = useState(target.container || "");
  const [tail, setTail] = useState(200);
  const [previous, setPrevious] = useState(false);
  const [follow, setFollow] = useState(true);
  const [logs, setLogs] = useState<PodLogs>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const pre = useRef<HTMLPreElement>(null);
  const load = useCallback(async () => {
    setBusy(true);
    try {
      const params = new URLSearchParams({
        namespace: target.namespace,
        pod: target.pod,
        tail: String(tail),
      });
      if (container) params.set("container", container);
      if (previous) params.set("previous", "true");
      const r = await api<PodLogs>("cluster/logs?" + params.toString());
      setLogs(r);
      setError("");
      if (!container && r.container) setContainer(r.container);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }, [target, container, tail, previous]);
  useEffect(() => {
    void load();
    if (!follow) return;
    const id = setInterval(() => void load(), LOG_REFRESH_MS);
    return () => clearInterval(id);
  }, [load, follow]);
  useEffect(() => {
    if (follow && pre.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [logs, follow]);
  return (
    <div className="cluster-logs" role="region" aria-label="Pod logs">
      <header>
        <strong>
          {target.namespace}/{target.pod}
        </strong>
        <label>
          Container
          <select
            value={container}
            onChange={(e) => setContainer(e.target.value)}
          >
            {(logs?.containers || [container]).map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </select>
        </label>
        <label>
          Lines
          <select
            value={tail}
            onChange={(e) => setTail(Number(e.target.value))}
          >
            {TAILS.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
        <label className="admin-toggle">
          <input
            type="checkbox"
            checked={previous}
            onChange={(e) => setPrevious(e.target.checked)}
          />
          Previous instance
        </label>
        <label className="admin-toggle">
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => setFollow(e.target.checked)}
          />
          Auto-refresh
        </label>
        <button className="ghost" disabled={busy} onClick={() => void load()}>
          <RefreshCw size={14} />
          Refresh
        </button>
        <button className="ghost" onClick={onClose} aria-label="Close logs">
          <X size={14} />
        </button>
      </header>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      {logs && (
        <>
          {logs.truncated && (
            <p className="muted">Older lines are not shown.</p>
          )}
          <pre ref={pre}>
            {logs.lines.length
              ? logs.lines.map(shortenTimestamp).join("\n")
              : "(no output)"}
          </pre>
          <small className="muted">
            Read {new Date(logs.at).toLocaleTimeString()}
          </small>
        </>
      )}
    </div>
  );
}

export function ClusterView() {
  const [cluster, setCluster] = useState<Cluster>();
  const [error, setError] = useState("");
  const [logs, setLogs] = useState<LogTarget>();
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const r = await api<Cluster>("cluster");
        if (cancelled) return;
        setCluster(r);
        setError("");
        setNow(Date.now());
      } catch (e) {
        if (!cancelled) setError(String(e));
      }
    };
    void load();
    const id = setInterval(load, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, []);
  if (!cluster || !cluster.available) return null;
  return (
    <section aria-labelledby="admin-cluster" className="cluster">
      <h2 id="admin-cluster">
        <Server size={16} />
        Cluster
      </h2>
      <p className="muted">
        {cluster.server && <>Kubernetes {cluster.server} · </>}
        sandboxes in <code>{cluster.sandboxNamespace}</code>
        {cluster.tier && (
          <>
            {" "}
            on the {cluster.tier} tier (RuntimeClass{" "}
            <code>{cluster.runtimeClass}</code>)
          </>
        )}
        {cluster.serviceNamespace && (
          <>
            {" "}
            · Warden in <code>{cluster.serviceNamespace}</code>
          </>
        )}{" "}
        · read {new Date(cluster.at).toLocaleTimeString()}
        {!cluster.metrics && (
          <>
            {" "}
            · live usage unavailable
            {cluster.metricsError && <> ({cluster.metricsError})</>}
          </>
        )}
      </p>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      <h3>Nodes</h3>
      {cluster.nodesError ? (
        <p className="muted">Nodes are not readable: {cluster.nodesError}</p>
      ) : cluster.nodes.length ? (
        <NodesTable nodes={cluster.nodes} now={now} />
      ) : (
        <p className="muted">No nodes listed.</p>
      )}
      <h3>Sandbox pods</h3>
      {cluster.sandboxPods.length ? (
        <PodsTable
          pods={cluster.sandboxPods}
          workspaces={cluster.workspaces}
          service={false}
          now={now}
          onLogs={setLogs}
        />
      ) : (
        <p className="muted">No sandbox pods right now.</p>
      )}
      <h3>Warden services</h3>
      {cluster.servicePodsError ? (
        <p className="muted">
          Service pods are not readable: {cluster.servicePodsError}
        </p>
      ) : cluster.servicePods.length ? (
        <PodsTable
          pods={cluster.servicePods}
          workspaces={{}}
          service
          now={now}
          onLogs={setLogs}
        />
      ) : (
        <p className="muted">No service pods listed.</p>
      )}
      {logs && (
        <LogViewer
          key={logs.namespace + "/" + logs.pod}
          target={logs}
          onClose={() => setLogs(undefined)}
        />
      )}
    </section>
  );
}
