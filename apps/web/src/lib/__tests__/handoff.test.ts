import { describe, expect, it } from 'vitest';
import { askOtherTabsForToken, serveTokenToOtherTabs } from '../session';

const b64 = (o: object) => btoa(JSON.stringify(o)).replace(/=+$/, '').replace(/\+/g, '-').replace(/\//g, '_');
const jwt = (expSecondsFromNow: number) => `${b64({ alg: 'RS256' })}.${b64({ sub: 'a@b.c', exp: Math.floor(Date.now() / 1000) + expSecondsFromNow })}.sig`;

describe('workspace tab handoff', () => {
  it('a new tab receives the token from a connected tab', async () => {
    const good = jwt(3600);
    const stop = serveTokenToOtherTabs(() => good);
    try {
      expect(await askOtherTabsForToken(500)).toBe(good);
    } finally {
      stop();
    }
  });

  it('a tab with an expired token does not answer, so the asker times out', async () => {
    const stop = serveTokenToOtherTabs(() => jwt(-60));
    try {
      expect(await askOtherTabsForToken(150)).toBeNull();
    } finally {
      stop();
    }
  });

  it('a signed-out tab does not answer', async () => {
    const stop = serveTokenToOtherTabs(() => '');
    try {
      expect(await askOtherTabsForToken(150)).toBeNull();
    } finally {
      stop();
    }
  });
});
