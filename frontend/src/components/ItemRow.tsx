import { formatBytes, formatEta, formatSpeed, percentOf } from '../format';
import type { ItemView } from '../types';
import { CancelIcon, RetryIcon } from './Icons';
import { ProgressBar } from './ProgressBar';

interface ItemRowProps {
  readonly item: ItemView;
  readonly onCancel: () => void;
  readonly onRetry: () => void;
}

/** One file: name, live progress, and the action that fits its state. */
export function ItemRow({ item, onCancel, onRetry }: ItemRowProps) {
  const percent = percentOf(item.downloaded, item.size);
  const running = item.status === 'running';
  const active = running || item.status === 'queued';

  return (
    <li className={`item item--${item.status}`}>
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
