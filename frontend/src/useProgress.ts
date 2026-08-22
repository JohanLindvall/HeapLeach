import { useEffect, useRef, useState } from 'react';
import {
  accumulate,
  EMPTY_PROGRESS,
  newlyUnlocked,
  type Achievement,
  type Progress,
} from './gamification';
import type { Snapshot } from './types';

const STORAGE_KEY = 'heapleach.progress.v1';

function load(): Progress {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return EMPTY_PROGRESS;
    const parsed = JSON.parse(raw) as Partial<Progress>;
    return { ...EMPTY_PROGRESS, ...parsed };
  } catch {
    // Corrupt or unavailable storage is not worth failing the page over.
    return EMPTY_PROGRESS;
  }
}

function save(progress: Progress): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(progress));
  } catch {
    // Private-mode or quota errors are ignored: progress is a nicety.
  }
}

/**
 * Tracks cumulative download progress and reports achievements as they are
 * unlocked. State is persisted so a reload does not reset the session.
 */
export function useProgress(
  snapshot: Snapshot | null,
  onUnlock: (achievement: Achievement) => void,
): Progress {
  const [progress, setProgress] = useState<Progress>(load);
  const unlockRef = useRef(onUnlock);
  unlockRef.current = onUnlock;

  useEffect(() => {
    if (!snapshot) return;

    setProgress((current) => {
      const next = accumulate(current, snapshot);
      const fresh = newlyUnlocked(next, snapshot.hostCount);

      const updated =
        fresh.length > 0
          ? { ...next, unlocked: [...next.unlocked, ...fresh.map((a) => a.id)] }
          : next;

      // Notify outside the reducer so React never sees a side effect during
      // a state update.
      if (fresh.length > 0) {
        queueMicrotask(() => {
          for (const achievement of fresh) unlockRef.current(achievement);
        });
      }

      const changed =
        updated.filesCompleted !== current.filesCompleted ||
        updated.bytesDownloaded !== current.bytesDownloaded ||
        updated.peakSpeed !== current.peakSpeed ||
        updated.maxParallel !== current.maxParallel ||
        updated.unlocked.length !== current.unlocked.length ||
        updated.hostsUsed.length !== current.hostsUsed.length ||
        updated.countedItems.length !== current.countedItems.length;

      if (!changed) return current;
      save(updated);
      return updated;
    });
  }, [snapshot]);

  return progress;
}
