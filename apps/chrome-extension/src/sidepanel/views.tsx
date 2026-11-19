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
  { key: "alternatives", label: "Alternatives" },
  { key: "investigation", label: "Full investigation" },
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
  return error ? <p className="error">{error}</p> : <p className="muted">Loading…</p>;
}

function FindingList({ items }: { items: Finding[] }) {
  if (items.length === 0) return <p className="muted">None yet.</p>;
  return (
    <ul>
      {items.map((f) => (
        <li key={f.id}>
          <Badge cls={f.evidence_class} /> {f.name} <span className="muted">({f.confidence}{f.source === "AGENT" && f.agent_type ? `, ${f.agent_type}` : ""})</span>
          {f.rationale && <div className="muted">{f.rationale}</div>}
        </li>
      ))}
    </ul>
  );
}

export function DiscoveryView({ client, profile }: { client: LensAPIClient; profile: WebsiteProfile }) {
  const { data, error } = useLoad(async () => {
    const [caps, hyps] = await Promise.all([client.listFindings(profile.id, "capabilities"), client.listFindings(profile.id, "hypotheses")]);
    return { caps: caps.items, hyps: hyps.items };
  }, [profile.id, profile.status]);
  if (!data) return <Loading error={error} />;
  const by = (xs: Finding[], kind: string) => xs.filter((f) => f.kind === kind);
  return (
    <div>
      <h3>Capability map</h3>
      <FindingList items={by(data.caps, "CAPABILITY")} />
      <h3>User journeys</h3>
      <FindingList items={by(data.caps, "JOURNEY")} />
      <h3>Domain hypotheses</h3>
      <FindingList items={by(data.hyps, "DOMAIN")} />
      <h3>Technology signals (passive)</h3>
      <FindingList items={by(data.caps, "TECHNOLOGY_SIGNAL")} />
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

export function InvestigationView({ client, profile, onStarted, busy }: {
  client: LensAPIClient; profile: WebsiteProfile; busy: boolean;
  onStarted: (p: WebsiteProfile, message: string) => void;
}) {
  const { data, error } = useLoad(() => client.listFindings(profile.id, "hypotheses"), [profile.id, profile.status, profile.run_id]);
  const [startError, setStartError] = useState("");
  const [starting, setStarting] = useState(false);
  const start = async () => {
    setStartError("");
    setStarting(true);
    try {
      const r = await client.startAnalysis(profile.id, `lens-analysis:${profile.id}`);
      onStarted({ ...profile, ...r.website }, `Multi-agent investigation started (run ${r.run.id}, ${r.tasks.length} specialist tasks; the Independent Reviewer runs last). Findings appear here as agents record them.`);
    } catch (e) {
      setStartError((e as Error).message);
    } finally {
      setStarting(false);
    }
  };
  return (
    <div>
      {profile.run_id ? (
        <p className="muted">Multi-agent run {profile.run_id} is attached to this investigation. Agent findings are labelled with the agent that recorded them; reviewer corrections update their confidence.</p>
      ) : (
        <>
          <p className="muted">Runs the specialist agents for this investigation's mode, with an Independent Reviewer last. This creates a workspace project.</p>
          <div className="row"><button className="primary" onClick={start} disabled={busy || starting}>{starting ? "Starting…" : "Start full investigation"}</button></div>
        </>
      )}
      {startError && <p className="error">{startError}</p>}
      {!data ? <Loading error={error} /> : DESIGN_KINDS.map(([kind, label]) => (
        <div key={kind}>
          <h3>{label}</h3>
          <FindingList items={data.items.filter((f) => f.kind === kind)} />
        </div>
      ))}
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
      <p className="muted">Pick up to 4 of your other investigations to compare with {profile.domain}.</p>
      {others.slice(0, 30).map((w) => (
        <label key={w.id} className="item">
          <input type="checkbox" checked={picked.includes(w.id)} disabled={!picked.includes(w.id) && picked.length >= 4}
            onChange={(e) => setPicked(e.target.checked ? [...picked, w.id] : picked.filter((x) => x !== w.id))} />
          <span>{w.domain} <span className="muted">{w.created_at ? new Date(w.created_at).toLocaleDateString() : ""}</span></span>
        </label>
      ))}
      <div className="row"><button className="primary" onClick={compare} disabled={picked.length === 0}>Compare {picked.length + 1} websites</button></div>
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
