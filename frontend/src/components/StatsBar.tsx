import { formatSpeed } from '../format';
import type { ConnectionState, Snapshot } from '../types';
import { BoltIcon, MoonIcon, PauseIcon, PlayIcon, SplitIcon, SunIcon } from './Icons';
import { SpeedLimit } from './SpeedLimit';
import { Sparkline } from './Sparkline';

interface StatsBarProps {
  readonly snapshot: Snapshot;
  readonly connection: ConnectionState;
  readonly onConcurrencyChange: (value: number) => void;
  readonly onStreamsChange: (value: number) => void;
  readonly speedSeries: number[];
  readonly theme: 'dark' | 'light';
  readonly onToggleTheme: () => void;
  readonly onTogglePause: () => void;
  readonly onSpeedLimitChange: (bytesPerSecond: number) => void;
}

/** Header: live totals, the worker-count control and connection status. */
export function StatsBar({
  snapshot,
  connection,
  onConcurrencyChange,
  onStreamsChange,
  speedSeries,
  theme,
  onToggleTheme,
  onTogglePause,
  onSpeedLimitChange,
}: StatsBarProps) {
  const totals = snapshot.jobs.reduce(
    (acc, job) => {
      acc.done += job.done;
      acc.failed += job.failed;
      acc.total += job.total;
      return acc;
    },
    { done: 0, failed: 0, total: 0 },
  );

  return (
    <div className="stats">
      <div className="stats__group">
        <Stat label="Downloading" value={String(snapshot.active)} accent={snapshot.active > 0} />
        <Stat label="Queued" value={String(snapshot.queued)} />
        <Stat label="Completed" value={`${totals.done}/${totals.total}`} />
        {totals.failed > 0 && <Stat label="Failed" value={String(totals.failed)} danger />}
        <div className="stat stat--graph">
          <span
            className={`stat__value ${snapshot.paused ? 'stat__value--held' : 'stat__value--accent'}`}
          >
            {snapshot.paused ? 'Paused' : formatSpeed(snapshot.speed)}
          </span>
          <Sparkline series={speedSeries} />
        </div>
      </div>

      <div className="stats__controls">
        <label className="concurrency" htmlFor="concurrency" title="Files downloaded at once">
          <BoltIcon />
          <span className="concurrency__label">Files</span>
          <input
            id="concurrency"
            type="range"
            min={1}
            max={snapshot.maxConcurrency}
            value={snapshot.concurrency}
            onChange={(e) => onConcurrencyChange(Number(e.target.value))}
          />
          <output className="concurrency__value">{snapshot.concurrency}</output>
        </label>

        <label
          className="concurrency"
          htmlFor="streams"
          title="Connections a slow file may be split across"
        >
          <SplitIcon />
          <span className="concurrency__label">Streams</span>
          <input
            id="streams"
            type="range"
            min={1}
            max={snapshot.maxStreams}
            value={snapshot.streams}
            onChange={(e) => onStreamsChange(Number(e.target.value))}
          />
          <output className="concurrency__value">{snapshot.streams}</output>
        </label>

        <SpeedLimit value={snapshot.speedLimit} onChange={onSpeedLimitChange} />

        <button
          type="button"
          className={`btn btn--pause${snapshot.paused ? ' is-paused' : ''}`}
          onClick={onTogglePause}
          title={snapshot.paused ? 'Resume the queue' : 'Pause every transfer'}
          aria-pressed={snapshot.paused}
        >
          {snapshot.paused ? <PlayIcon /> : <PauseIcon />}
          <span className="btn__label">{snapshot.paused ? 'Resume' : 'Pause'}</span>
        </button>

        <button
          type="button"
          className="btn btn--icon theme-toggle"
          onClick={onToggleTheme}
          title={theme === 'dark' ? 'Switch to light' : 'Switch to dark'}
          aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
        >
          {theme === 'dark' ? <SunIcon /> : <MoonIcon />}
        </button>

        <span className={`pill pill--${connection}`} title={`Event stream: ${connection}`}>
          <span className="pill__dot" />
          <span className="pill__text">{connection}</span>
        </span>
      </div>
    </div>
  );
}

interface StatProps {
  readonly label: string;
  readonly value: string;
  readonly accent?: boolean;
  readonly danger?: boolean;
}

function Stat({ label, value, accent = false, danger = false }: StatProps) {
  const classes = ['stat'];
  if (accent) classes.push('stat--accent');
  if (danger) classes.push('stat--danger');
  return (
    <div className={classes.join(' ')}>
      <span className="stat__value">{value}</span>
      <span className="stat__label">{label}</span>
    </div>
  );
}
