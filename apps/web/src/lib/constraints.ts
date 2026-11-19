import type { ConstraintId } from '../types/xray';

export const ALL_CONSTRAINTS: Array<{ id: ConstraintId; label: string }> = [
  { id: 'scale', label: 'Scale' },
  { id: 'availability', label: 'Availability' },
  { id: 'latency', label: 'Latency' },
  { id: 'cost', label: 'Cost' },
  { id: 'team_size', label: 'Team Size' },
  { id: 'compliance', label: 'Compliance' },
  { id: 'operational_maturity', label: 'Operational Maturity' },
  { id: 'migration_complexity', label: 'Migration Complexity' },
];

export const DEFAULT_CONSTRAINT_WEIGHTS: Record<ConstraintId, number> = Object.fromEntries(
  ALL_CONSTRAINTS.map((c) => [c.id, 1.0]),
) as Record<ConstraintId, number>;
