import { afterEach, describe, expect, it, vi } from 'vitest';
import { addUrls, ApiError, fetchState } from './api';

afterEach(() => vi.unstubAllGlobals());

describe('API responses', () => {
  it('preserves the reasons when every submitted URL is rejected', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({
      accepted: [],
      rejected: [{ url: 'ftp://example.test/file', error: 'unsupported scheme' }],
    }), { status: 400 })));
    await expect(addUrls('ftp://example.test/file', '')).rejects.toMatchObject({
      name: 'ApiError', status: 400, message: 'ftp://example.test/file: unsupported scheme',
    });
  });

  it('rejects a successful HTML response instead of returning a null snapshot', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('<html>proxy error</html>')));
    await expect(fetchState()).rejects.toBeInstanceOf(ApiError);
  });

  it('passes cancellation through to fetch', async () => {
    const fetch = vi.fn().mockResolvedValue(new Response('{"jobs":[]}'));
    vi.stubGlobal('fetch', fetch);
    const controller = new AbortController();
    await fetchState(controller.signal);
    expect(fetch).toHaveBeenCalledWith('/api/state', expect.objectContaining({ signal: controller.signal }));
  });
});
