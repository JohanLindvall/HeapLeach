// SPDX-License-Identifier: MIT
import { useState } from 'react';
import { formatSpeed } from '../format';
import type { JobLimit } from '../types';

/** The queue's own limits, which a job's can only hold lower. */
export interface QueueLimits {
  readonly files: number;
  readonly streams: number;
  /** Bytes per second; 0 is unlimited. */
  readonly speed: number;
}

type Count = 'fileLimit' | 'streamLimit';

/** Speeds a job can be held to, in bytes per second. */
const SPEEDS = [256 * 1024, 512 * 1024, 1e6, 2e6, 5e6, 10e6, 25e6, 50e6];

const LABELS: Record<Count, string> = { fileLimit: 'Files at once', streamLimit: 'Streams per file' };

/** Only the limits that are set: a field left out has none of its own. */
function limitsOf(limit: JobLimit): JobLimit {
  const out: JobLimit = {};
  if (limit.fileLimit !== undefined) out.fileLimit = limit.fileLimit;
  if (limit.streamLimit !== undefined) out.streamLimit = limit.streamLimit;
  if (limit.speedLimit) out.speedLimit = limit.speedLimit;
  return out;
}

function sameLimits(a: JobLimit, b: JobLimit): boolean {
  return a.fileLimit === b.fileLimit && a.streamLimit === b.streamLimit && (a.speedLimit ?? 0) === (b.speedLimit ?? 0);
}

interface JobLimitsProps {
  /** Names the fields, which every open card has its own of. */
  readonly id: string;
  readonly limit: JobLimit;
  readonly queue: QueueLimits;
  /** Sends the job's whole limit; the server replaces what it had. */
  readonly onChange: (limit: JobLimit) => void;
}

/**
 * Lowers the queue's limits for one job: how many of its files download at
 * once, how many streams each may use, and how fast it may go. A field left
 * empty leaves the job to the queue's own; 0 files holds its new downloads
 * while those running finish.
 */
export function JobLimits({ id, limit, queue, onChange }: JobLimitsProps) {
  // What is being typed, until it is committed: the server's value shows
  // otherwise, so an edit never fights the snapshots arriving meanwhile.
  const [drafts, setDrafts] = useState<Partial<Record<Count, string>>>({});
  const current = limitsOf(limit);
  const most: Record<Count, number> = { fileLimit: queue.files, streamLimit: queue.streams };
  const least: Record<Count, number> = { fileLimit: 0, streamLimit: 1 };

  const send = (next: JobLimit): void => {
    if (!sameLimits(next, current)) onChange(limitsOf(next));
  };

  const commit = (field: Count): void => {
    const draft = drafts[field];
    setDrafts(({ [field]: _, ...rest }) => rest);
    if (draft === undefined) return;
    const text = draft.trim();
    const next = { ...current };
    if (text === '') {
      delete next[field];
    } else {
      const value = Number(text);
      if (!Number.isInteger(value)) return;
      next[field] = Math.min(most[field], Math.max(least[field], value));
    }
    send(next);
  };

  // Only speeds beneath the queue's own: one above it would change nothing.
  const speed = current.speedLimit ?? 0;
  const offered = queue.speed > 0 ? SPEEDS.filter((option) => option < queue.speed) : SPEEDS;
  const speeds = speed === 0 || offered.includes(speed) ? offered : [...offered, speed].sort((a, b) => a - b);

  return (
    <div className="job-limits" id={id} role="group" aria-label="Limits for this job">
      {(['fileLimit', 'streamLimit'] as const).map((field) => (
        <label key={field} className="job-limits__field" htmlFor={`${id}-${field}`}>
          <span>{LABELS[field]}</span>
          <input
            id={`${id}-${field}`}
            className="input job-limits__input"
            type="number"
            inputMode="numeric"
            min={least[field]}
            max={most[field]}
            step={1}
            placeholder={String(most[field])}
            value={drafts[field] ?? (current[field] === undefined ? '' : String(current[field]))}
            onChange={(e) => {
              const text = e.target.value;
              setDrafts((d) => ({ ...d, [field]: text }));
            }}
            onBlur={() => commit(field)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') e.currentTarget.blur();
            }}
          />
        </label>
      ))}
      <label className="job-limits__field" htmlFor={`${id}-speedLimit`}>
        <span>Speed</span>
        <select
          id={`${id}-speedLimit`}
          className="input job-limits__input"
          value={speed}
          onChange={(e) => send({ ...current, speedLimit: Number(e.target.value) })}
        >
          <option value={0}>{queue.speed > 0 ? `Queue’s (${formatSpeed(queue.speed)})` : 'Unlimited'}</option>
          {speeds.map((option) => (
            <option key={option} value={option}>
              {formatSpeed(option)}
            </option>
          ))}
        </select>
      </label>
      {Object.keys(current).length > 0 && (
        <button type="button" className="btn btn--ghost btn--sm" onClick={() => onChange({})}>
          Clear
        </button>
      )}
      <p className="job-limits__hint">Empty uses the queue’s limits. 0 files holds new downloads; those running finish.</p>
    </div>
  );
}
