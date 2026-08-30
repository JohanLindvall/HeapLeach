import { useEffect, useRef, useState } from 'react';
import { formatBytes } from '../format';
import { FolderIcon } from './Icons';

interface DownloadDirProps {
  readonly value: string;
  readonly onChange: (dir: string) => void;
  /** Bytes still writable there, and the size of the filesystem holding it. */
  readonly free: number;
  readonly total: number;
  /** Room that must be left before another transfer starts. */
  readonly minFree: number;
}

/**
 * Below this share of the filesystem left, the figure stops being a footnote
 * and starts being a warning. A queue can be hundreds of gigabytes, so the
 * useful moment to notice is well before the last block goes.
 */
const LOW_FRACTION = 0.1;

/**
 * Where finished files land, and a way to change it.
 *
 * A button until it is clicked, because the path is far more often read than
 * edited — and a text field sitting open invites a stray keystroke into the
 * one setting that decides where a hundred files get written.
 *
 * The server is what validates: it expands `~`, resolves the path, creates
 * the directory and proves it writable, and says why if it cannot. So this
 * submits and reports rather than guessing at any of that itself. The value
 * shown always comes back from the snapshot, so what is displayed is the
 * directory actually in force rather than what was typed.
 */
export function DownloadDir({ value, onChange, free, total, minFree }: DownloadDirProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(value);
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editing) input.current?.select();
  }, [editing]);

  const open = (): void => {
    setDraft(value);
    setEditing(true);
  };

  const commit = (): void => {
    setEditing(false);
    const next = draft.trim();
    // Nothing to do, and nothing to report: submitting the value that is
    // already in force would only produce a needless round trip.
    if (next && next !== value) onChange(next);
  };

  // A total of zero is a destination that could not be measured, not one
  // with no room: reporting "0 B free" for a directory that has simply gone
  // missing would be worse than saying nothing.
  //
  // It also decides what a phone shows. There is no room there for both the
  // path and the figure, and the figure is the one worth having, so the
  // control drops the path at that width — but only when there is a figure
  // to drop it for, which is what the modifier below says.
  const measured = total > 0;
  // Below the floor nothing new starts, so the figure stops being a warning
  // and becomes the reason the queue is sitting still.
  const held = measured && minFree > 0 && free < minFree;
  const low = held || (measured && free < total * LOW_FRACTION);

  if (!editing) {
    return (
      <button
        type="button"
        className={measured ? 'jobs__dir jobs__dir--measured' : 'jobs__dir'}
        onClick={open}
        title={
          held
            ? `Saving to ${value} — ${formatBytes(free)} free, below the ${formatBytes(minFree)} ` +
              'floor, so downloads are waiting. Free some space and they resume by themselves.'
            : measured
              ? `Saving to ${value} — ${formatBytes(free)} free of ${formatBytes(total)}. Click to change.`
              : `Saving to ${value} — click to change`
        }
      >
        <FolderIcon />
        <code>{value}</code>
        {measured && (
          <span className={low ? 'jobs__free jobs__free--low' : 'jobs__free'}>
            {held ? `${formatBytes(free)} free — downloads held` : `${formatBytes(free)} free`}
          </span>
        )}
      </button>
    );
  }

  return (
    <form
      className="jobs__dir jobs__dir--editing"
      onSubmit={(e) => {
        e.preventDefault();
        commit();
      }}
    >
      <FolderIcon />
      <input
        ref={input}
        type="text"
        className="jobs__dir-input"
        aria-label="Download directory"
        value={draft}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        onChange={(e) => setDraft(e.target.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            // Abandon the edit rather than commit it: Escape means "forget
            // this", and blur would otherwise submit what was typed.
            setDraft(value);
            setEditing(false);
          }
        }}
      />
    </form>
  );
}
