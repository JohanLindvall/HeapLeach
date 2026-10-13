// SPDX-License-Identifier: MIT
import { useState } from 'react';
import type { GroupLimit, Snapshot } from '../types';

type Field = keyof GroupLimit;

interface GroupLimitsProps {
  readonly snapshot: Snapshot;
  /** Sends the complete set of limits; the server replaces what it had. */
  readonly onChange: (groups: Record<string, GroupLimit>) => void;
}

/**
 * Lowers the queue's limits for one transfer group: a service such as
 * keep2share, or the site a job was added from. A field left empty leaves
 * the group to the limits above it, and 0 files holds the group's new
 * downloads while those running finish.
 */
export function GroupLimits({ snapshot, onChange }: GroupLimitsProps) {
  const groups = snapshot.groups ?? [];
  const limits = snapshot.groupLimits ?? {};
  // What is being typed, until it is committed: the server's value shows
  // otherwise, so an edit never fights the snapshots arriving meanwhile.
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const key = (group: string, field: Field): string => `${group}\u0000${field}`;
  const most: Record<Field, number> = { files: snapshot.concurrency, streams: snapshot.streams };
  const least: Record<Field, number> = { files: 0, streams: 1 };

  const commit = (group: string, field: Field): void => {
    const draft = drafts[key(group, field)];
    setDrafts(({ [key(group, field)]: _, ...rest }) => rest);
    if (draft === undefined) return;
    const next: Record<string, GroupLimit> = { ...limits, [group]: { ...limits[group] } };
    const text = draft.trim();
    if (text === '') {
      delete next[group]![field];
    } else {
      const value = Number(text);
      if (!Number.isInteger(value)) return;
      next[group]![field] = Math.min(most[field], Math.max(least[field], value));
    }
    if (next[group]!.files === limits[group]?.files && next[group]!.streams === limits[group]?.streams) return;
    if (next[group]!.files === undefined && next[group]!.streams === undefined) delete next[group];
    onChange(next);
  };

  const clear = (group: string): void => {
    const { [group]: _, ...rest } = limits;
    onChange(rest);
  };

  if (groups.length === 0) return null;
  return (
    <section className="group-limits" aria-labelledby="group-limits-title">
      <h3 id="group-limits-title">Limits per group</h3>
      <p>Lower a service or site beneath the limits above. Empty uses those; 0 files holds its new downloads.</p>
      <div className="group-limits__table" role="table" aria-labelledby="group-limits-title">
        <div className="group-limits__row group-limits__row--head" role="row">
          <span role="columnheader">Group</span>
          <span role="columnheader">Files at once</span>
          <span role="columnheader">Streams per file</span>
          <span role="columnheader" className="sr-only">Actions</span>
        </div>
        {groups.map((group) => {
          const limit = limits[group];
          const shown = (field: Field): string =>
            drafts[key(group, field)] ?? (limit?.[field] === undefined ? '' : String(limit[field]));
          return (
            <div className="group-limits__row" role="row" key={group}>
              <span role="cell" className="group-limits__name" title={group}>{group}</span>
              {(['files', 'streams'] as const).map((field) => (
                <span role="cell" key={field}>
                  <input type="number" inputMode="numeric" min={least[field]} max={most[field]} step={1}
                    aria-label={`${group}: ${field === 'files' ? 'files at once' : 'streams per file'}`}
                    placeholder={String(most[field])} value={shown(field)}
                    onChange={(e) => {
                      const text = e.target.value;
                      setDrafts((d) => ({ ...d, [key(group, field)]: text }));
                    }}
                    onBlur={() => commit(group, field)}
                    onKeyDown={(e) => { if (e.key === 'Enter') e.currentTarget.blur(); }} />
                </span>
              ))}
              <span role="cell">
                {limit && (
                  <button type="button" className="btn btn--ghost btn--sm" onClick={() => clear(group)}
                    aria-label={`Clear the limits for ${group}`}>Clear</button>
                )}
              </span>
            </div>
          );
        })}
      </div>
    </section>
  );
}
