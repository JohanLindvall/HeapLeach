// SPDX-License-Identifier: MIT
import { useEffect, useState } from 'react';
import { ApiError, fetchProxies, type ProxyQuery } from '../api';
import { formatDuration, formatSpeed } from '../format';
import type { ProxyPage, ProxyRow } from '../types';

const REFRESH_MS = 5000;
const SEARCH_DELAY_MS = 250;

export function ProxyList({ enabled, revision }: { readonly enabled: boolean; readonly revision: number }) {
  const [page, setPage] = useState<ProxyPage | null>(null);
  const [error, setError] = useState('');
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState<ProxyQuery>({ offset: 0, search: '', status: 'all', sort: 'score' });
  useEffect(() => {
    const timer = window.setTimeout(() => setQuery((previous) => previous.search === search ? previous : { ...previous, search, offset: 0 }), SEARCH_DELAY_MS);
    return () => window.clearTimeout(timer);
  }, [search]);

  useEffect(() => {
    const controller = new AbortController();
    let timer: number | undefined;
    let pending = false;
    setPage(null);
    setError('');
    const refresh = async (): Promise<void> => {
      if (pending || controller.signal.aborted) return;
      window.clearTimeout(timer);
      if (!document.hidden) {
        pending = true;
        try {
          const result = await fetchProxies(query, controller.signal);
          if (!controller.signal.aborted) { setPage(result); setError(''); }
        } catch (e) {
          if (!controller.signal.aborted) setError(e instanceof ApiError ? e.message : 'Could not refresh the proxy list.');
        } finally {
          pending = false;
        }
      }
      if (!controller.signal.aborted) timer = window.setTimeout(() => { void refresh(); }, REFRESH_MS);
    };
    const visible = (): void => { if (!document.hidden) void refresh(); };
    void refresh();
    document.addEventListener('visibilitychange', visible);
    return () => {
      controller.abort();
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', visible);
    };
  }, [query, revision, enabled]);

  return (
    <div className="proxy-list" aria-label="K2S proxy inventory">
      {!enabled && <p className="proxy-list__off">Proxy routing is off. Enable it above to discover and use routes for K2S.</p>}
      {page && (
        <div className="proxy-list__summary">
          <span><strong>{page.summary.total.toLocaleString()}</strong> routes</span>
          <span><strong>{page.summary.available.toLocaleString()}</strong> available</span>
          <span><strong>{page.summary.active}</strong> active transfers</span>
          <span><strong>{page.summary.cooling.toLocaleString()}</strong> cooling down</span>
          <span><strong>{page.summary.untested.toLocaleString()}</strong> untested</span>
        </div>
      )}
      <div className="proxy-list__tools">
        <input type="search" value={search} placeholder="Find proxy address" aria-label="Find proxy address"
          onChange={(e) => setSearch(e.target.value)} />
        <select aria-label="Proxy status" value={query.status}
          onChange={(e) => setQuery({ ...query, status: e.target.value, offset: 0 })}>
          <option value="all">All statuses</option>
          <option value="available">Available</option>
          <option value="active">Active</option>
          <option value="busy">Address busy</option>
          <option value="cooling">Cooling down</option>
          <option value="untested">Untested</option>
        </select>
        <select aria-label="Sort proxies" value={query.sort}
          onChange={(e) => setQuery({ ...query, sort: e.target.value, offset: 0 })}>
          <option value="score">Highest score</option>
          <option value="throughput">Highest throughput</option>
          <option value="success">Highest success rate</option>
          <option value="address">Address</option>
        </select>
      </div>
      <p className="proxy-list__help" id="proxy-score-help">Score estimates useful throughput for a 32 MiB transfer, including reliability, measured speed, and setup time. Higher is better; actual downloads also account for remaining file size. Speed estimates update during transfers.</p>
      {error && <p className="settings-error" role="alert">{error} Retrying automatically{page ? '; showing the last update.' : '.'}</p>}
      {!page ? (!error && <p role="status">Loading proxy list…</p>) : page.rows.length === 0 ? (
        <p className="proxy-list__empty">{page.summary.total > 0 ? 'No proxies match these filters.' : 'No routes loaded yet. Configure sources above, then enable K2S proxies.'}</p>
      ) : (
        <>
          <div className="proxy-list__scroll" tabIndex={0} role="region" aria-label="Proxy measurements">
            <table className="proxy-table">
              <thead><tr>
                <th scope="col">Endpoint</th><th scope="col">Status</th>
                <th scope="col" aria-describedby="proxy-score-help">Score</th>
                <th scope="col" title="Smoothed rate from completed transfers; current speed appears while downloading">Throughput</th>
                <th scope="col" title="Recent successful requests, with older outcomes weighted less">Success</th>
                <th scope="col">Requests</th><th scope="col">Last success</th>
              </tr></thead>
              <tbody>{page.rows.map((row) => <ProxyTableRow key={row.id} row={row} />)}</tbody>
            </table>
          </div>
          <div className="proxy-list__pager">
            <span>{(page.offset + 1).toLocaleString()}–{(page.offset + page.rows.length).toLocaleString()} of {page.total.toLocaleString()}</span>
            <button className="btn btn--ghost btn--sm" type="button" disabled={page.offset === 0}
              onClick={() => setQuery({ ...query, offset: Math.max(0, page.offset - page.limit) })}>Previous</button>
            <button className="btn btn--ghost btn--sm" type="button" disabled={page.offset + page.limit >= page.total}
              onClick={() => setQuery({ ...query, offset: page.offset + page.limit })}>Next</button>
          </div>
        </>
      )}
      {page && page.sources.length > 0 && (
        <details className="proxy-list__feeds">
          <summary>Discovery feeds ({page.sources.length}){page.sources.some((s) => s.refreshing) ? ' · refreshing…' : page.sources.some((s) => s.failed) ? ' · fetch failed' : ''}</summary>
          <ul>{page.sources.map((source) => (
            <li key={source.url}>
              <code>{source.url}</code>
              <span>{source.refreshing ? 'Refreshing…' : source.failed ? 'Fetch failed; keeping saved inventory.' : source.fetched ? `${source.count.toLocaleString()} entries · fetched ${date(source.fetched)}` : 'Waiting to fetch.'}</span>
            </li>
          ))}</ul>
        </details>
      )}
      <p className="proxy-list__help">Measurements are for K2S and refresh every 5 seconds while this panel is open.</p>
    </div>
  );
}

