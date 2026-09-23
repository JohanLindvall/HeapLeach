import { describe, expect, it } from 'vitest';
import {
  sliderDragged,
  sliderIdle,
  sliderReleased,
  sliderSettled,
  sliderShows,
  type SliderState,
} from './slider';

describe('sliderShows', () => {
  it('renders the server value when nothing is held', () => {
    expect(sliderShows(sliderIdle, 4)).toBe(4);
  });

  // The whole point: a snapshot arriving mid-drag must not move the thumb.
  it('renders the held value while the user is dragging', () => {
    const held = sliderDragged(sliderIdle, 13);
    expect(sliderShows(held, 4)).toBe(13);
  });

  // And must not move it in the gap between letting go and the server
  // confirming, which is where the flicker used to be.
  it('keeps rendering the held value while the request is outstanding', () => {
    const { state } = sliderReleased(sliderDragged(sliderIdle, 13), 4);
    expect(sliderShows(state, 4)).toBe(13);
  });
});

describe('sliderReleased', () => {
  it('sends the value the user let go of', () => {
    const { state, send } = sliderReleased(sliderDragged(sliderIdle, 13), 4);
    expect(send).toBe(13);
    expect(state).toEqual<SliderState>({ held: 13, sent: 13 });
  });

  it('sends nothing when the control was never moved', () => {
    expect(sliderReleased(sliderIdle, 4)).toEqual({ state: sliderIdle, send: null });
  });

  // Dragged away and back again is not a change, and asking the server to
  // set what it already has would leave a request outstanding forever.
  it('sends nothing when the value came back to where it started', () => {
    const { state, send } = sliderReleased(sliderDragged(sliderIdle, 4), 4);
    expect(send).toBeNull();
    expect(state).toEqual(sliderIdle);
  });

  // A pointer lifting and focus leaving afterwards are two releases of one
  // drag, and the second must not repeat the request.
  it('sends nothing again for a value already on its way', () => {
    const { state } = sliderReleased(sliderDragged(sliderIdle, 13), 4);
    expect(sliderReleased(state, 4)).toEqual({ state, send: null });
  });

  it('sends a new value dragged to while an earlier one is outstanding', () => {
    const { state } = sliderReleased(sliderDragged(sliderIdle, 13), 4);
    const { send } = sliderReleased(sliderDragged(state, 9), 4);
    expect(send).toBe(9);
  });

  it('sends the server value back when a request for another is outstanding', () => {
    const { state } = sliderReleased(sliderDragged(sliderIdle, 13), 4);
    const { send } = sliderReleased(sliderDragged(state, 4), 4);
    expect(send).toBe(4);
  });
});

describe('sliderSettled', () => {
  it('hands the control back to the snapshot', () => {
    expect(sliderSettled()).toEqual(sliderIdle);
    expect(sliderShows(sliderSettled(), 8)).toBe(8);
  });
});
