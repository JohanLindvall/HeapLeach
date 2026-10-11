// SPDX-License-Identifier: MIT
import { useLayoutEffect, useMemo, useRef, useState } from 'react';
import { formatLog, logSeverity, matchesLog } from '../logs';
import { useLogs } from '../useLogs';
import { DownloadIcon, PauseIcon, PlayIcon } from './Icons';

const timeFormat = new Intl.DateTimeFormat(undefined, {
  hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23', fractionalSecondDigits: 3,
});

export function LogPanel() {
  const [paused, setPaused] = useState(false);
  const [following, setFollowing] = useState(true);
  const [query, setQuery] = useState('');
  const [minimum, setMinimum] = useState(-100);
  const { page, error } = useLogs(paused);
  const viewport = useRef<HTMLDivElement>(null);
  const entries = useMemo(() => (page?.entries ?? []).filter((entry) => matchesLog(entry, query, minimum)),
    [page, query, minimum]);

  useLayoutEffect(() => {
    if (following && viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight;
  }, [entries, following]);

  const download = (): void => {
    const blob = new Blob([entries.map(formatLog).join('\n') + '\n'], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = 'heapleach.log';
    link.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 0);
  };

  return (
    <section id="service-logs" className="log-panel" aria-labelledby="service-logs-title">
      <div className="log-panel__head">
        <div>
          <h2 id="service-logs-title">Service logs</h2>
          <p>Recent messages from this service run, including download and connection diagnostics.</p>
        </div>
        <span className={`log-panel__status${error ? ' log-panel__status--error' : ''}`} role="status">
          {paused ? 'Updates paused' : error ? 'Reconnecting' : page ? 'Live' : 'Connecting'}
        </span>
      </div>
      <div className="log-panel__tools">
        <input type="search" aria-label="Search service logs" placeholder="Search messages, jobs or hosts"
          value={query} onChange={(event) => setQuery(event.target.value)} />
        <select aria-label="Log level" value={minimum} onChange={(event) => setMinimum(Number(event.target.value))}>
          <option value={-100}>All levels</option>
          <option value={0}>Info and above</option>
          <option value={4}>Warnings and errors</option>
          <option value={8}>Errors only</option>
        </select>
        <button type="button" className="btn btn--sm" aria-pressed={paused}
          onClick={() => setPaused((current) => !current)}>
          {paused ? <PlayIcon /> : <PauseIcon />}{paused ? 'Resume updates' : 'Pause updates'}
        </button>
        <button type="button" className="btn btn--sm" onClick={download} disabled={entries.length === 0}
          title="Download the messages matching the current filters">
          <DownloadIcon />Download logs
        </button>
      </div>
      {error && <p className="log-panel__error" role="alert">{error} {paused ? 'Resume updates to retry.' : 'Retrying automatically.'}</p>}
      <div ref={viewport} className="log-panel__viewport" role="log" aria-label="Service log messages" aria-live="off" tabIndex={0}
        onScroll={(event) => {
          const view = event.currentTarget;
          setFollowing(view.scrollHeight - view.clientHeight - view.scrollTop < 24);
        }}>
        {entries.map((entry) => (
          <div key={`${page!.session}:${entry.id}`} className={`log-row${logSeverity(entry.level) >= 8 ? ' log-row--error' : logSeverity(entry.level) >= 4 ? ' log-row--warn' : ''}`}>
            <time className="log-row__time" dateTime={entry.time || undefined} title={entry.time}>
              {entry.time ? timeFormat.format(new Date(entry.time)) : '—'}
            </time>
            <span className="log-row__level">{entry.level}</span>
            <span className="log-row__text">{entry.text}{entry.truncated && ' … [truncated]'}</span>
          </div>
        ))}
        {entries.length === 0 && <p className="log-panel__empty">
          {!page ? 'Waiting for service logs…' : page.entries.length === 0 ? 'No messages yet.' : 'No messages match these filters.'}
        </p>}
      </div>
      <div className="log-panel__foot">
        <span>{entries.length.toLocaleString()} of {(page?.entries.length ?? 0).toLocaleString()} recent messages · Older messages roll off; restarting clears history.</span>
        {!following && <button type="button" className="btn btn--ghost btn--sm" onClick={() => setFollowing(true)}>Jump to latest</button>}
      </div>
    </section>
  );
}
