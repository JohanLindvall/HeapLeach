// SPDX-License-Identifier: MIT
import { useEffect, useState } from 'react';

/**
 * How long a control goes on showing a choice the server has not yet
 * reported back. A request can fail, and a failed one would leave the
 * control showing a value the queue does not have — the error says so, but
 * the control would go on lying until something else changed. Generous
 * enough that an ordinary round trip is never cut short, short enough to
 * correct itself while the user is still looking at it.
 */
export const SETTLE_TIMEOUT_MS = 4000;

/**
 * A choice the user has made, shown in place of the server's value until a
 * snapshot agrees with it, or for SETTLE_TIMEOUT_MS once it has been sent if
 * none does: a disconnected tab must not keep an override. null is no
 * choice, and shows the server's value.
 */
export function useSettledChoice<T>(server: T, sending: boolean): readonly [T | null, (choice: T | null) => void] {
  const [choice, setChoice] = useState<T | null>(null);
  useEffect(() => {
    if (choice === null || sending) return;
    if (choice === server) {
      setChoice(null);
      return;
    }
    const timer = window.setTimeout(() => setChoice(null), SETTLE_TIMEOUT_MS);
    return () => window.clearTimeout(timer);
  }, [choice, server, sending]);
  return [choice, setChoice];
}
