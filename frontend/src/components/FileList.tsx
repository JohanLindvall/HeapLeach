// SPDX-License-Identifier: MIT
import { useCallback, useMemo, useRef, type ReactNode } from 'react';
import { formatBytes, formatSpeed } from '../format';
import { PHASES, type FileEntry, type Phase } from '../status';
import { useVirtualRows } from '../useVirtualRows';
import { AlertIcon, CheckIcon, ClockIcon, DownloadIcon, RetryIcon } from './Icons';
import { ItemRow } from './ItemRow';

interface FileListProps {
  readonly phase: Phase;
  /** The files in this phase, already narrowed by the search. */
  readonly entries: readonly FileEntry[];
  /** True while a search is narrowing the list. */
  readonly searching: boolean;
  readonly onCancelItem: (jobId: string, itemId: string) => void;
  readonly onRetryItem: (jobId: string, itemId: string) => void;
  /** Retries every job holding a failed or cancelled file. */
  readonly onRetryAll: (jobIds: string[]) => void;
}

const ICONS: Record<Phase, ReactNode> = {
  running: <DownloadIcon size={18} />,
  queued: <ClockIcon size={18} />,
  failed: <AlertIcon size={18} />,
  done: <CheckIcon size={18} />,
};

const EMPTY: Record<Phase, { title: string; body: string }> = {
  running: { title: 'Nothing downloading', body: 'Files appear here while their bytes are moving.' },
  queued: { title: 'Nothing waiting', body: 'Files waiting for a free slot show up here.' },
  failed: { title: 'No failures', body: 'Everything that was tried either finished or is still going.' },
  done: { title: 'Nothing finished yet', body: 'Completed files collect here, whichever job they came from.' },
};

/**
 * Every file in one phase, across all jobs, as one list.
 *
 * The rows are the job cards' own, with the job each came from beside the
 * name, so a file's actions work the same here as there. A phase can hold
 * thousands of files — every finished one of a long queue — so the list is
 * windowed the way a long job's is.
 */
export function FileList({
  phase,
  entries,
  searching,
  onCancelItem,
  onRetryItem,
  onRetryAll,
}: FileListProps) {
  const listRef = useRef<HTMLUListElement>(null);
  const rows = useVirtualRows(listRef, entries.length);
  const shown = rows ? entries.slice(rows.start, rows.end) : entries;

  // A row's actions carry only its own id, so one stable callback can serve
  // every row and a row that did not change is not rendered again; which
  // job the id belongs to is looked up here.
  const owner = useRef(new Map<string, string>());
  owner.current = useMemo(() => new Map(entries.map((e) => [e.item.id, e.job.id])), [entries]);
  const cancel = useCallback(
    (itemId: string) => {
      const jobId = owner.current.get(itemId);
      if (jobId) onCancelItem(jobId, itemId);
    },
    [onCancelItem],
  );
  const retry = useCallback(
    (itemId: string) => {
      const jobId = owner.current.get(itemId);
      if (jobId) onRetryItem(jobId, itemId);
    },
    [onRetryItem],
  );

  const label = PHASES.find((p) => p.key === phase)?.label ?? phase;
  const summary = summarise(phase, entries);
  const failedJobs = phase === 'failed' ? [...new Set(entries.map((e) => e.job.id))] : [];

  return (
    <section className={`card files files--${phase}`} aria-label={`${label} files`}>
      <header className="files__head">
        <span className="files__icon" aria-hidden="true">
          {ICONS[phase]}
        </span>
        <div className="files__title">
          <h3>{label}</h3>
          <p className="files__summary">{summary}</p>
        </div>
        {failedJobs.length > 0 && (
          <button
            type="button"
            className="btn btn--ghost btn--sm"
            onClick={() => onRetryAll(failedJobs)}
            title={`Retry the failed files of ${failedJobs.length} ${failedJobs.length === 1 ? 'job' : 'jobs'}`}
          >
            <RetryIcon />
            Retry all
          </button>
        )}
      </header>

      {entries.length === 0 ? (
        <div className="files__empty">
          <p className="empty__title">{searching ? 'No matches' : EMPTY[phase].title}</p>
          <p className="empty__body">
            {searching ? 'No file in this list matches the search.' : EMPTY[phase].body}
          </p>
        </div>
      ) : (
        <ul className="files__items job__items" ref={listRef}>
          {rows && rows.padTop > 0 && (
            <li className="job__gap" style={{ height: rows.padTop }} aria-hidden="true" />
          )}
          {shown.map(({ item, job }, index) => (
            <ItemRow
              key={item.id}
              item={item}
              context={job.title}
              position={rows ? rows.start + index + 1 : undefined}
              total={rows ? entries.length : undefined}
              onCancel={cancel}
              onRetry={retry}
            />
          ))}
          {rows && rows.padBottom > 0 && (
            <li className="job__gap" style={{ height: rows.padBottom }} aria-hidden="true" />
          )}
        </ul>
      )}
    </section>
  );
}

/** The phase's totals, in the words that matter for it. */
function summarise(phase: Phase, entries: readonly FileEntry[]): string {
  const files = `${entries.length.toLocaleString()} ${entries.length === 1 ? 'file' : 'files'}`;
  const jobs = new Set(entries.map((e) => e.job.id)).size;
  const across = jobs > 1 ? ` from ${jobs} jobs` : '';
  let size = 0;
  let unknown = 0;
  let speed = 0;
  let skipped = 0;
  for (const { item } of entries) {
    if (item.size > 0) size += item.size;
    else unknown += 1;
    speed += item.speed;
    if (item.skipped) skipped += 1;
  }
  const bytes = size > 0 ? ` · ${formatBytes(size)}${unknown > 0 ? '+' : ''}` : '';
  switch (phase) {
    case 'running':
      return `${files}${across}${speed > 0 ? ` · ${formatSpeed(speed)}` : ''}`;
    case 'done':
      return `${files}${across}${bytes}${skipped > 0 ? ` · ${skipped.toLocaleString()} already on disk` : ''}`;
    default:
      return `${files}${across}${bytes}`;
  }
}
