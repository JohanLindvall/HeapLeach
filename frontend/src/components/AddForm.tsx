import { memo, useRef, useState, type FormEvent } from 'react';
import { addUrls, ApiError } from '../api';
import { linksIn } from '../links';
import type { AddResponse } from '../types';
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

/**
 * URL entry: accepts a paste of many links, one per line. Memoised since it
 * reads nothing from the snapshot, and a paste of thousands of links is
 * otherwise split again on every frame.
 */
export const AddForm = memo(function AddForm({ onNotice }: AddFormProps) {
  const [urls, setUrls] = useState('');
  const [password, setPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [busy, setBusy] = useState(false);
  const pending = useRef(false);

  // A ref closes the gap before React paints the disabled button. It also
  // covers the clipboard permission prompt, which may stay open for a while.
  const whileBusy = async (action: () => Promise<void>): Promise<void> => {
    if (pending.current) return;
    pending.current = true;
    setBusy(true);
    try {
      await action();
    } finally {
      pending.current = false;
      setBusy(false);
    }
  };

  const send = async (text: string): Promise<AddResponse | null> => {
    try {
      const result = await addUrls(text, password);
      if (result.accepted.length > 0) {
        onNotice(
          `Queued ${result.accepted.length} link${result.accepted.length === 1 ? '' : 's'}.`,
          'info',
        );
      }
      if (result.rejected.length > 0) {
        const details = result.rejected.slice(0, 3).map((bad) => `${bad.url}: ${bad.error}`).join('\n');
        onNotice(`${result.rejected.length} link(s) could not be queued.\n${details}`, 'error');
      }
      return result;
    } catch (error) {
      onNotice(error instanceof ApiError ? error.message : 'Could not reach the server.', 'error');
      return null;
    }
  };

  const submit = async (event: FormEvent): Promise<void> => {
    event.preventDefault();

    const trimmed = urls.trim();
    if (!trimmed) {
      onNotice('Paste at least one URL first.', 'error');
      return;
    }
    await whileBusy(async () => {
      const result = await send(trimmed);
      if (!result || result.accepted.length === 0) return;
      // Keep rejected links available for correction, and never erase text
      // typed while this request was in flight.
      setUrls((current) => current.trim() === trimmed
        ? result.rejected.map((bad) => bad.url).join('\n')
        : current);
    });
  };

  // The whole point is to skip the paste, so this queues what it finds
  // rather than filling the box and waiting for a second click. readText is
  // called first thing in the handler because some browsers only allow the
  // read while the click that asked for it is still being handled.
  const queueClipboard = async (): Promise<void> => {
    await whileBusy(async () => {
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
      const result = await send(links.join('\n'));
      if (result && result.rejected.length > 0) {
        setUrls((current) => [current.trim(), ...result.rejected.map((bad) => bad.url)].filter(Boolean).join('\n'));
      }
    });
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
});
