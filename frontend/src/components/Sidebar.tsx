import { countByFilter, type Filter } from '../status';
import type { JobView, Status } from '../types';

interface SidebarProps {
  readonly jobs: JobView[];
  readonly filter: Filter;
  readonly onFilter: (filter: Filter) => void;
}

const GROUPS: { key: Filter; label: string; dot: Status | 'all' }[] = [
  { key: 'all', label: 'All', dot: 'all' },
  { key: 'active', label: 'Active', dot: 'running' },
  { key: 'done', label: 'Completed', dot: 'done' },
  { key: 'failed', label: 'Stopped', dot: 'failed' },
];

export function Sidebar({ jobs, filter, onFilter }: SidebarProps) {
  const counts = countByFilter(jobs);
  return (
    <aside className="sidebar">
      <nav className="sidebar__nav" aria-label="Filter downloads">
        {GROUPS.map((group) => (
          <button
            key={group.key}
            type="button"
            className={`filter ${filter === group.key ? 'is-active' : ''}`}
            aria-pressed={filter === group.key}
            onClick={() => onFilter(group.key)}
          >
            <span className={`filter__dot filter__dot--${group.dot}`} />
            <span className="filter__label">{group.label}</span>
            <span className="filter__count">{counts[group.key]}</span>
          </button>
        ))}
      </nav>
    </aside>
  );
}
