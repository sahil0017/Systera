import { describe, expect, it } from 'vitest';
import {
  ago, blankOption, evidenceCounts, fingerprintLayers, groupFindings, growthLabel, isStale, kindLabel, optionProblems,
  runFinished, scoreSeries, sparkPoints, toneOf,
} from '../research';
import { readLink } from '../session';
import type { TrendScore, WebsiteFinding, WebsiteFingerprint } from '../../types/research';

const score = (at: string, v: number): TrendScore => ({
  id: at, topic_key: 't', computed_at: at, window_days: 30, score: v,
  components: { popularity: 0, growth: 50, significance: 0, reliability: 0, novelty: 0 },
});
const finding = (kind: string, ev: WebsiteFinding['evidence_class'], name = kind): WebsiteFinding => ({
  id: `${kind}${name}`, kind, name, statement: '', evidence_class: ev, confidence: 'MEDIUM', rationale: '', observation_ids: [], source: 'BASELINE',
});

describe('research helpers', () => {
  it('orders score history oldest first', () => {
    expect(scoreSeries([score('2026-03-01T00:00:00Z', 70), score('2026-01-01T00:00:00Z', 40)])).toEqual([40, 70]);
    expect(scoreSeries(null)).toEqual([]);
  });

  it('scales spark points into the box and clamps out-of-range values', () => {
    expect(sparkPoints([0, 100], 100, 20)).toBe('0,20 100,0');
    expect(sparkPoints([150], 10, 10)).toBe('0,0 10,0');
    expect(sparkPoints([-10, 50, 200], 100, 10)).toBe('0,10 50,5 100,0');
  });

  it('reads growth relative to the flat midpoint', () => {
    expect(growthLabel(75).text).toBe('rising');
    expect(growthLabel(50).text).toBe('steady');
    expect(growthLabel(30).text).toBe('cooling');
  });

  it('flags stale knowledge only once its date has passed', () => {
    const now = Date.parse('2026-06-01T00:00:00Z');
    expect(isStale('2026-05-01T00:00:00Z', now)).toBe(true);
    expect(isStale('2026-07-01T00:00:00Z', now)).toBe(false);
    expect(isStale('0001-01-01T00:00:00Z', now)).toBe(false); // Go zero time is "not set"
    expect(isStale(undefined, now)).toBe(false);
  });

  it('maps statuses and bands to tones', () => {
    expect(toneOf('HIGH')).toBe('ok');
    expect(toneOf('published')).toBe('ok');
    expect(toneOf('MEDIUM')).toBe('warn');
    expect(toneOf('REJECTED')).toBe('bad');
    expect(toneOf('whatever')).toBe('muted');
  });

  it('groups findings in reading order with readable labels', () => {
    const g = groupFindings([finding('TECHNOLOGY_SIGNAL', 'OBSERVED'), finding('API', 'INFERRED'), finding('CAPABILITY', 'OBSERVED'), finding('CAPABILITY', 'PROPOSED', 'b')]);
    expect(g.map((x) => x.label)).toEqual(['Capabilities', 'APIs', 'Technology signals']);
    expect(g[0].items).toHaveLength(2);
    expect(kindLabel('NEW_KIND')).toBe('New Kind');
  });

  it('counts evidence classes', () => {
    expect(evidenceCounts([finding('A', 'OBSERVED'), finding('B', 'OBSERVED'), finding('C', 'PROPOSED')])).toEqual({ OBSERVED: 2, INFERRED: 0, PROPOSED: 1 });
  });

  it('lists only populated fingerprint layers, largest first', () => {
    const el = (id: string) => ({ node_id: id, name: id, node_type: 'SERVICE', evidence_class: 'INFERRED' as const, confidence: 'LOW' });
    const fp: WebsiteFingerprint = {
      website_id: 'w', domain: 'd', disclaimer: '', quality_attributes: null,
      layers: { frontend: { layer: 'FRONTEND', elements: [el('a')] }, data: { layer: 'DATA', elements: [el('b'), el('c')] }, infra: { layer: 'INFRA', elements: null } },
    };
    expect(fingerprintLayers(fp).map((l) => [l.label, l.elements.length])).toEqual([['Data', 2], ['Frontend', 1]]);
    expect(fingerprintLayers(null)).toEqual([]);
  });

  it('knows when a run can no longer change', () => {
    expect(runFinished('SUCCEEDED')).toBe(true);
    expect(runFinished('CANCELLED')).toBe(true);
    expect(runFinished('RUNNING')).toBe(false);
    expect(runFinished(undefined)).toBe(false);
  });

  it('blocks a comparison the guardrail would reject', () => {
    const a = blankOption('Kafka');
    expect(optionProblems([a])[0]).toMatch(/at least two/);
    const full = { fit: 'x', security: 'x', maturity: 'x', operational_burden: 'x', cost: 'x' };
    const ok = [{ ...a, rationale: full }, { ...blankOption('NATS'), rationale: full }];
    expect(optionProblems(ok)).toEqual([]);
    const dup = [{ ...a, rationale: full }, { ...blankOption('kafka'), rationale: full }];
    expect(optionProblems(dup).join(' ')).toMatch(/listed twice/);
    const missing = [{ ...a, rationale: { ...full, cost: ' ' } }, ok[1]];
    expect(optionProblems(missing)[0]).toMatch(/Kafka: add a rationale for cost/);
  });

  it('describes elapsed time', () => {
    const now = Date.parse('2026-06-01T12:00:00Z');
    expect(ago('2026-06-01T11:59:30Z', now)).toBe('just now');
    expect(ago('2026-06-01T10:00:00Z', now)).toBe('2 h ago');
    expect(ago(undefined, now)).toBe('—');
  });

  it('reads the new deep-link fields', () => {
    expect(readLink('?tab=research&section=provenance&run=abc&site=s1')).toEqual({ tab: 'research', section: 'provenance', run: 'abc', site: 's1' });
  });
});
