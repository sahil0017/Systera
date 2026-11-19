import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { sparkPoints, toneOf } from '../lib/research';

export function Tone({ value, label }: { value?: string; label?: string }) {
  if (!value) return null;
  return <span className={`tone tone-${toneOf(value)}`}>{label ?? value.replace(/_/g, ' ').toLowerCase()}</span>;
}

export function EvBadge({ value }: { value: string }) {
  return <span className={`badge ev-${value}`}>{value.toLowerCase()}</span>;
}

export function Spark({ values, w = 120, h = 28, label }: { values: number[]; w?: number; h?: number; label?: string }) {
  if (values.length === 0) return <span className="spark-empty">no history</span>;
  return (
    <svg className="spark" width={w} height={h} viewBox={`0 0 ${w} ${h}`} role="img" aria-label={label ?? `Trend over ${values.length} points`}>
      <polyline points={sparkPoints(values, w, h)} fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinejoin="round" />
    </svg>
  );
}

export function Meter({ value, max = 100, label }: { value: number; max?: number; label: string }) {
  const pct = Math.max(0, Math.min(100, (value / max) * 100));
  return (
    <div className="meter" title={`${label}: ${Math.round(value)}`}>
      <span className="meter-label">{label}</span>
      <span className="meter-track"><span className="meter-fill" style={{ width: `${pct}%` }} /></span>
      <span className="meter-val">{Math.round(value)}</span>
    </div>
  );
}

export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <div className="skeleton-list">
      {Array.from({ length: rows }, (_, i) => <div key={i} className={`skeleton-line ${i === 0 ? 'wide' : i === rows - 1 ? 'short' : ''}`} />)}
    </div>
  );
}

/** Friendly text for API errors: 404 on an optional feature means it isn't enabled on this server. */
export function errorText(err: unknown, what = 'this'): string {
  const msg = err instanceof Error ? err.message : String(err);
  if (/HTTP 404/.test(msg)) return `Nothing found for ${what}, or this server doesn't have it enabled.`;
  if (/HTTP 403/.test(msg)) return `Your role can't access ${what}.`;
  if (/HTTP 401/.test(msg)) return 'Your session has expired. Reconnect with a fresh token.';
  return msg;
}

export function ErrorNote({ error, onRetry }: { error: string; onRetry?: () => void }) {
  return (
    <div className="cf-error row-between">
      <span>{error}</span>
      {onRetry && <button type="button" className="micro-btn" onClick={onRetry}>Retry</button>}
    </div>
  );
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty-note">{children}</div>;
}

export interface Async<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
}

/** Loads `fn` whenever `key` changes; stale responses are discarded. */
export function useAsync<T>(fn: (() => Promise<T>) | null, key: string, what?: string): Async<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  useEffect(() => {
    const f = fnRef.current;
    if (!f) { setData(null); setLoading(false); setError(null); return; }
    let live = true;
    setLoading(true);
    setError(null);
    f().then((d) => { if (live) setData(d); })
      .catch((e) => { if (live) { setError(errorText(e, what)); setData(null); } })
      .finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, [key, tick, what]);
  const reload = useCallback(() => setTick((t) => t + 1), []);
  return { data, loading, error, reload };
}
