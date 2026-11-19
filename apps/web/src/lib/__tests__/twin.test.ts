import { describe, expect, it } from 'vitest';
import { assumptionsToForm, blankForm, formToAssumptions, sortChecks, visibleChecks } from '../twin';
import type { TwinCheck } from '../../types/twin';

describe('twin assumptions form', () => {
  it('blank form means nothing is stated', () => {
    const { assumptions, problems } = formToAssumptions(blankForm());
    expect(problems).toEqual([]);
    expect(assumptions).toEqual({ workload: { peak_rps: 0 }, environment: { regions: [] }, failures: [] });
  });

  it('converts percentages and canonicalizes regions like the backend', () => {
    const f = { ...blankForm(), peakRps: '1200', readRatio: '90', regions: 'us-east-1, eu-west-1 ,us-east-1,', defaultReplicas: '2', defaultTimeoutMs: '800' };
    const { assumptions, problems } = formToAssumptions(f);
    expect(problems).toEqual([]);
    expect(assumptions.workload).toEqual({ peak_rps: 1200, read_ratio: 0.9 });
    expect(assumptions.environment).toEqual({ regions: ['eu-west-1', 'us-east-1'], default_replicas: 2, default_timeout_ms: 800 });
  });

  it('round-trips through the saved form', () => {
    const f = { ...blankForm(), peakRps: '50', readRatio: '33.33', cloud: 'AWS', regions: 'eu-west-1', failures: [{ kind: 'NODE_LOSS' as const, target: 'n1', note: 'az-a' }], notes: 'from load test' };
    const back = assumptionsToForm(formToAssumptions(f).assumptions);
    expect(back).toEqual({ ...f, payloadKb: '', growthPct: '', defaultReplicas: '', defaultTimeoutMs: '' });
  });

  it('reports the same problems the backend would reject', () => {
    const f = { ...blankForm(), peakRps: '-3', readRatio: '140', defaultReplicas: '1.5', defaultTimeoutMs: 'abc', failures: [{ kind: 'DEPENDENCY_OUTAGE' as const, target: '', note: '' }] };
    const { problems } = formToAssumptions(f);
    expect(problems).toHaveLength(5);
    expect(problems.join(' ')).toMatch(/needs a target component/);
  });

  it('whole-system failures need no target', () => {
    const f = { ...blankForm(), failures: [{ kind: 'REGION_LOSS' as const, target: '', note: '' }] };
    expect(formToAssumptions(f).problems).toEqual([]);
    expect(formToAssumptions(f).assumptions.failures).toEqual([{ kind: 'REGION_LOSS' }]);
  });
});

describe('twin report helpers', () => {
  const c = (id: string, status: TwinCheck['status'], findings = 0, dimension: TwinCheck['dimension'] = 'STRUCTURE'): TwinCheck =>
    ({ id, dimension, title: id, status, findings: Array.from({ length: findings }, () => ({ severity: 'INFO', message: 'm', subjects: [] })) });

  it('puts failures first and keeps report order within a status', () => {
    expect(sortChecks([c('a', 'PASS'), c('b', 'FAIL'), c('c', 'NOT_APPLICABLE'), c('d', 'WARN'), c('e', 'FAIL')]).map((x) => x.id)).toEqual(['b', 'e', 'd', 'a', 'c']);
  });

  it('problems-only keeps passing checks that carry info findings', () => {
    const checks = [c('a', 'PASS'), c('b', 'PASS', 1), c('d', 'WARN', 1, 'DATA')];
    expect(visibleChecks(checks, '', true).map((x) => x.id)).toEqual(['d', 'b']);
    expect(visibleChecks(checks, 'DATA', false).map((x) => x.id)).toEqual(['d']);
  });
});
