import { COLLECTOR_VERSION, NormalizedObservation } from "../shared/types";
import { sanitizeText, sanitizeUrl } from "../content/dom-sanitizer";
import { collectHeadings } from "../collector/headings";
import { collectNavigation } from "../collector/navigation";
import { collectForms } from "../collector/forms";
import { collectVisibleFeatures } from "../collector/visible-features";
import { collectVisibleText } from "../collector/visible-text";
import { collectPublicMetadata } from "../collector/metadata";
import { collectResourceMetadata } from "../collector/resource-metadata";

/** Builds the observation for one page. Nothing is sent here: the side panel previews it first. */
export function buildNormalizedObservation(doc: Document, pageUrl: string, perf?: Performance): NormalizedObservation {
  const redactions = new Set<string>();
  const url = sanitizeUrl(pageUrl, redactions);
  const nav = collectNavigation(doc, url, redactions);
  return {
    schema_version: "1.0",
    collector_version: COLLECTOR_VERSION,
    url,
    title: sanitizeText(doc.title ?? "", redactions),
    user_initiated: true,
    observations: {
      headings: collectHeadings(doc, redactions),
      navigation: nav.labels,
      visible_features: collectVisibleFeatures(doc, redactions),
      visible_text: collectVisibleText(doc, redactions),
      forms: collectForms(doc, redactions),
      public_routes: nav.routes,
      public_metadata: collectPublicMetadata(doc, redactions),
      resource_metadata: collectResourceMetadata(perf, redactions),
    },
    redactions_applied: Array.from(redactions).sort(),
  };
}
