import { ACHIEVEMENTS, standing, type Progress } from '../gamification';
import { formatBytes, formatSpeed } from '../format';

interface ProgressPanelProps {
  readonly progress: Progress;
  readonly hostCount: number;
}

/**
 * A light progression strip: rank, a bar toward the next one, session
 * totals, and the badges earned so far. Deliberately one row so it informs
 * without competing with the queue below it.
 */
export function ProgressPanel({ progress, hostCount }: ProgressPanelProps) {
  const rank = standing(progress.bytesDownloaded);
  const earned = new Set(progress.unlocked);

  return (
    <section className="card progress-panel" aria-label="Session progress">
      <div className="rank">
        <span className="rank__badge" aria-hidden="true">
          {rank.level.index}
        </span>
        <div className="rank__body">
          <div className="rank__line">
            <span className="rank__name">{rank.level.name}</span>
            <span className="rank__next">
              {rank.next
                ? `${formatBytes(rank.toNext)} to ${rank.next.name}`
                : 'Max rank'}
            </span>
          </div>
          <div
            className="rank__track"
            role="progressbar"
            aria-label="Progress to next rank"
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={Math.round(rank.fraction * 100)}
          >
            <div className="rank__fill" style={{ width: `${rank.fraction * 100}%` }} />
          </div>
        </div>
      </div>

      <dl className="session">
        <div className="session__stat">
          <dt>Files</dt>
          <dd>{progress.filesCompleted}</dd>
        </div>
        <div className="session__stat">
          <dt>Total</dt>
          <dd>{formatBytes(progress.bytesDownloaded)}</dd>
        </div>
        <div className="session__stat">
          <dt>Peak</dt>
          <dd>{formatSpeed(progress.peakSpeed)}</dd>
        </div>
      </dl>

      <ul className="badges">
        {ACHIEVEMENTS.map((achievement) => {
          const has = earned.has(achievement.id);
          const detail =
            achievement.id === 'globetrotter'
              ? `${achievement.detail} (${progress.hostsUsed.length}/${hostCount})`
              : achievement.detail;
          return (
            <li
              key={achievement.id}
              className={`badge ${has ? 'badge--earned' : 'badge--locked'}`}
              title={`${achievement.title} — ${detail}`}
            >
              <span aria-hidden="true">{achievement.icon}</span>
              <span className="sr-only">
                {achievement.title}: {detail}. {has ? 'Unlocked' : 'Locked'}
              </span>
            </li>
          );
        })}
      </ul>
    </section>
  );
}
