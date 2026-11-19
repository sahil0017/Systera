import { describe, expect, it } from 'vitest';
import { conflictProblems, defaultCanonicalKey, ingestProblems, memoryProblems, splitRefs, utf8Bytes, MAX_MEMORY_SUMMARY_BYTES } from '../research';

const A = '11111111-2222-3333-4444-555555555555';
const B = '99999999-2222-3333-4444-555555555555';

describe('knowledge ingest form', () => {
  it('needs a title and content, and a well-formed URL when given', () => {
    expect(ingestProblems({ title: ' ', raw_content: '' })).toEqual(['Add a title.', 'Paste the document content.']);
    expect(ingestProblems({ title: 'T', raw_content: 'x', source_url: 'ftp://x' })[0]).toMatch(/http/);
    expect(ingestProblems({ title: 'T', raw_content: 'x', source_url: 'https://a.b/c' })).toEqual([]);
  });

  it('derives the same key the server would', () => {
    expect(defaultCanonicalKey('  Postgres Partitioning Guide ')).toBe('postgres-partitioning-guide');
  });
});

describe('conflict form', () => {
  it('requires a different, valid document and a note', () => {
    expect(conflictProblems(A, 'nope', 'n')[0]).toMatch(/Choose/);
    expect(conflictProblems(A, A.toUpperCase(), 'n')[0]).toMatch(/itself/);
    expect(conflictProblems(A, B, ' ')).toEqual(['Explain what the two documents disagree on.']);
    expect(conflictProblems(A, B, 'retry windows differ')).toEqual([]);
  });
});

describe('memory form', () => {
  const ok = { summary: 'Chose Postgres for joins.', memory_type: 'PROJECT' };

  it('accepts a distilled summary', () => {
    expect(memoryProblems(ok)).toEqual([]);
  });

  it('counts bytes like the server, so multi-byte text hits the limit sooner', () => {
    expect(utf8Bytes('é')).toBe(2);
    const justOver = 'é'.repeat(MAX_MEMORY_SUMMARY_BYTES / 2 + 1); // 2002 chars, 4002 bytes
    expect(justOver.length).toBeLessThan(MAX_MEMORY_SUMMARY_BYTES);
    expect(memoryProblems({ ...ok, summary: justOver })[0]).toMatch(/never full transcripts/);
  });

  it('checks run id, relevance and expiry', () => {
    expect(memoryProblems({ ...ok, run_id: 'abc' })).toContain('Run id must be a UUID.');
    expect(memoryProblems({ ...ok, run_id: A })).toEqual([]);
    expect(memoryProblems({ ...ok, relevance: 1.5 })).toContain('Relevance must be between 0 and 1.');
    expect(memoryProblems({ ...ok, memory_type: 'WORKING' })).toContain('Working memory should expire; pick an expiry.');
    expect(memoryProblems({ ...ok, expires_at: '2000-01-01' })).toContain('Expiry must be in the future.');
  });

  it('splits and dedupes source references', () => {
    expect(splitRefs('ADR-12, https://x.y\nADR-12,, ')).toEqual(['ADR-12', 'https://x.y']);
  });
});
