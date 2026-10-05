import { versionBadge } from '../version';

interface VersionBadgeProps {
  /** The version the server reports now. */
  readonly version?: string;
  /** The version the page was loaded with: the first one reported. */
  readonly pageVersion?: string;
}

/**
 * The server's version beside the name, and an offer to reload when a
 * deploy has replaced the server underneath this page.
 */
export function VersionBadge({ version, pageVersion }: VersionBadgeProps) {
  const badge = versionBadge(version, pageVersion);
  if (!badge) return null;
  if (badge.stale) {
    return (
      <button
        type="button"
        className="version version--stale"
        onClick={() => window.location.reload()}
        title={`The server was updated to ${badge.label}; this page still runs ${pageVersion}. Reload to update it.`}
      >
        {badge.label} · reload
      </button>
    );
  }
  return (
    <span className="version" title={`HeapLeach ${badge.label}`}>
      {badge.label}
    </span>
  );
}
