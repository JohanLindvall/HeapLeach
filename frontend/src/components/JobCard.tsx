import { memo, useCallback, useRef, useState } from 'react';
import { formatBytes, formatEta, formatSpeed, hostLabel, percentOf } from '../format';
import { isActive, isRetryable } from '../status';
import type { JobView } from '../types';
import { CancelIcon, ChevronIcon, PlayIcon, RetryIcon, TrashIcon } from './Icons';
import { ItemRow } from './ItemRow';
import { ProgressBar } from './ProgressBar';
import { useVirtualRows } from '../useVirtualRows';

interface JobCardProps {
  readonly job: JobView;
  // Every handler is told which job, rather than being bound to it by the
  // caller: that is what lets one stable callback serve every card, and
  // the memo below skip a card whose job did not move.
  readonly onCancel: (jobId: string) => void;
  readonly onRetry: (jobId: string) => void;
  readonly onRemove: (jobId: string) => void;
  readonly onCancelItem: (jobId: string, itemId: string) => void;
  readonly onRetryItem: (jobId: string, itemId: string) => void;
}

/**
 * One submitted link, with its files collapsed behind a disclosure.
 *
 * A frame rebuilds every job object, but a finished job's fields come back
 * equal and its item list is the very array held before (see
 * mergeSnapshot), so comparing field by field is what spares a long queue
 * from rendering every card a second.
 */
export const JobCard = memo(JobCardView, (before, after) => {
  const { job: a, ...restA } = before;
  const { job: b, ...restB } = after;
  return shallowEqual(a, b) && shallowEqual(restA, restB);
});

function shallowEqual(a: object, b: object): boolean {
  if (a === b) return true;
  const keysA = Object.keys(a);
  if (keysA.length !== Object.keys(b).length) return false;
  return keysA.every((key) => Object.is(a[key as keyof typeof a], b[key as keyof typeof b]));
}

function JobCardView({
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
  // Null where the job's rate cannot carry a projection, which is where the
  // "… left" phrase should be absent rather than empty.
  const jobEta = job.sizeKnown ? formatEta(job.size - job.downloaded, job.speed) : null;
  const busy = isActive(job.status);
  const retryable = isRetryable(job.status);

  // Stable across frames, so an unchanged row's memo holds.
  const jobId = job.id;
  const cancelItem = useCallback((itemId: string) => onCancelItem(jobId, itemId), [jobId, onCancelItem]);
  const retryItem = useCallback((itemId: string) => onRetryItem(jobId, itemId), [jobId, onRetryItem]);

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
          <h3 title={job.title}>{job.title}</h3>
          <div className="job__tags">
            <span className="tag tag--host">{hostLabel(job.host)}</span>
            {job.held ? (
              <span className="tag tag--held">held</span>
            ) : (
              <span className={`tag tag--${job.status}`}>{job.status}</span>
            )}
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
            <button type="button" className="btn btn--icon" onClick={() => onCancel(job.id)} title="Cancel job">
              <CancelIcon />
              <span className="sr-only">Cancel job</span>
            </button>
          )}
          {job.held && (
            <button type="button" className="btn btn--icon" onClick={() => onRetry(job.id)} title="Resume job">
              <PlayIcon />
              <span className="sr-only">Resume job</span>
            </button>
          )}
          {retryable && (
            <button type="button" className="btn btn--icon" onClick={() => onRetry(job.id)} title="Retry job">
              <RetryIcon />
              <span className="sr-only">Retry job</span>
            </button>
          )}
          <button type="button" className="btn btn--icon" onClick={() => onRemove(job.id)} title="Remove job">
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
          {jobEta !== null && ` · ${jobEta} left`}
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
              onCancel={cancelItem}
              onRetry={retryItem}
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
