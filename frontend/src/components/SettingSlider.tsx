import { useCallback, useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import {
  sliderDragged,
  sliderIdle,
  sliderReleased,
  sliderSettled,
  sliderShows,
  type SliderState,
} from '../slider';

/**
 * How long the control will go on showing the user's value when the server
 * never reports it.
 *
 * A request can fail, and a failed one leaves the slider showing a number
 * the queue does not have — the error toast says so, but the control would
 * go on lying until something else changed. Generous enough that an ordinary
 * round trip is never cut short, short enough to correct itself while the
 * user is still looking at it.
 */
const SETTLE_TIMEOUT_MS = 4000;

interface SettingSliderProps {
  readonly id: string;
  readonly icon: ReactNode;
  readonly label: string;
  readonly title: string;
  /** The value the server reports. */
  readonly value: number;
  readonly max: number;
  /** Called once, with the value the user let go of. */
  readonly onCommit: (value: number) => void;
}

/**
 * A slider that follows the finger and tells the server afterwards.
 *
 * See slider.ts for why it does not simply render the snapshot like
 * everything else here.
 */
export function SettingSlider({
  id,
  icon,
  label,
  title,
  value,
  max,
  onCommit,
}: SettingSliderProps) {
  const [state, setState] = useState<SliderState>(sliderIdle);
  const awaiting = useRef(false);
  const deadline = useRef<number | undefined>(undefined);

  const settle = useCallback((): void => {
    awaiting.current = false;
    window.clearTimeout(deadline.current);
    deadline.current = undefined;
    setState(sliderSettled());
  }, []);

  // The server's value changing is the signal that it has answered. The
  // effect is keyed on that value alone, so the snapshots that arrive twice
  // a second saying the same thing do not disturb a drag in progress.
  useEffect(() => {
    if (awaiting.current) settle();
  }, [value, settle]);

  useEffect(() => () => window.clearTimeout(deadline.current), []);

  const release = useCallback((): void => {
    setState((current) => {
      const { state: next, send } = sliderReleased(current, value);
      if (send !== null) {
        awaiting.current = true;
        window.clearTimeout(deadline.current);
        deadline.current = window.setTimeout(settle, SETTLE_TIMEOUT_MS);
        onCommit(send);
      }
      return next;
    });
  }, [onCommit, settle, value]);

  const shown = sliderShows(state, value);

  return (
    <label className="concurrency" htmlFor={id} title={title}>
      {icon}
      <span className="concurrency__label">{label}</span>
      <input
        id={id}
        type="range"
        min={1}
        max={max}
        value={shown}
        onChange={(e) => setState((current) => sliderDragged(current, Number(e.target.value)))}
        // Release is what sends, and it arrives by several names: a finger or
        // a mouse lifting, an arrow key coming back up, and focus leaving as
        // the net under both.
        onPointerUp={release}
        onKeyUp={release}
        onBlur={release}
      />
      <output className="concurrency__value">{shown}</output>
    </label>
  );
}
