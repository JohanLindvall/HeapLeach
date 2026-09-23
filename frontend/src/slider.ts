// What a slider shows while the server catches up.
//
// Every control in this UI renders straight from the SSE snapshot, which is
// the right default: one authority, and a client that missed a frame heals
// on the next one. A slider is the exception, and the reason is the round
// trip. The snapshot arrives on a tick, so between the drag and the frame
// that confirms it the control re-renders with the value the server still
// has — the thumb jumps back under the finger, the next drag event fights
// it, and on a touch screen the control is close to unusable.
//
// So a slider under the hand renders what the hand is doing, and the
// snapshot takes over again once the server has answered. That is all this
// is: which of the two values to show, and when to stop preferring ours.

/** A slider's own state, apart from the value the server reports. */
export interface SliderState {
  /** What the user is holding, or null when they are not holding it. */
  readonly held: number | null;
  /** What the server was asked for, or null when nothing is outstanding. */
  readonly sent: number | null;
}

/** Nothing held and nothing outstanding: the snapshot decides. */
export const sliderIdle: SliderState = { held: null, sent: null };

/** The value to render. */
export function sliderShows(state: SliderState, server: number): number {
  return state.held ?? server;
}

/** The user moved the control. Nothing is sent yet. */
export function sliderDragged(state: SliderState, value: number): SliderState {
  return { held: value, sent: state.sent };
}

/**
 * The user let go.
 *
 * The request goes on release rather than on every step of the drag. A drag
 * from four to sixteen is a dozen intermediate values that nobody chose, and
 * sending them all would have the queue start and stop workers a dozen times
 * to arrive where it was going anyway — and, on a slow link, land out of
 * order, so the value that sticks is whichever reply came last rather than
 * the one under the finger when it lifted.
 *
 * The held value survives the release: dropping it here would show the
 * server's old value again for the moment before the answer arrives, which
 * is the same flicker this exists to prevent.
 */
export function sliderReleased(
  state: SliderState,
  server: number,
): { readonly state: SliderState; readonly send: number | null } {
  if (state.held === null) return { state: sliderIdle, send: null };
  // Already asked for and not yet answered. Release arrives by several
  // names — the pointer lifting and then focus leaving, most often — and
  // each of them must not ask again for what is already on its way.
  if (state.held === state.sent) return { state, send: null };
  // Back where the server is, with nothing else outstanding. With another
  // value outstanding it is a change after all: that request would
  // otherwise land and move the queue away from where the user left it.
  if (state.held === server && state.sent === null) return { state: sliderIdle, send: null };
  return { state: { held: state.held, sent: state.held }, send: state.held };
}

/**
 * The server has spoken, so it is the authority again.
 *
 * Called when the reported value changes after a request, whatever it
 * changed to: what we asked for, what the server clamped it to, or what
 * somebody changed it to in another tab. All three mean the same thing here
 * — stop showing ours.
 */
export function sliderSettled(): SliderState {
  return sliderIdle;
}
