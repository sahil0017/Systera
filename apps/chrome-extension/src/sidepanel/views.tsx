// Side panel investigation screens (spec §16.2): Architecture Discovery, Architecture X-Ray,
// Alternatives, Full Investigation and Compare websites (§7.6). Each view loads its data only
// when it is opened, from the lens-scoped /v1/websites routes.
import React, { useEffect, useState } from "react";
import { LensAPIClient } from "../api/client";
import { CounterfactualSet, EvidenceClass, Finding, Fingerprint, WebsiteComparison, WebsiteProfile } from "../shared/types";

export type ViewKey = "discovery" | "xray" | "alternatives" | "investigation" | "compare";

export const VIEWS: Array<{ key: ViewKey; label: string }> = [
  { key: "discovery", label: "Discovery" },
  { key: "xray", label: "X-Ray" },
  { key: "alternatives", label: "Options" },
  { key: "investigation", label: "Agents" },
  { key: "compare", label: "Compare" },
];

const LAYER_LABELS: Record<string, string> = {
  capability: "Capabilities",
  domain_boundary: "Domains",
  user_journey: "User journeys",
  service_dependency: "Services",
  api_interaction: "APIs",
  data_entity: "Data",
  event_flow: "Events",
  infrastructure: "Infrastructure",
};

export function Badge({ cls }: { cls: EvidenceClass | string }) {
  return <span className={`badge ${cls}`}>{cls}</span>;
}

function useLoad<T>(load: () => Promise<T>, deps: unknown[]) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    setData(null);
    setError("");
    load().then((d) => live && setData(d)).catch((e: Error) => live && setError(e.message));
    return () => {
      live = false;
    };
  }, deps);
  return { data, error };
}

function Loading({ error }: { error: string }) {
  if (error) return <p className="error">{error}</p>;
  return (
    <div className="skeleton" aria-busy="true" aria-label="Loading">
      <div className="sk-line w60" /><div className="sk-line w90" /><div className="sk-line w75" /><div className="sk-line w40" />
    </div>
  );
}

function Section({ title, count, children }: { title: string; count?: number; children: React.ReactNode }) {
  return (
    <div className="view-section">
      <h3>{title}{count !== undefined && <span className="count">{count}</span>}</h3>
      {children}
    </div>
  );
}

function FindingList({ items, empty = "None yet." }: { items: Finding[]; empty?: string }) {
  if (items.length === 0) return <p className="fine">{empty}</p>;
  return (
    <div className="findings">
      {items.map((f) => (
        <details key={f.id} className="finding">
          <summary>
            <Badge cls={f.evidence_class} />
            <span className="finding-name">{f.name}</span>
            <span className={`conf ${f.confidence}`} title={`${f.confidence} confidence`}>{f.confidence?.[0] ?? ""}</span>
          </summary>
          <div className="finding-body">
            {f.statement && f.statement !== f.name && <p>{f.statement}</p>}
            {f.rationale && <p className="fine">{f.rationale}</p>}
            {f.clue && <p className="fine">Clue: <code>{f.clue}</code></p>}
            <p className="fine">{f.source === "AGENT" && f.agent_type ? `Recorded by ${f.agent_type.replace(/_/g, " ")}` : "Baseline analysis"} · cites {f.observation_ids.length} page{f.observation_ids.length === 1 ? "" : "s"}</p>
          </div>
        </details>
      ))}
    </div>
  );
}

export function DiscoveryView({ client, profile }: { client: LensAPIClient; profile: WebsiteProfile }) {
  const { data, error } = useLoad(async () => {
    const [caps, hyps] = await Promise.all([client.listFindings(profile.id, "capabilities"), client.listFindings(profile.id, "hypotheses")]);
    return { caps: caps.items, hyps: hyps.items };
  }, [profile.id, profile.status]);
  if (!data) return <Loading error={error} />;
  const by = (xs: Finding[], kind: string) => xs.filter((f) => f.kind === kind);
  const sections: Array<[string, Finding[], string]> = [
    ["Capabilities", by(data.caps, "CAPABILITY"), "No capabilities detected on the scanned pages."],
    ["User journeys", by(data.caps, "JOURNEY"), "No journeys yet. Product map mode captures more pages."],
    ["Domains", by(data.hyps, "DOMAIN"), "Domains are proposed by the Domain Architect agent (Agents tab)."],
    ["Technology signals (passive)", by(data.caps, "TECHNOLOGY_SIGNAL"), "No technology clues in what the page loaded."],
  ];
  return (
    <div>
      {sections.map(([t, items, empty]) => (
        <Section key={t} title={t} count={items.length}><FindingList items={items} empty={empty} /></Section>
      ))}
    </div>
  );
}

