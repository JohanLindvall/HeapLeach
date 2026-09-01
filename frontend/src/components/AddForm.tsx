import { useState, type FormEvent } from 'react';
import { addUrls, ApiError } from '../api';
import { linksIn } from '../links';
import { ClipboardIcon, DownloadIcon } from './Icons';

interface AddFormProps {
  readonly onNotice: (message: string, kind: 'info' | 'error') => void;
}

/**
 * Reading the clipboard needs a secure context — https, or localhost — and
 * the browser's permission. Deciding once, here, keeps a button that could
 * never work from appearing at all: served over plain http to another
 * machine the API is simply absent, and an offer that always fails is worse
 * than no offer.
 */
const CAN_READ_CLIPBOARD =
  typeof navigator !== 'undefined' && typeof navigator.clipboard?.readText === 'function';

/** URL entry: accepts a paste of many links, one per line. */
export function AddForm({ onNotice }: AddFormProps) {
  const [urls, setUrls] = useState('');
  const [password, setPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [busy, setBusy] = useState(false);

  // Returns whether anything was queued, so the caller can decide what to
  // clear: text the user typed is theirs to lose only once it is safely in
  // the queue, and a clipboard grab must not empty a box it never filled.
  const send = async (text: string): Promise<boolean> => {
    setBusy(true);
    try {
      const result = await addUrls(text, password);
      if (result.accepted.length > 0) {
        onNotice(
          `Queued ${result.accepted.length} link${result.accepted.length === 1 ? '' : 's'}.`,
          'info',
        );
      }
      for (const bad of result.rejected) {
        onNotice(`${bad.url}: ${bad.error}`, 'error');
      }
      return result.accepted.length > 0;
    } catch (error) {
      onNotice(error instanceof ApiError ? error.message : 'Could not reach the server.', 'error');
      return false;
    } finally {
      setBusy(false);
    }
  };

  const submit = async (event: FormEvent): Promise<void> => {
    event.preventDefault();
    if (busy) return;

    const trimmed = urls.trim();
    if (!trimmed) {
      onNotice('Paste at least one URL first.', 'error');
      return;
    }
    if (await send(trimmed)) setUrls('');
  };

  // The whole point is to skip the paste, so this queues what it finds
  // rather than filling the box and waiting for a second click. readText is
  // called first thing in the handler because some browsers only allow the
  // read while the click that asked for it is still being handled.
  const queueClipboard = async (): Promise<void> => {
    if (busy) return;
    let text: string;
    try {
      text = await navigator.clipboard.readText();
    } catch {
      onNotice('The browser would not hand over the clipboard.', 'error');
      return;
    }
    const links = linksIn(text);
    if (links.length === 0) {
      onNotice('No link in the clipboard.', 'error');
      return;
    }
    await send(links.join('\n'));
  };

  // Split on any whitespace, exactly as the server does; only the empty
  // piece a leading newline produces needs filtering out.
  const count = urls.split(/\s+/).filter((piece) => piece.length > 0).length;

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
          {CAN_READ_CLIPBOARD && (
            <button
              type="button"
              className="btn btn--ghost btn--sm"
              title="Queue the links in the clipboard"
              disabled={busy}
              onClick={() => void queueClipboard()}
            >
              <ClipboardIcon />
              Clipboard
            </button>
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
