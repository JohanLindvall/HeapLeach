import type { JobView, Status } from '../types';

/** The status groups the sidebar filters by. */
export type Filter = 'all' | 'active' | 'done' | 'failed';

interface SidebarProps {
  readonly jobs: JobView[];
  readonly filter: Filter;
  readonly onFilter: (filter: Filter) => void;
  readonly downloadDir: string;
}

/** Which filter a job belongs to. */
export function matchesFilter(job: JobView, filter: Filter): boolean {
  switch (filter) {
    case 'active':
      return job.status === 'running' || job.status === 'queued' || job.status === 'resolving';
    case 'done':
      return job.status === 'done';
    case 'failed':
      return job.status === 'failed' || job.status === 'canceled';
    default:
      return true;
  }
}

const GROUPS: { key: Filter; label: string; dot: Status | 'all' }[] = [
  { key: 'all', label: 'All', dot: 'all' },
  { key: 'active', label: 'Active', dot: 'running' },
  { key: 'done', label: 'Completed', dot: 'done' },
  { key: 'failed', label: 'Stopped', dot: 'failed' },
];

export function Sidebar({ jobs, filter, onFilter, downloadDir }: SidebarProps) {
  return (
    <aside className="sidebar">
      <nav className="sidebar__nav" aria-label="Filter downloads">
        {GROUPS.map((group) => {
          const count = jobs.filter((job) => matchesFilter(job, group.key)).length;
          return (
            <button
              key={group.key}
              type="button"
              className={`filter ${filter === group.key ? 'is-active' : ''}`}
              aria-pressed={filter === group.key}
              onClick={() => onFilter(group.key)}
            >
              <span className={`filter__dot filter__dot--${group.dot}`} />
              <span className="filter__label">{group.label}</span>
              <span className="filter__count">{count}</span>
            </button>
          );
        })}
      </nav>

      <div className="sidebar__section">
        <h2 className="sidebar__heading">Saving to</h2>
        <code className="sidebar__path" title={downloadDir}>
          {downloadDir}
        </code>
      </div>
    </aside>
  );
}
