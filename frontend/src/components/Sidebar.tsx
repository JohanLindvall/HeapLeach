import { countByFilter, PHASES, type Filter, type Phase } from '../status';
import type { JobView, Status } from '../types';

interface SidebarProps {
  readonly jobs: JobView[];
  /** The job filter in force, shown as chosen only while no phase is. */
  readonly filter: Filter;
  readonly onFilter: (filter: Filter) => void;
  /** The file phase in force, or null for the job view. */
  readonly phase: Phase | null;
  readonly phaseCounts: Record<Phase, number>;
  readonly onPhase: (phase: Phase) => void;
}

const GROUPS: { key: Filter; label: string; dot: Status | 'all' }[] = [
  { key: 'all', label: 'All', dot: 'all' },
  { key: 'active', label: 'Active', dot: 'running' },
  { key: 'done', label: 'Completed', dot: 'done' },
  { key: 'failed', label: 'Stopped', dot: 'failed' },
];

export function Sidebar({ jobs, filter, onFilter, phase, phaseCounts, onPhase }: SidebarProps) {
  const counts = countByFilter(jobs);
  return (
    <aside className="sidebar">
      {/* The header counts files and these count jobs; unlabelled, "4
          downloading" beside "2 active" read as figures that disagree. */}
      <div className="sidebar__section">
        <h2 className="sidebar__title" id="sidebar-jobs">
          Jobs
        </h2>
        <nav className="sidebar__nav" aria-labelledby="sidebar-jobs">
          {GROUPS.map((group) => {
            const chosen = phase === null && filter === group.key;
            return (
              <button
                key={group.key}
                type="button"
                className={`filter ${chosen ? 'is-active' : ''}`}
                aria-pressed={chosen}
                onClick={() => onFilter(group.key)}
              >
                <span className={`filter__dot filter__dot--${group.dot}`} />
                <span className="filter__label">{group.label}</span>
                <span className="filter__count">{counts[group.key]}</span>
              </button>
            );
          })}
        </nav>
      </div>

      {/* Every file in one phase, whichever job it belongs to. */}
      <div className="sidebar__section">
        <h2 className="sidebar__title" id="sidebar-files">
          Files
        </h2>
        <nav className="sidebar__nav" aria-labelledby="sidebar-files">
          {PHASES.map(({ key, label }) => {
            const chosen = phase === key;
            const count = phaseCounts[key];
            return (
              <button
                key={key}
                type="button"
                className={`filter filter--phase filter--${key} ${chosen ? 'is-active' : ''}`}
                aria-pressed={chosen}
                onClick={() => onPhase(key)}
              >
                <span
                  className={`filter__dot filter__dot--${key}${key === 'running' && count > 0 ? ' is-live' : ''}`}
                />
                <span className="filter__label">{label}</span>
                <span className={`filter__count${count > 0 ? ' has-some' : ''}`}>{count}</span>
              </button>
            );
          })}
        </nav>
      </div>
    </aside>
  );
}
