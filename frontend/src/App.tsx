// SPDX-License-Identifier: MIT
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  ApiError,
  cancelItem,
  cancelJob,
  clearFinished,
  removeJob,
  retryItem,
  retryJob,
  updateSettings,
} from './api';
import { AddForm } from './components/AddForm';
import { DownloadDir } from './components/DownloadDir';
import { FileList } from './components/FileList';
import { JobCard } from './components/JobCard';
import { ProgressPanel } from './components/ProgressPanel';
import { Sidebar } from './components/Sidebar';
import { StatsBar } from './components/StatsBar';
import { SettingsPanel } from './components/SettingsPanel';
import { VersionBadge } from './components/VersionBadge';
import { DownloadIcon, TrashIcon } from './components/Icons';
import {
  countPhases,
  filesInPhase,
  isTerminal,
  matchesFileQuery,
  matchesFilter,
  matchesQuery,
  type Filter,
  type Phase,
} from './status';
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

const IDLE_TITLE = 'HeapLeach — bulk downloader';

export default function App() {
  const { snapshot, connection } = useLiveState();
  // The version this page was loaded with: the first one a snapshot
  // reports. See VersionBadge for what a later, different one means.
  const [pageVersion, setPageVersion] = useState<string>();
  const serverVersion = snapshot?.version;
  useEffect(() => {
    if (serverVersion && pageVersion === undefined) setPageVersion(serverVersion);
  }, [serverVersion, pageVersion]);
  const [notices, setNotices] = useState<Notice[]>([]);
  const [filter, setFilter] = useState<Filter>('all');
  // A file phase, when one is chosen, replaces the job cards with one list
  // of files; choosing a job filter goes back to the cards.
  const [phase, setPhase] = useState<Phase | null>(null);
  const chooseFilter = useCallback((next: Filter) => {
    setFilter(next);
    setPhase(null);
  }, []);
  const [query, setQuery] = useState('');
  const [settingsOpen, setSettingsOpen] = useState(false);
  const searchRef = useRef<HTMLInputElement>(null);
  const nextNoticeId = useRef(0);

  const dismiss = useCallback((id: number): void => {
    setNotices((current) => current.filter((n) => n.id !== id));
  }, []);

  const notify = useCallback(
    (message: string, kind: NoticeKind): void => {
      const id = nextNoticeId.current++;
      setNotices((current) => [...current, { id, message, kind }]);
      // Errors linger: a toast the user did not see in time is a toast that
      // never happened.
      window.setTimeout(() => dismiss(id), kind === 'error' ? ERROR_NOTICE_TTL_MS : NOTICE_TTL_MS);
    },
    [dismiss],
  );

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

  // Stable, so a card whose job did not move is not rendered again just
  // because its handlers were rebuilt.
  const onCancelJob = useCallback((jobId: string) => run(() => cancelJob(jobId)), [run]);
  const onRetryJob = useCallback((jobId: string) => run(() => retryJob(jobId)), [run]);
  const onRemoveJob = useCallback((jobId: string) => run(() => removeJob(jobId)), [run]);
  const onCancelItem = useCallback(
    (jobId: string, itemId: string) => run(() => cancelItem(jobId, itemId)),
    [run],
  );
  const onRetryItem = useCallback(
    (jobId: string, itemId: string) => run(() => retryItem(jobId, itemId)),
    [run],
  );
  // A job's retry takes every failed and cancelled file in it, so retrying
  // each job once covers every file in the failed list.
  const onRetryAll = useCallback(
    (jobIds: string[]) => run(() => Promise.all(jobIds.map((id) => retryJob(id)))),
    [run],
  );

  // Recomputed only when the job list itself changes: the merge keeps it the
  // same array while nothing moved. The counts cost a pass and no
  // allocation; a list is built only for the phase on screen.
  const jobs = snapshot?.jobs;
  const phaseCounts = useMemo(() => countPhases(jobs ?? []), [jobs]);
  const phaseEntries = useMemo(
    () => (phase && jobs ? filesInPhase(jobs, phase).filter((entry) => matchesFileQuery(entry, query)) : []),
    [jobs, phase, query],
  );

  // Keyed on the count rather than the snapshot, which arrives twice a
  // second: the title only has to change when the number does.
  const active = snapshot?.active ?? 0;
  useEffect(() => {
    document.title = active > 0 ? `↓ ${active} downloading — HeapLeach` : IDLE_TITLE;
  }, [active]);

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
  const finishedCount = snapshot.jobs.filter((job) => isTerminal(job.status)).length;

  return (
    <div className={connection === 'offline' ? 'app app--polling' : 'app'}>
      <header className="header">
        <div className="header__brand">
          <span className="header__mark" aria-hidden="true">
            ↓
          </span>
          <div>
            <div className="header__title">
              <h1>HeapLeach</h1>
              <VersionBadge version={snapshot.version} pageVersion={pageVersion} />
            </div>
            <p>Parallel bulk downloader</p>
          </div>
        </div>
        <StatsBar
          snapshot={snapshot}
          connection={connection}
          speedSeries={speedSeries}
          theme={theme}
          onToggleTheme={toggleTheme}
          onTogglePause={() => run(() => updateSettings({ paused: !(snapshot.paused || snapshot.held > 0) }))}
          settingsOpen={settingsOpen}
          onToggleSettings={() => setSettingsOpen((open) => !open)}
        />
      </header>

      <div className="shell">
        <Sidebar
          jobs={snapshot.jobs}
          filter={filter}
          onFilter={chooseFilter}
          phase={phase}
          phaseCounts={phaseCounts}
          onPhase={setPhase}
        />

        <main className="main">
          {settingsOpen && <SettingsPanel snapshot={snapshot} />}
          <AddForm onNotice={notify} />

          <ProgressPanel progress={progress} hostCount={snapshot.hostCount} />

          <section className="jobs" aria-label="Downloads">
            <div className="jobs__head">
              <h2>
                {phase ? 'Files' : 'Downloads'}
                {!phase && visible.length > 0 && <span className="jobs__count">{visible.length}</span>}
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
                <DownloadDir
                  value={snapshot.downloadDir}
                  free={snapshot.diskFree}
                  total={snapshot.diskTotal}
                  minFree={snapshot.diskMinFree}
                  onChange={(downloadDir) => run(() => updateSettings({ downloadDir }))}
                />
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

            {phase ? (
              <FileList
                key={phase}
                phase={phase}
                entries={phaseEntries}
                searching={query.trim() !== ''}
                onCancelItem={onCancelItem}
                onRetryItem={onRetryItem}
                onRetryAll={onRetryAll}
              />
            ) : visible.length === 0 ? (
              <div className="empty">
                <span className="empty__icon" aria-hidden="true">
                  <DownloadIcon size={26} />
                </span>
                <p className="empty__title">
                  {query ? 'No matches' : filter === 'all' ? 'Nothing queued yet' : 'Nothing here'}
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
                    onCancel={onCancelJob}
                    onRetry={onRetryJob}
                    onRemove={onRemoveJob}
                    onCancelItem={onCancelItem}
                    onRetryItem={onRetryItem}
                  />
                ))}
              </div>
            )}
          </section>
        </main>
      </div>

      {/* Each notice dismisses itself when clicked, so one that has been
          read need not sit in the corner for the rest of its allotted time. */}
      <div className="notices" role="status" aria-live="polite">
        {notices.map((notice) => (
          <button
            key={notice.id}
            type="button"
            className={`notice notice--${notice.kind}`}
            title="Dismiss"
            onClick={() => dismiss(notice.id)}
          >
            {notice.message}
          </button>
        ))}
      </div>
    </div>
  );
}
