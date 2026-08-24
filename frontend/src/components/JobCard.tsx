import { useRef, useState } from 'react';
import { formatBytes, formatEta, formatSpeed, hostLabel, percentOf } from '../format';
import type { JobView } from '../types';
import { CancelIcon, ChevronIcon, RetryIcon, TrashIcon } from './Icons';
import { ItemRow } from './ItemRow';
import { ProgressBar } from './ProgressBar';
import { useVirtualRows } from '../useVirtualRows';

interface JobCardProps {
  readonly job: JobView;
  readonly onCancel: () => void;
  readonly onRetry: () => void;
  readonly onRemove: () => void;
  readonly onCancelItem: (itemId: string) => void;
  readonly onRetryItem: (itemId: string) => void;
}

/** One submitted link, with its files collapsed behind a disclosure. */
export function JobCard({
  job,
  onCancel,
  onRetry,
  onRemove,
  onCancelItem,
  onRetryItem,
}: JobCardProps) {
  // Multi-file jobs stay collapsed to keep a long queue scannable. Derived
  // until the user chooses, rather than captured at mount: a job is usually
  // mounted while still resolving, when its count is 0 — deciding then
  // would leave every album expanded.
  const [userOpen, setUserOpen] = useState<boolean | null>(null);
  const open = userOpen ?? job.total <= 1;

  // A job of thousands of files is a list only the browser suffers for
  // holding whole, so a long one is rendered a viewport at a time. Closed,
  // it has no rows at all and nothing to window.
  const listRef = useRef<HTMLUListElement>(null);
  const rows = useVirtualRows(listRef, open ? job.items.length : 0);
  const shown = rows ? job.items.slice(rows.start, rows.end) : job.items;

  const percent = job.sizeKnown ? percentOf(job.downloaded, job.size) : null;
  const busy = job.status === 'running' || job.status === 'queued' || job.status === 'resolving';
  const retryable = job.status === 'failed' || job.status === 'canceled';

  return (
    <article className={`card job job--${job.status}`}>
      <header className="job__head">
        <button
          type="button"
          className={`job__toggle ${open ? 'is-open' : ''}`}
          aria-expanded={open}
          onClick={() => setUserOpen(!open)}
          disabled={job.items.length === 0}
        >
          <ChevronIcon />
          <span className="sr-only">{open ? 'Collapse' : 'Expand'} files</span>
        </button>

        <div className="job__title">
          <h2 title={job.title}>{job.title}</h2>
          <div className="job__tags">
            <span className="tag tag--host">{hostLabel(job.host)}</span>
            <span className={`tag tag--${job.status}`}>{job.status}</span>
            {job.total > 0 && (
              <span className="tag">
                {job.done}/{job.total} files
              </span>
            )}
            {job.failed > 0 && <span className="tag tag--failed">{job.failed} failed</span>}
          </div>
        </div>

        <div className="job__actions">
          {busy && (
            <button type="button" className="btn btn--icon" onClick={onCancel} title="Cancel job">
              <CancelIcon />
              <span className="sr-only">Cancel job</span>
            </button>
          )}
          {retryable && (
            <button type="button" className="btn btn--icon" onClick={onRetry} title="Retry job">
              <RetryIcon />
              <span className="sr-only">Retry job</span>
            </button>
          )}
          <button type="button" className="btn btn--icon" onClick={onRemove} title="Remove job">
            <TrashIcon />
            <span className="sr-only">Remove job</span>
          </button>
        </div>
      </header>

      <ProgressBar percent={percent} status={job.status} label={`${job.title} progress`} />

      <div className="job__meta">
        {/* A real link: being able to revisit the page a job came from is
            half the point of keeping the source around. */}
        <a
          className="job__source"
          href={job.source}
          target="_blank"
          rel="noreferrer noopener"
          title={`Open ${job.source}`}
        >
          {job.source}
        </a>
        <span className="job__numbers">
          {formatBytes(job.downloaded)}
          {job.size > 0 && job.sizeKnown && ` / ${formatBytes(job.size)}`}
          {job.speed > 0 && ` · ${formatSpeed(job.speed)}`}
          {job.speed > 0 && job.sizeKnown && job.size > job.downloaded && (
            ` · ${formatEta(job.size - job.downloaded, job.speed)} left`
          )}
        </span>
      </div>

      {job.error && <p className="job__error">{job.error}</p>}

      {job.status === 'resolving' && (
        <p className="job__resolving">Reading the page and collecting files…</p>
      )}

      {open && job.items.length > 0 && (
        <ul className="job__items" ref={listRef}>
          {rows && rows.padTop > 0 && (
            <li className="job__gap" style={{ height: rows.padTop }} aria-hidden="true" />
          )}
          {shown.map((item, index) => (
            <ItemRow
              key={item.id}
              item={item}
              /* A windowed list holds a fraction of its rows, so each one
                 has to say where in the whole it sits. */
              position={rows ? rows.start + index + 1 : undefined}
              total={rows ? job.items.length : undefined}
              onCancel={() => onCancelItem(item.id)}
              onRetry={() => onRetryItem(item.id)}
            />
          ))}
          {rows && rows.padBottom > 0 && (
            <li className="job__gap" style={{ height: rows.padBottom }} aria-hidden="true" />
          )}
        </ul>
      )}
    </article>
  );
}
