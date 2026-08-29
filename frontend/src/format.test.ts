import { describe, expect, it } from 'vitest';
import { formatBytes, formatDuration, formatEta, formatSpeed, hostLabel, percentOf } from './format';

// The terminal renders the same numbers (internal/cli/format.go), and the
// contract there is digit-for-digit agreement: the same transfer must not
// read "1.5 GB" in one place and "1.6 GB" in the other. The byte cases
// below mirror the Go table exactly; where the two sides deliberately
// differ — this one says "—" where an int64 parameter says "?" — the case
// says so.
describe('formatBytes', () => {
  it.each([
    [0, '0 B'],
    [512, '512 B'],
    [999, '999 B'],
    [1000, '1.0 kB'],
    [1536, '1.5 kB'],
    [102400, '102 kB'],
    [1000000, '1.0 MB'],
    [999_449_999, '999 MB'],
    [1610612736, '1.6 GB'],
    [5_000_000_000_000, '5.0 TB'],
  ])('renders %d as %s, matching the terminal', (bytes, want) => {
    expect(formatBytes(bytes)).toBe(want);
  });

  it('declines a negative or unusable count', () => {
    expect(formatBytes(-1)).toBe('—');
    expect(formatBytes(Number.NaN)).toBe('—');
    expect(formatBytes(Number.POSITIVE_INFINITY)).toBe('—');
  });

  it('renders a fractional count as whole bytes', () => {
    // A smoothed rate decays through fractional values; interpolated as it
    // stands it once rendered "1.237576767 B/s" and broke the column layout.
    expect(formatBytes(1.237576767)).toBe('1 B');
  });
});

describe('formatSpeed', () => {
  it('is an em dash while nothing moves', () => {
    expect(formatSpeed(0)).toBe('—');
    expect(formatSpeed(-5)).toBe('—');
  });
  it('appends the unit', () => {
    expect(formatSpeed(1_000_000)).toBe('1.0 MB/s');
  });
});

describe('formatDuration', () => {
  it.each([
    [0.4, '0s'],
    [45, '45s'],
    [90, '1m 30s'],
    // Rounded before splitting into fields: rounding the remainder
    // afterwards would render 119.7s as "1m 60s".
    [119.7, '2m 0s'],
    [3900, '1h 5m'],
    [90000, '1d 1h'],
  ])('renders %ss as %s', (seconds, want) => {
    expect(formatDuration(seconds)).toBe(want);
  });
  it('declines a negative or unusable duration', () => {
    expect(formatDuration(-1)).toBe('—');
    expect(formatDuration(Number.NaN)).toBe('—');
  });
});

describe('formatEta', () => {
  it('refuses to guess without a rate or a remainder', () => {
    expect(formatEta(1000, 0)).toBe('—');
    expect(formatEta(0, 1000)).toBe('—');
  });
  it('divides what is left by the rate', () => {
    expect(formatEta(3000, 1000)).toBe('3s');
  });
});

describe('percentOf', () => {
  it('is null when the total is unknown, so bars can be indeterminate', () => {
    expect(percentOf(10, 0)).toBeNull();
    expect(percentOf(10, -1)).toBeNull();
  });
  it('clamps to the 0-100 range', () => {
    expect(percentOf(150, 100)).toBe(100);
    expect(percentOf(-5, 100)).toBe(0);
    expect(percentOf(25, 100)).toBe(25);
  });
});

describe('hostLabel', () => {
  it('names a bare file link for what it is', () => {
    expect(hostLabel('')).toBe('direct');
    expect(hostLabel('gofile')).toBe('gofile');
  });
});
