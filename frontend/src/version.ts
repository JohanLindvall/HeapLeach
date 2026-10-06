// SPDX-License-Identifier: MIT
/**
 * What the header's version badge says.
 *
 * The page is part of the server binary, so the version the first snapshot
 * reports is the page's own. A later snapshot reporting another is a server
 * replaced by a deploy while this tab stayed open — the stream reconnects
 * to the new one, but the page goes on running the interface it was loaded
 * with until it is reloaded, which is worth saying.
 */
export interface VersionBadgeState {
  readonly label: string;
  readonly stale: boolean;
}

export function versionBadge(server: string | undefined, page: string | undefined): VersionBadgeState | null {
  if (!server) return null;
  return { label: server, stale: page !== undefined && page !== server };
}
