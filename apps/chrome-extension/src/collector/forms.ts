import { FormObservation } from "../shared/types";
import { isExcluded, sanitizeText, visibleText } from "../content/dom-sanitizer";

const SENSITIVE_FIELD = /pass|pwd|card|cc-|cvc|cvv|iban|ssn|tax|otp|token|secret|pin\b/i;

/**
 * Form structure only: visible labels, placeholders and the action path. Field values are
 * never read; password, hidden, file and payment fields are skipped entirely.
 */
export function collectForms(doc: Document, redactions: Set<string>): FormObservation[] {
  const out: FormObservation[] = [];
  for (const form of Array.from(doc.querySelectorAll("form"))) {
    if (isExcluded(form)) continue;
    const labels: string[] = [];
    const add = (t: string) => {
      if (t && !labels.includes(t)) labels.push(t);
    };
    for (const label of Array.from(form.querySelectorAll("label, legend"))) add(visibleText(label, redactions, 80));
    for (const field of Array.from(form.querySelectorAll("input, select, textarea"))) {
      const type = (field.getAttribute("type") ?? "text").toLowerCase();
      const ident = `${field.getAttribute("name") ?? ""} ${field.getAttribute("autocomplete") ?? ""} ${field.id}`;
      if (type === "password" || type === "hidden" || type === "file" || SENSITIVE_FIELD.test(ident)) {
        redactions.add("sensitive_form_field_skipped");
        continue;
      }
      add(sanitizeText(field.getAttribute("placeholder") ?? field.getAttribute("aria-label") ?? "", redactions, 80));
    }
    let action = "";
    try {
      const raw = form.getAttribute("action") ?? "";
      if (raw) {
        const u = new URL(raw, doc.baseURI || "https://invalid.invalid/");
        action = u.pathname;
        if (u.search) redactions.add("form_action_query_stripped");
      }
    } catch {
      action = "";
    }
    out.push({ action: action || undefined, method: (form.getAttribute("method") ?? "GET").toUpperCase(), labels: labels.slice(0, 30) });
    if (out.length >= 20) break;
  }
  return out;
}
