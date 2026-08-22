import { useState, type FormEvent } from 'react';
import { addUrls, ApiError } from '../api';
import { DownloadIcon } from './Icons';

interface AddFormProps {
  readonly onNotice: (message: string, kind: 'info' | 'error') => void;
}

/** URL entry: accepts a paste of many links, one per line. */
export function AddForm({ onNotice }: AddFormProps) {
  const [urls, setUrls] = useState('');
  const [password, setPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent): Promise<void> => {
    event.preventDefault();
    if (busy) return;

    const trimmed = urls.trim();
    if (!trimmed) {
      onNotice('Paste at least one URL first.', 'error');
      return;
    }

    setBusy(true);
    try {
      const result = await addUrls(trimmed, password);
      if (result.accepted.length > 0) {
        setUrls('');
        onNotice(
          `Queued ${result.accepted.length} link${result.accepted.length === 1 ? '' : 's'}.`,
          'info',
        );
      }
      for (const bad of result.rejected) {
        onNotice(`${bad.url}: ${bad.error}`, 'error');
      }
    } catch (error) {
      onNotice(error instanceof ApiError ? error.message : 'Could not reach the server.', 'error');
    } finally {
      setBusy(false);
    }
  };

  const count = urls
    .split(/\s+/)
    .filter((line) => line.trim().length > 0).length;

  return (
    <form className="card add" onSubmit={(e) => void submit(e)}>
      <div className="add__head">
        <label className="add__label" htmlFor="urls">
          Links
        </label>
        <span className="add__hint">One per line</span>
      </div>

      <textarea
        id="urls"
        className="add__input"
        placeholder={
          'https://gofile.io/d/…\nhttps://bunkr.cr/f/…\nhttps://mega.nz/file/…#…\nhttps://youtu.be/…'
        }
        value={urls}
        spellCheck={false}
        autoCapitalize="off"
        autoCorrect="off"
        rows={4}
        onChange={(e) => setUrls(e.target.value)}
        onKeyDown={(e) => {
          // Ctrl/Cmd+Enter submits without leaving the textarea.
          if ((e.metaKey || e.ctrlKey) && e.key === 'Enter') {
            void submit(e);
          }
        }}
      />

      <div className="add__actions">
        <button
          type="button"
          className="btn btn--ghost btn--sm"
          aria-expanded={showPassword}
          onClick={() => setShowPassword((v) => !v)}
        >
          {showPassword ? 'Hide password' : 'Password protected?'}
        </button>

        <div className="add__submit">
          {count > 0 && (
            <span className="add__count">
              {count} link{count === 1 ? '' : 's'}
            </span>
          )}
          <button type="submit" className="btn btn--primary" disabled={busy}>
            <DownloadIcon />
            {busy ? 'Adding…' : 'Add to queue'}
          </button>
        </div>
      </div>

      {showPassword && (
        <div className="add__password">
          <label htmlFor="password">Folder password</label>
          <input
            id="password"
            type="password"
            className="input"
            value={password}
            autoComplete="off"
            placeholder="Only needed for protected links"
            onChange={(e) => setPassword(e.target.value)}
          />
        </div>
      )}
    </form>
  );
}
