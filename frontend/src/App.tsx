import { useCallback, useEffect, useRef, useState } from 'react';
import {
  ApiError,
  cancelItem,
  cancelJob,
  clearFinished,
  removeJob,
  retryItem,
  retryJob,
  setConcurrency,
  setPaused,
  setSpeedLimit,
  setStreams,
} from './api';
import { AddForm } from './components/AddForm';
import { JobCard } from './components/JobCard';
import { ProgressPanel } from './components/ProgressPanel';
import { Sidebar, matchesFilter, type Filter } from './components/Sidebar';
import { StatsBar } from './components/StatsBar';
import { DownloadIcon, FolderIcon, TrashIcon } from './components/Icons';
import type { JobView } from './types';
import { useLiveState } from './useLiveState';
import { useProgress } from './useProgress';
import { useSpeedHistory } from './useSpeedHistory';
import { useTheme } from './useTheme';

type NoticeKind = 'info' | 'error' | 'reward';

interface Notice {
  readonly id: number;
  readonly message: string;
  readonly kind: NoticeKind;
}

const NOTICE_TTL_MS = 6000;
const ERROR_NOTICE_TTL_MS = 10000;

/** Case-insensitive match against everything a user knows a job by. */
function matchesQuery(job: JobView, query: string): boolean {
  if (!query) return true;
  const q = query.toLowerCase();
  return (
    job.title.toLowerCase().includes(q) ||
    job.source.toLowerCase().includes(q) ||
    job.host.toLowerCase().includes(q) ||
    job.items.some((item) => item.name.toLowerCase().includes(q))
  );
}

