import type { Status } from '../types';

interface ProgressBarProps {
  /** Completion 0-100, or null when the total length is unknown. */
  readonly percent: number | null;
  readonly status: Status;
  readonly label: string;
  readonly compact?: boolean;
}

/**
 * A determinate bar when the total is known, and an animated indeterminate
 * one while it is not — a bar pinned at 0% would read as "stuck".
 */
export function ProgressBar({ percent, status, label, compact = false }: ProgressBarProps) {
  const indeterminate = percent === null && status === 'running';
  const width = percent ?? (status === 'done' ? 100 : 0);

  const classes = ['progress', `progress--${status}`];
  if (compact) classes.push('progress--compact');
  if (indeterminate) classes.push('progress--indeterminate');

  return (
    <div
      className={classes.join(' ')}
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      {...(percent === null ? {} : { 'aria-valuenow': Math.round(percent) })}
    >
      <div className="progress__fill" style={{ width: `${width}%` }} />
    </div>
  );
}
