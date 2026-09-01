/**
 * Pulling links out of arbitrary text.
 *
 * The queue accepts whitespace-separated URLs, so text a user typed needs no
 * help. Text taken from the clipboard does: it arrives as whatever was
 * copied, which may be a line of prose around the link, several links at
 * once, or nothing resembling one — and sending that verbatim would ask the
 * server to reject it one piece at a time.
 */

/**
 * The links in `text`, in the order they appear.
 *
 * Only absolute http(s) URLs count. A scheme-less host is left out on
 * purpose: it is as likely to be a sentence's worth of words with a dot in
 * it as a link, and guessing wrong queues rubbish.
 */
export function linksIn(text: string): string[] {
  return text
    .split(/\s+/)
    .map((piece) => piece.trim())
    .filter((piece) => /^https?:\/\/\S/i.test(piece));
}
