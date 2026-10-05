import { describe, expect, it } from 'vitest';
import { versionBadge } from './version';

describe('versionBadge', () => {
  it('shows nothing before the server has said', () => {
    expect(versionBadge(undefined, undefined)).toBeNull();
  });

  it('shows the server version, current while it is the page’s own', () => {
    expect(versionBadge('v1.2.62', undefined)).toEqual({ label: 'v1.2.62', stale: false });
    expect(versionBadge('v1.2.62', 'v1.2.62')).toEqual({ label: 'v1.2.62', stale: false });
  });

  it('marks a server replaced under an open page as stale', () => {
    expect(versionBadge('v1.2.63', 'v1.2.62')).toEqual({ label: 'v1.2.63', stale: true });
  });
});
