/** Human-readable byte count, e.g. "1.4 GB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '—';
  if (bytes < 1000) return `${bytes} B`;

  const units = ['kB', 'MB', 'GB', 'TB', 'PB'];
  let value = bytes / 1000;
  let unit = 0;
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000;
    unit += 1;
  }
  return `${value.toFixed(value < 10 ? 1 : 0)} ${units[unit]}`;
}

/** Transfer rate, e.g. "12.3 MB/s". */
export function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return '—';
  return `${formatBytes(bytesPerSecond)}/s`;
}

/** Remaining time from the current rate, e.g. "2m 30s". */
export function formatEta(remaining: number, bytesPerSecond: number): string {
  if (remaining <= 0 || bytesPerSecond <= 0) return '—';
  return formatDuration(remaining / bytesPerSecond);
}

/** Duration in seconds as a compact string. */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '—';
  if (seconds < 60) return `${Math.round(seconds)}s`;

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${Math.round(seconds % 60)}s`;

  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

/**
 * Completion as a 0-100 percentage. Returns null when the total is unknown,
 * so callers can show an indeterminate bar rather than a misleading zero.
 */
export function percentOf(done: number, total: number): number | null {
  if (total <= 0) return null;
  return Math.min(100, Math.max(0, (done / total) * 100));
}

/** Short label for a host, e.g. "gofile". */
export function hostLabel(host: string): string {
  return host || 'direct';
}
