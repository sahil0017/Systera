import { ResourceMetadata } from "../shared/types";
import { resourceUrl } from "../content/dom-sanitizer";

/** Limited, passive resource timing: origin + path, initiator, duration. Never bodies or headers. */
export function collectResourceMetadata(perf: Performance | undefined, redactions: Set<string>): ResourceMetadata[] {
  if (!perf?.getEntriesByType) return [];
  const out: ResourceMetadata[] = [];
  for (const e of perf.getEntriesByType("resource") as PerformanceResourceTiming[]) {
    const name = resourceUrl(e.name, redactions);
    if (!name || out.some((r) => r.name === name)) continue;
    out.push({ name, initiator_type: e.initiatorType, duration_ms: Math.round(e.duration) });
    if (out.length >= 40) break;
  }
  return out;
}
