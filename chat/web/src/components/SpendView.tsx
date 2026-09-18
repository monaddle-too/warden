import { useCallback, useEffect, useState } from "react";
import { Coins, RefreshCw } from "lucide-react";
import { api } from "../api";
import { formatCost, formatTokens } from "../turns";
import type { Spend, SpendPeriod, SpendReport } from "../types";

/* The admin console's Spend section (docs/claude-parity.md, R2.2): what
   the agents' turns took today, over the last seven days and all time,
   by provider, summed by the service over every chat (GET spend). Claude
   reports a cost estimate per turn; Codex tokens alone. */

const providerName = (p: string) =>
  p === "claude" ? "Claude" : p === "codex" ? "Codex" : p;

function cell(s: Spend | undefined): string {
  if (!s || !s.turns) return "—";
  const tokens = `${formatTokens(s.total)} tokens`;
  return s.priced ? `${formatCost(s.costUSD)} · ${tokens}` : tokens;
}

export function providersOf(report: SpendReport): string[] {
  return Object.keys(report.all.providers || {}).sort();
}

export function SpendView() {
  const [report, setReport] = useState<SpendReport>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setReport(await api<SpendReport>("spend"));
    } catch (e) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);
  const providers = report ? providersOf(report) : [];
  const rows: [string, SpendPeriod][] = report
    ? [
        ["Today", report.today],
        ["Last 7 days", report.week],
        ["All time", report.all],
      ]
    : [];
  return (
    <section aria-labelledby="admin-spend" className="admin-spend">
      <h2 id="admin-spend">
        <Coins size={16} />
        Spend
        <button
          className="ghost"
          disabled={loading}
          onClick={() => void load()}
          title="Recompute from every chat's turns"
        >
          <RefreshCw size={15} />
          Refresh
        </button>
      </h2>
      <p className="muted">
        What the agents' turns took, summed over every chat (archived ones
        included) from the usage each turn reported: Claude's own cost estimate,
        Codex's tokens without a price. Side questions and chat titles are not
        turns and are left out.
      </p>
      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}
      {loading && !report && <p className="muted">Loading…</p>}
      {report && (
        <table className="cluster-table spend-table">
          <thead>
            <tr>
              <th>Period</th>
              <th>Total</th>
              {providers.map((p) => (
                <th key={p}>{providerName(p)}</th>
              ))}
              <th>Turns</th>
              <th>Chats</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(([name, period]) => (
              <tr key={name}>
                <td>{name}</td>
                <td>{cell(period)}</td>
                {providers.map((p) => (
                  <td key={p}>{cell(period.providers?.[p])}</td>
                ))}
                <td>{period.turns}</td>
                <td>{period.chats}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {report && !report.all.turns && (
        <p className="muted">No agent turns recorded yet.</p>
      )}
    </section>
  );
}
