import { formatBytes, formatEta, formatSpeed, percentOf } from '../format';
import { isActive } from '../status';
import type { ItemView } from '../types';
import { CancelIcon, RetryIcon } from './Icons';
import { ProgressBar } from './ProgressBar';

interface ItemRowProps {
  readonly item: ItemView;
  readonly onCancel: () => void;
  readonly onRetry: () => void;
  /**
   * Where this row sits in the whole list, one-based, and how long that list
   * is. Set only when the list is windowed, where the document holds too
   * few rows to say either on its own.
   */
  readonly position?: number;
  readonly total?: number;
}

/** One file: name, live progress, and the action that fits its state. */
export function ItemRow({ item, onCancel, onRetry, position, total }: ItemRowProps) {
  const running = item.status === 'running';
  const active = isActive(item.status);

  // A playlist has no byte total until its last part lands, so parts joined
  // is the only progress it can honestly show.
  const segments = segmentProgress(item);
  const percent = segments
    ? percentOf(segments.done, segments.total)
    : percentOf(item.downloaded, item.size);

  return (
    <li className={`item item--${item.status}`} aria-posinset={position} aria-setsize={total}>
      <div className="item__main">
        <span className="item__name" title={item.path ?? item.name}>
          {item.dir && <span className="item__dir">{item.dir}/</span>}
          {item.name}
        </span>
        <span className="item__status">{statusLabel(item)}</span>
      </div>

      <ProgressBar
        percent={percent}
        status={item.status}
        label={`${item.name} progress`}
        compact
      />

      <div className="item__meta">
        <span>
          {formatBytes(item.downloaded)}
          {item.size > 0 && ` / ${formatBytes(item.size)}`}
        </span>
        {segments && (
          <span className="item__segments" title="Parts joined so far">
            {segments.done}/{segments.total} parts
          </span>
        )}
        {running && <span className="item__speed">{formatSpeed(item.speed)}</span>}
        {running && (item.streams ?? 0) > 1 && (
          <span className="item__streams" title={`Split across ${item.streams} connections`}>
            ×{item.streams}
          </span>
        )}
        {running && item.size > 0 && (
          <span>{formatEta(item.size - item.downloaded, item.speed)} left</span>
        )}
        {percent !== null && <span className="item__pct">{percent.toFixed(0)}%</span>}
      </div>

      {/* A note explains a deliberate wait, so it belongs where a stalled
          download would otherwise look like nothing happening. */}
      {item.note && <p className="item__note">{item.note}</p>}
      {item.error && <p className="item__error">{item.error}</p>}

      <div className="item__actions">
        {active ? (
          <button type="button" className="btn btn--icon" onClick={onCancel} title="Cancel">
            <CancelIcon />
            <span className="sr-only">Cancel {item.name}</span>
          </button>
        ) : (
          item.status !== 'done' && (
            <button type="button" className="btn btn--icon" onClick={onRetry} title="Retry">
              <RetryIcon />
              <span className="sr-only">Retry {item.name}</span>
            </button>
          )
        )}
      </div>
    </li>
  );
}

/** Part counts, when this file arrives as a playlist rather than one body. */
function segmentProgress(item: ItemView): { done: number; total: number } | null {
  const total = item.segmentsTotal ?? 0;
  if (total <= 0) return null;
  return { done: item.segmentsDone ?? 0, total };
}

function statusLabel(item: ItemView): string {
  switch (item.status) {
    case 'queued':
      return 'Waiting';
    case 'running':
      return 'Downloading';
    case 'done':
      // Worth distinguishing: nothing was transferred, the file was
      // already there.
      return item.skipped ? 'Skipped' : 'Done';
    case 'failed':
      return 'Failed';
    case 'canceled':
      return 'Canceled';
    case 'resolving':
      return 'Resolving';
    default:
      return item.status;
  }
}
