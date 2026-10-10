// SPDX-License-Identifier: MIT
import { useEffect, useRef, useState } from 'react';
import { ApiError, fetchSettings, updateSettings, type Settings } from '../api';
import type { Snapshot } from '../types';
import { BoltIcon, SplitIcon } from './Icons';
import { ProxyList } from './ProxyList';
import { SettingSlider } from './SettingSlider';
import { SpeedLimit } from './SpeedLimit';

const SETTLE_TIMEOUT_MS = 4000;

export function SettingsPanel({ snapshot }: { readonly snapshot: Snapshot }) {
  const [error, setError] = useState('');
  const [editing, setEditing] = useState(false);
  const [revision, setRevision] = useState(0);
  const [switching, setSwitching] = useState(false);
  const switchingRef = useRef(false);
  const [proxyChoice, setProxyChoice] = useState<boolean | null>(null);
  const proxies = proxyChoice ?? snapshot.proxies;
  useEffect(() => {
    if (proxyChoice === null || switching) return;
    if (proxyChoice === snapshot.proxies) {
      setProxyChoice(null);
      return;
    }
    // Keep the acknowledged choice visible until SSE catches up, then
    // return to server state. A disconnected tab must not keep an override.
    const timer = window.setTimeout(() => setProxyChoice(null), SETTLE_TIMEOUT_MS);
    return () => window.clearTimeout(timer);
  }, [proxyChoice, snapshot.proxies, switching]);

  const change = async (patch: Partial<Settings>): Promise<void> => {
    setError('');
    try {
      await updateSettings(patch);
      setRevision((value) => value + 1);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'Could not update settings.');
    }
  };

  const toggle = async (): Promise<void> => {
    if (switchingRef.current) return;
    switchingRef.current = true;
    setSwitching(true);
    setProxyChoice(!proxies);
    setError('');
    try {
      await updateSettings({ proxies: !proxies });
      setRevision((value) => value + 1);
    } catch (e) {
      setProxyChoice(null);
      setError(e instanceof ApiError ? e.message : 'Could not update settings.');
    } finally {
      switchingRef.current = false;
      setSwitching(false);
    }
  };

  return (
    <section id="live-settings" className="settings-panel" aria-labelledby="settings-title">
      <div className="settings-panel__head">
        <div>
          <h2 id="settings-title">Live settings</h2>
          <p>Changes apply to this session. Running downloads keep their current route.</p>
        </div>
      </div>
      <div className="settings-panel__transfers">
        <SettingSlider id="concurrency" icon={<BoltIcon />} label="Files at once"
          title="Maximum files downloaded at once, across all hosts"
          value={snapshot.concurrency} max={snapshot.maxConcurrency}
          onCommit={(concurrency) => { void change({ concurrency }); }} />
        <SettingSlider id="streams" icon={<SplitIcon />} label="Streams per file"
          title="Connections a slow file may be split across; K2S uses one"
          value={snapshot.streams} max={snapshot.maxStreams}
          onCommit={(streams) => { void change({ streams }); }} />
        <SpeedLimit value={snapshot.speedLimit} onChange={(speedLimit) => { void change({ speedLimit }); }} />
      </div>
      <div className="settings-panel__proxy-head">
        <div>
          <h3>K2S proxies <span className="settings-panel__scope">K2S only</span></h3>
          <p>Use separate proxy addresses for simultaneous free downloads.</p>
        </div>
        <label className="proxy-switch">
          <input type="checkbox" role="switch" checked={proxies} disabled={switching}
            onChange={() => { void toggle(); }} aria-label="Use proxies for K2S" />
          {switching ? 'Applying…' : proxies ? 'Enabled' : 'Disabled'}
        </label>
      </div>
      {error && <p className="settings-error" role="alert">{error}</p>}
      <button type="button" className="btn btn--ghost btn--sm" aria-expanded={editing}
        aria-controls="proxy-source-editor" onClick={() => setEditing((value) => !value)}>
        {editing ? 'Close source editor' : 'Edit proxy sources'}
      </button>
      {editing && <ProxySources onSaved={() => { setEditing(false); setRevision((value) => value + 1); }} />}
      <ProxyList enabled={snapshot.proxies} revision={revision} />
    </section>
  );
}

/** Drafts are isolated from polling; a failed save leaves the user's text intact. */
function ProxySources({ onSaved }: { readonly onSaved: () => void }) {
  const [draft, setDraft] = useState<{ endpoints: string; feeds: string } | null>(null);
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const pending = useRef(false);
  useEffect(() => {
    const controller = new AbortController();
    void fetchSettings(controller.signal).then((settings) => {
      if (!controller.signal.aborted) {
        setDraft({ endpoints: settings.proxyEndpoints.join('\n'), feeds: settings.proxyFeeds.join('\n') });
      }
    }).catch((e: unknown) => {
      if (!controller.signal.aborted) setError(e instanceof ApiError ? e.message : 'Could not load proxy sources.');
    });
    return () => controller.abort();
  }, []);

  const save = async (): Promise<void> => {
    if (pending.current || !draft) return;
    pending.current = true;
    setSaving(true);
    setError('');
    const lines = (text: string): string[] => text.split('\n').map((line) => line.trim()).filter(Boolean);
    try {
      await updateSettings({ proxyEndpoints: lines(draft.endpoints), proxyFeeds: lines(draft.feeds) });
      onSaved();
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'Could not save proxy sources.');
    } finally {
      pending.current = false;
      setSaving(false);
    }
  };

  return (
    <form id="proxy-source-editor" className="proxy-sources" onSubmit={(e) => { e.preventDefault(); void save(); }}>
      {error && <p className="settings-error" role="alert">{error}</p>}
      {!draft ? <p>{error ? 'Close and reopen the editor to try again.' : 'Loading sources…'}</p> : (
        <fieldset disabled={saving}>
          <div className="proxy-sources__fields">
            <div className="proxy-sources__field">
              <label htmlFor="proxy-endpoints">Proxy endpoints</label>
              <textarea id="proxy-endpoints" value={draft.endpoints} rows={4} spellCheck={false}
                autoCapitalize="off" autoComplete="off" aria-describedby="proxy-endpoints-help"
                onChange={(e) => setDraft({ ...draft, endpoints: e.target.value })} />
              <small id="proxy-endpoints-help">One HTTP(S) or SOCKS5 proxy URL per line. Include <code>direct</code> to allow your usual connection.</small>
            </div>
            <div className="proxy-sources__field">
              <label htmlFor="proxy-feeds">Discovery feeds</label>
              <textarea id="proxy-feeds" value={draft.feeds} rows={4} spellCheck={false}
                autoCapitalize="off" autoComplete="off" aria-describedby="proxy-feeds-help"
                onChange={(e) => setDraft({ ...draft, feeds: e.target.value })} />
              <small id="proxy-feeds-help">One HTTP(S) list URL per line. Leave empty to use only the endpoints you entered.</small>
            </div>
          </div>
          <button type="submit" className="btn btn--primary btn--sm">{saving ? 'Saving…' : 'Save sources'}</button>
        </fieldset>
      )}
    </form>
  );
}