const STATUS: Record<ProxyRow['status'], string> = {
  available: 'Available', untested: 'Untested', active: 'Active', busy: 'Address busy', cooling: 'Cooling down', finishing: 'Finishing',
};

function ProxyTableRow({ row }: { readonly row: ProxyRow }) {
  return (
    <tr>
      <th scope="row"><code title={row.url}>{row.source === 'direct' ? 'Direct connection' : row.url}</code><small>{row.source === 'direct' ? 'Usual connection' : row.source === 'manual' ? 'Configured endpoint' : 'Discovered'}</small></th>
      <td><span className={`proxy-status proxy-status--${row.status}`}>{STATUS[row.status]}{row.active > 0 ? ` (${row.active})` : ''}</span>
        {row.cooldownUntil && <small title={date(row.cooldownUntil)}>{formatDuration(Math.max(0, (Date.parse(row.cooldownUntil) - Date.now()) / 1000))} left</small>}</td>
      <td>{formatSpeed(row.score)}</td>
      <td>{formatSpeed(row.throughput)}<small>{row.currentSpeed > 0 ? `${formatSpeed(row.currentSpeed)} now` : row.throughput > 0 ? 'Measured average' : 'No transfer sample'}</small></td>
      <td>{row.requests > 0 ? `${Math.round(row.successRate * 100)}%` : '—'}</td>
      <td>{row.requests.toLocaleString()}</td>
      <td>{row.lastSuccess ? <time dateTime={row.lastSuccess} title={new Date(row.lastSuccess).toLocaleString()}>{date(row.lastSuccess)}</time> : '—'}</td>
    </tr>
  );
}

function date(value: string): string {
  return new Date(value).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}