export function XRayView({ client, profile }: { client: LensAPIClient; profile: WebsiteProfile }) {
  const { data, error } = useLoad(() => client.getFingerprint(profile.id), [profile.id, profile.status]);
  if (!data) return <Loading error={error} />;
  const fp: Fingerprint = data;
  return (
    <div>
      <p className="muted">{fp.disclaimer}</p>
      {Object.keys(LAYER_LABELS).map((layer) => {
        const els = fp.layers?.[layer]?.elements ?? [];
        return (
          <details key={layer} open={els.length > 0 && els.length <= 6}>
            <summary>{LAYER_LABELS[layer]} ({els.length})</summary>
            <ul>
              {els.map((e) => (
                <li key={e.node_id}><Badge cls={e.evidence_class} /> {e.name} <span className="muted">{e.node_type} · {e.confidence}</span></li>
              ))}
            </ul>
          </details>
        );
      })}
      <h3>Security, reliability &amp; quality attributes</h3>
      {(fp.quality_attributes ?? []).length === 0 ? <p className="muted">No quality attributes recorded yet.</p> : (
        <ul>
          {(fp.quality_attributes ?? []).map((q, i) => (
            <li key={i}><Badge cls={q.evidence_class} /> {q.attribute}: {q.target} <span className="muted">{q.rationale}</span></li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function AlternativesView({ client, profile }: { client: LensAPIClient; profile: WebsiteProfile }) {
  const { data, error } = useLoad(() => client.getAlternatives(profile.id), [profile.id, profile.status]);
  if (!data) return <Loading error={error} />;
  const set: CounterfactualSet = data;
  const constraints = set.decision_matrix?.constraints ?? [];
  return (
    <div>
      <p className="muted">{set.decision_matrix?.recommended_reason}</p>
      <div className="table-wrap">
        <table className="matrix">
          <thead>
            <tr><th>Constraint</th>{set.alternatives.map((a) => <th key={a.id}>{a.name}{a.is_recommended ? " ★" : ""}</th>)}</tr>
          </thead>
          <tbody>
            {constraints.map((c) => (
              <tr key={c}>
                <td>{c.replace(/_/g, " ")}</td>
                {set.alternatives.map((a) => <td key={a.id} title={a.scores[c]?.rationale}>{a.scores[c]?.score.toFixed(1) ?? "–"}</td>)}
              </tr>
            ))}
            <tr><td><strong>Weighted</strong></td>{set.alternatives.map((a) => <td key={a.id}><strong>{a.weighted_score.toFixed(2)}</strong></td>)}</tr>
          </tbody>
        </table>
      </div>
      {set.alternatives.map((a) => (
        <details key={a.id}>
          <summary>{a.name}</summary>
          <p>{a.summary}</p>
          <p className="muted"><strong>When appropriate:</strong> {a.when_appropriate}</p>
          <p className="muted"><strong>Strengths:</strong> {a.pros}</p>
          <p className="muted"><strong>Costs / risks:</strong> {a.cons}</p>
        </details>
      ))}
    </div>
  );
}

const DESIGN_KINDS: Array<[string, string]> = [
  ["SYSTEM_DESIGN", "Proposed HLD"],
  ["COMPONENT", "Components"],
  ["DATA_ENTITY", "Database entities"],
  ["API", "APIs & events"],
  ["INFRASTRUCTURE", "Infrastructure"],
];

const POLL_MS = 5000;
const POLL_MAX = 60; // about five minutes

export function InvestigationView({ client, profile, onStarted }: {
  client: LensAPIClient; profile: WebsiteProfile;
  onStarted: (p: WebsiteProfile, message: string) => void;
}) {
  const [items, setItems] = useState<Finding[] | null>(null);
  const [error, setError] = useState("");
  const [starting, setStarting] = useState(false);
  const [polls, setPolls] = useState(0);
  const [fresh, setFresh] = useState(0);

  // Load, and while an agent run is attached keep refreshing so findings stream in.
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let n = 0;
    let prev = -1;
    const tick = async () => {
      try {
        const r = await client.listFindings(profile.id, "hypotheses");
        if (!live) return;
        const agentCount = r.items.filter((f) => f.source === "AGENT").length;
        if (prev >= 0 && agentCount > prev) setFresh((x) => x + agentCount - prev);
        prev = agentCount;
        setItems(r.items);
      } catch (e) {
        if (live) setError((e as Error).message);
      }
      n++;
      if (live) setPolls(n);
      if (live && profile.run_id && n < POLL_MAX) timer = setTimeout(tick, POLL_MS);
    };
    void tick();
    return () => {
      live = false;
      if (timer) clearTimeout(timer);
    };
  }, [client, profile.id, profile.run_id]);

  const start = async () => {
    setError("");
    setStarting(true);
    try {
      const r = await client.startAnalysis(profile.id, `lens-analysis:${profile.id}`);
      onStarted({ ...profile, ...r.website }, `${r.tasks.length} specialist agents are working on ${profile.domain}; the Independent Reviewer runs last. Findings appear in the Agents tab as they're recorded.`);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setStarting(false);
    }
  };
  const agents = items?.filter((f) => f.source === "AGENT").length ?? 0;
  const watching = !!profile.run_id && polls < POLL_MAX;
  return (
    <div>
      {profile.run_id ? (
        <div className={`run-status ${watching ? "live" : ""}`}>
          {watching && <span className="pulse" aria-hidden="true" />}
          <div>
            <strong>{watching ? "Agents are working" : "Agent run attached"}</strong>
            <div className="fine">{agents} agent finding{agents === 1 ? "" : "s"} so far{fresh > 0 ? ` · ${fresh} new` : ""}{watching ? " · refreshing every 5 s" : ""}</div>
          </div>
        </div>
      ) : (
        <div className="run-status">
          <div>
            <strong>Run the specialist agents</strong>
            <div className="fine">Agents look deeper than the baseline analysis: domains, data, APIs and infrastructure, then an Independent Reviewer checks every finding. Creates a workspace project.</div>
          </div>
          <button className="primary" onClick={start} disabled={starting}>{starting ? <span className="spinner" /> : "Start agents"}</button>
        </div>
      )}
      {error && <p className="error">{error}</p>}
      {!items ? <Loading error={error} /> : DESIGN_KINDS.map(([kind, label]) => {
        const xs = items.filter((f) => f.kind === kind);
        return <Section key={kind} title={label} count={xs.length}><FindingList items={xs} empty={profile.run_id ? "Waiting for agents…" : "Start the agents to propose these."} /></Section>;
      })}
    </div>
  );
}

export function CompareView({ client, profile }: { client: LensAPIClient; profile: WebsiteProfile }) {
  const { data: list, error } = useLoad(() => client.listWebsites(), [profile.id]);
  const [picked, setPicked] = useState<string[]>([]);
  const [cmp, setCmp] = useState<WebsiteComparison | null>(null);
  const [cmpError, setCmpError] = useState("");
  if (!list) return <Loading error={error} />;
  const others = list.items.filter((w) => w.id !== profile.id);
  if (others.length === 0) return <p className="muted">Investigate another website first; then you can compare it with {profile.domain} here using the same capability taxonomy.</p>;
  const domainOf = (id: string) => cmp?.sites.find((s) => s.website_id === id)?.domain ?? id;
  const compare = async () => {
    setCmpError("");
    try {
      setCmp(await client.compareWebsites([profile.id, ...picked]));
    } catch (e) {
      setCmpError((e as Error).message);
    }
  };
  return (
    <div>
      <p className="fine">Pick up to 4 of your other investigations to compare with {profile.domain}, using the same capability list.</p>
      {others.slice(0, 30).map((w) => (
        <label key={w.id} className="item">
          <input type="checkbox" checked={picked.includes(w.id)} disabled={!picked.includes(w.id) && picked.length >= 4}
            onChange={(e) => setPicked(e.target.checked ? [...picked, w.id] : picked.filter((x) => x !== w.id))} />
          <span>{w.domain} <span className="muted">{w.created_at ? new Date(w.created_at).toLocaleDateString() : ""}</span></span>
        </label>
      ))}
      <button className="primary block" onClick={compare} disabled={picked.length === 0}>{picked.length === 0 ? "Pick at least one site" : `Compare ${picked.length + 1} websites`}</button>
      {cmpError && <p className="error">{cmpError}</p>}
      {cmp && (
        <div>
          <p className="muted">{cmp.disclaimer}</p>
          {cmp.notes.map((n) => <p key={n} className="muted">⚠ {n}</p>)}
          <div className="table-wrap">
            <table className="matrix">
              <thead><tr><th>Capability</th>{cmp.sites.map((s) => <th key={s.website_id}>{s.domain}</th>)}</tr></thead>
              <tbody>
                {cmp.capabilities.map((row) => (
                  <tr key={row.key}>
                    <td>{row.name}</td>
                    {cmp.sites.map((s) => <td key={s.website_id}>{row.sites[s.website_id]?.present ? "●" : "–"}</td>)}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p><strong>Shared:</strong> {cmp.shared_capabilities.join(", ") || "none"}</p>
          {cmp.sites.map((s) => (
            <p key={s.website_id} className="muted">
              <strong>Only {s.domain}:</strong> {(cmp.unique_capabilities[s.website_id] ?? []).join(", ") || "none"}
              {(cmp.unmapped_capabilities[s.website_id] ?? []).length > 0 && ` · outside the taxonomy: ${cmp.unmapped_capabilities[s.website_id].join(", ")}`}
            </p>
          ))}
          {cmp.pairs.map((p) => (
            <p key={p.a + p.b} className="muted">{domainOf(p.a)} vs {domainOf(p.b)}: {(p.capability_similarity * 100).toFixed(0)}% capability overlap{p.shared_technology.length > 0 ? `; shared technology: ${p.shared_technology.join(", ")}` : ""}</p>
          ))}
        </div>
      )}
    </div>
  );
}
