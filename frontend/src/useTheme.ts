import { useCallback, useEffect, useState } from 'react';

/** What the user picked, as opposed to what is showing. */
export type ThemePreference = 'system' | 'dark' | 'light';

/** What is actually on screen. */
export type Theme = 'dark' | 'light';

const STORAGE_KEY = 'heapleach.theme';

function storedPreference(): ThemePreference {
  try {
    const saved = window.localStorage.getItem(STORAGE_KEY);
    if (saved === 'dark' || saved === 'light') return saved;
  } catch {
    // Private mode or a blocked store: fall through to following the OS.
  }
  return 'system';
}

function systemTheme(): Theme {
  return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

/**
 * Resolves the theme and stamps it on the document.
 *
 * The stylesheet keys entirely off data-theme rather than a media query, so
 * an explicit choice and the system default go through one path. "system"
 * keeps following the OS, including when it changes while the page is open.
 */
export function useTheme(): {
  preference: ThemePreference;
  theme: Theme;
  setPreference: (next: ThemePreference) => void;
  cycle: () => void;
} {
  const [preference, setStored] = useState<ThemePreference>(storedPreference);
  const [system, setSystem] = useState<Theme>(systemTheme);

  // Keep following the OS while the preference is "system".
  useEffect(() => {
    const query = window.matchMedia('(prefers-color-scheme: light)');
    const update = () => setSystem(query.matches ? 'light' : 'dark');
    query.addEventListener('change', update);
    return () => query.removeEventListener('change', update);
  }, []);

  const theme: Theme = preference === 'system' ? system : preference;

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  const setPreference = useCallback((next: ThemePreference) => {
    setStored(next);
    try {
      if (next === 'system') {
        window.localStorage.removeItem(STORAGE_KEY);
      } else {
        window.localStorage.setItem(STORAGE_KEY, next);
      }
    } catch {
      // Not being able to remember the choice is not worth failing over.
    }
  }, []);

  const cycle = useCallback(() => {
    setPreference(theme === 'dark' ? 'light' : 'dark');
  }, [theme, setPreference]);

  return { preference, theme, setPreference, cycle };
}
