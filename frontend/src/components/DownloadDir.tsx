import { useEffect, useRef, useState } from 'react';
import { FolderIcon } from './Icons';

interface DownloadDirProps {
  readonly value: string;
  readonly onChange: (dir: string) => void;
}

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
export function DownloadDir({ value, onChange }: DownloadDirProps) {
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

  if (!editing) {
    return (
      <button
        type="button"
        className="jobs__dir"
        onClick={open}
        title={`Saving to ${value} — click to change`}
      >
        <FolderIcon />
        <code>{value}</code>
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
