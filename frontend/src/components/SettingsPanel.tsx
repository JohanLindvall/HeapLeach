// SPDX-License-Identifier: MIT
import { useEffect, useRef, useState } from 'react';
import { ApiError, fetchSettings, updateSettings, type Settings } from '../api';
import type { ProxyGroup, ProxyProtocol, Snapshot } from '../types';
import { BoltIcon, SplitIcon } from './Icons';
import { ProxyList } from './ProxyList';
import { SettingSlider } from './SettingSlider';
import { SpeedLimit } from './SpeedLimit';

const SETTLE_TIMEOUT_MS = 4000;

const PROTOCOLS: Record<ProxyProtocol, string> = { any: 'Any protocol', socks: 'SOCKS only', http: 'HTTP only' };

const GROUPS: readonly (readonly [ProxyGroup, string])[] = [
  ['keep2share', 'Keep2Share'], ['fileboom', 'FileBoom'], ['cloudflare', 'WAF recovery'],
];

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

  const change = async (patch: Partial<Settings>): Promise<boolean> => {
    setError('');
    try {
      await updateSettings(patch);
      setRevision((value) => value + 1);
      return true;
    } catch (e) {
      setError(e instanceof ApiError ? e.message : 'Could not update settings.');
      return false;
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
          title="Connections a slow file may be split across; K2S and FileBoom use one"
          value={snapshot.streams} max={snapshot.maxStreams}
          onCommit={(streams) => { void change({ streams }); }} />
        <SpeedLimit value={snapshot.speedLimit} onChange={(speedLimit) => { void change({ speedLimit }); }} />
      </div>
      <div className="settings-panel__proxy-head">
        <div>
          <h3>Download proxies <span className="settings-panel__scope">K2S · FileBoom · WAF recovery</span></h3>
          <p>Use “Files at once” across proxy addresses, with one transfer per address for each service.</p>
          <p>Other hosts use proxies after a Cloudflare challenge.</p>
        </div>
        <label className="proxy-switch">
          <input type="checkbox" role="switch" checked={proxies} disabled={switching}
            onChange={() => { void toggle(); }} aria-label="Use download proxies" />
          {switching ? 'Applying…' : proxies ? 'Enabled' : 'Disabled'}
        </label>
      </div>
      <fieldset className="proxy-protocols" aria-describedby="proxy-protocols-help">
        <legend>Proxy protocol</legend>
        <div className="proxy-protocols__choices">
          <ProtocolSelect id="proxy-type" label="All services" value={snapshot.proxyType}
            onCommit={(proxyType) => change({ proxyType: proxyType as ProxyProtocol })} />
          {GROUPS.map(([group, label]) => (
            <ProtocolSelect key={group} id={`proxy-type-${group}`} label={label}
              value={snapshot.proxyTypes[group] ?? ''} follows={snapshot.proxyType}
              onCommit={(kind) => change({ proxyTypes: { [group]: kind as ProxyProtocol | '' } })} />
          ))}
        </div>
        <small id="proxy-protocols-help">SOCKS or HTTP alone leaves your usual connection out. A service with no route of its protocol waits for one.</small>
      </fieldset>
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

/**
 * One protocol choice, shown as made until a snapshot agrees with it, or
 * for a few seconds if none does, as the proxy switch is. A group's own
 * choice offers to follow the protocol the others do, naming it.
 */
function ProtocolSelect({ id, label, value, follows, onCommit }: {
  readonly id: string;
  readonly label: string;
  /** The server's choice; '' for a group following the others. */
  readonly value: string;
  readonly follows?: ProxyProtocol;
  readonly onCommit: (value: string) => Promise<boolean>;
}) {
  const [choice, setChoice] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  useEffect(() => {
    if (choice === null || pending) return;
    if (choice === value) {
      setChoice(null);
      return;
    }
    const timer = window.setTimeout(() => setChoice(null), SETTLE_TIMEOUT_MS);
    return () => window.clearTimeout(timer);
  }, [choice, value, pending]);

  const commit = async (next: string): Promise<void> => {
    setChoice(next);
    setPending(true);
    try {
      if (!await onCommit(next)) setChoice(null);
    } finally {
      setPending(false);
    }
  };

  return (
    <label className="proxy-protocol" htmlFor={id}>
      <span>{label}</span>
      <select id={id} value={choice ?? value} disabled={pending} onChange={(e) => { void commit(e.target.value); }}>
        {follows && <option value="">Same as all · {PROTOCOLS[follows]}</option>}
        {Object.entries(PROTOCOLS).map(([kind, name]) => <option key={kind} value={kind}>{name}</option>)}
      </select>
    </label>
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
