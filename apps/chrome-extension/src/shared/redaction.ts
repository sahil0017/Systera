import { NormalizedObservation } from "./types";

/** One previewable item; unchecking it in the preview removes it before anything is sent. */
export interface PreviewItem {
  id: string;
  section: string;
  text: string;
}

const LIST_SECTIONS = ["headings", "navigation", "visible_features", "visible_text", "public_routes"] as const;

export function previewItems(obs: NormalizedObservation): PreviewItem[] {
  const out: PreviewItem[] = [{ id: "title", section: "title", text: obs.title }];
  const o = obs.observations;
  for (const s of LIST_SECTIONS) o[s].forEach((t, i) => out.push({ id: `${s}.${i}`, section: s, text: t }));
  o.forms.forEach((f, fi) => {
    out.push({ id: `forms.${fi}`, section: "forms", text: `${f.method ?? "GET"} ${f.action ?? "(no action)"}` });
    f.labels.forEach((l, li) => out.push({ id: `forms.${fi}.labels.${li}`, section: "form labels", text: l }));
  });
  for (const [k, v] of Object.entries(o.public_metadata)) out.push({ id: `public_metadata.${k}`, section: "public metadata", text: `${k}: ${v}` });
  o.resource_metadata.forEach((r, i) => out.push({ id: `resource_metadata.${i}`, section: "resource timing", text: r.name }));
  return out;
}

/** Returns a copy of obs without the removed items; records that the user redacted. */
export function applyRedactions(obs: NormalizedObservation, removed: ReadonlySet<string>): NormalizedObservation {
  if (removed.size === 0) return obs;
  const keep = (prefix: string) => (_: unknown, i: number) => !removed.has(`${prefix}.${i}`);
  const o = obs.observations;
  const lists = Object.fromEntries(LIST_SECTIONS.map((s) => [s, (o[s] as string[]).filter(keep(s))]));
  return {
    ...obs,
    title: removed.has("title") ? "" : obs.title,
    observations: {
      ...o,
      ...(lists as Pick<typeof o, (typeof LIST_SECTIONS)[number]>),
      forms: o.forms
        .map((f, fi) => ({ ...f, labels: f.labels.filter(keep(`forms.${fi}.labels`)) }))
        .filter((_, fi) => !removed.has(`forms.${fi}`)),
      public_metadata: Object.fromEntries(Object.entries(o.public_metadata).filter(([k]) => !removed.has(`public_metadata.${k}`))),
      resource_metadata: o.resource_metadata.filter(keep("resource_metadata")),
    },
    redactions_applied: Array.from(new Set([...obs.redactions_applied, "user_redacted_items"])).sort(),
  };
}

/** Byte size of what would be sent (shown in the preview; the server caps at 256 KiB). */
export function payloadBytes(obs: NormalizedObservation): number {
  return new TextEncoder().encode(JSON.stringify(obs)).length;
}