export default function App() {
  const { snapshot, connection } = useLiveState();
  const [notices, setNotices] = useState<Notice[]>([]);
  const [filter, setFilter] = useState<Filter>('all');
  const [query, setQuery] = useState('');
  const searchRef = useRef<HTMLInputElement>(null);
  const nextNoticeId = useRef(0);

  const notify = useCallback((message: string, kind: NoticeKind): void => {
    const id = nextNoticeId.current++;
    setNotices((current) => [...current, { id, message, kind }]);
    // Errors linger: a toast the user did not see in time is a toast that
    // never happened.
    window.setTimeout(() => {
      setNotices((current) => current.filter((n) => n.id !== id));
    }, kind === 'error' ? ERROR_NOTICE_TTL_MS : NOTICE_TTL_MS);
  }, []);

  // "/" focuses the queue search from anywhere that is not already a text
  // field, the way every searchable list on the web works.
  useEffect(() => {
    const onKey = (event: KeyboardEvent): void => {
      if (event.key !== '/' || event.metaKey || event.ctrlKey || event.altKey) return;
      const target = event.target as HTMLElement | null;
      if (target && (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable)) {
        return;
      }
      event.preventDefault();
      searchRef.current?.focus();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const speedSeries = useSpeedHistory(snapshot?.speed ?? 0);
  const { theme, cycle: toggleTheme } = useTheme();
  const progress = useProgress(snapshot, (achievement) => {
    notify(`${achievement.icon}  ${achievement.title} — ${achievement.detail}`, 'reward');
  });

  // Any action failing is worth surfacing; the state itself refreshes from
  // the event stream, so nothing here needs to update local state.
  const run = useCallback(
    (action: () => Promise<unknown>): void => {
      void action().catch((error: unknown) => {
        notify(error instanceof ApiError ? error.message : 'Request failed.', 'error');
      });
    },
    [notify],
  );

  useEffect(() => {
    document.title =
      snapshot && snapshot.active > 0
        ? `↓ ${snapshot.active} downloading — HeapLeach`
        : 'HeapLeach — bulk downloader';
  }, [snapshot]);

  if (!snapshot) {
    return (
      <div className="app app--loading">
        <div className="spinner" role="status" aria-label="Connecting" />
        <p>Connecting to the download service…</p>
      </div>
    );
  }

  // A plain filter, deliberately: this sits after the early return above,
  // so a hook here would be called conditionally and break the hook order.
  const visible = snapshot.jobs.filter(
    (job) => matchesFilter(job, filter) && matchesQuery(job, query),
  );
  const finishedCount = snapshot.jobs.filter(
    (job) => job.status === 'done' || job.status === 'failed' || job.status === 'canceled',
  ).length;

  return (
    <div className="app">
      <header className="header">
        <div className="header__brand">
          <span className="header__mark" aria-hidden="true">
            ↓
          </span>
          <div>
            <h1>HeapLeach</h1>
            <p>Parallel bulk downloader</p>
          </div>
        </div>
        <StatsBar
          snapshot={snapshot}
          connection={connection}
          speedSeries={speedSeries}
          theme={theme}
          onToggleTheme={toggleTheme}
          onConcurrencyChange={(value) => run(() => setConcurrency(value))}
          onStreamsChange={(value) => run(() => setStreams(value))}
          onTogglePause={() => run(() => setPaused(!snapshot.paused))}
          onSpeedLimitChange={(value) => run(() => setSpeedLimit(value))}
        />
      </header>

      <div className="shell">
        <Sidebar
          jobs={snapshot.jobs}
          filter={filter}
          onFilter={setFilter}
          downloadDir={snapshot.downloadDir}
        />

        <main className="main">
        <AddForm onNotice={notify} />

        <ProgressPanel progress={progress} hostCount={snapshot.hostCount} />

        <section className="jobs" aria-label="Downloads">
          <div className="jobs__head">
            <h2>
              Downloads
              {visible.length > 0 && <span className="jobs__count">{visible.length}</span>}
            </h2>
            <div className="jobs__tools">
              {(snapshot.jobs.length > 0 || query) && (
                <input
                  ref={searchRef}
                  type="search"
                  className="jobs__search"
                  placeholder="Search  /"
                  aria-label="Search downloads"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Escape') {
                      setQuery('');
                      e.currentTarget.blur();
                    }
                  }}
                />
              )}
              <span className="jobs__dir" title={`Files are saved to ${snapshot.downloadDir}`}>
                <FolderIcon />
                <code>{snapshot.downloadDir}</code>
              </span>
              {finishedCount > 0 && (
                <button
                  type="button"
                  className="btn btn--ghost btn--sm"
                  onClick={() => run(clearFinished)}
                >
                  <TrashIcon />
                  Clear finished ({finishedCount})
                </button>
              )}
            </div>
          </div>

          {visible.length === 0 ? (
            <div className="empty">
              <span className="empty__icon" aria-hidden="true">
                <DownloadIcon size={26} />
              </span>
              <p className="empty__title">
                {query
                  ? 'No matches'
                  : filter === 'all'
                    ? 'Nothing queued yet'
                    : 'Nothing here'}
              </p>
              <p className="empty__body">
                {query
                  ? `Nothing in the queue matches “${query}”.`
                  : 'Paste one or more links above. Anything without an extractor of its own is treated as a direct file link.'}
              </p>
            </div>
          ) : (
            <div className="jobs__list">
              {visible.map((job) => (
                <JobCard
                  key={job.id}
                  job={job}
                  onCancel={() => run(() => cancelJob(job.id))}
                  onRetry={() => run(() => retryJob(job.id))}
                  onRemove={() => run(() => removeJob(job.id))}
                  onCancelItem={(itemId) => run(() => cancelItem(job.id, itemId))}
                  onRetryItem={(itemId) => run(() => retryItem(job.id, itemId))}
                />
              ))}
            </div>
          )}
        </section>
        </main>
      </div>

      <div className="notices" role="status" aria-live="polite">
        {notices.map((notice) => (
          <div key={notice.id} className={`notice notice--${notice.kind}`}>
            {notice.message}
          </div>
        ))}
      </div>
    </div>
  );
}
