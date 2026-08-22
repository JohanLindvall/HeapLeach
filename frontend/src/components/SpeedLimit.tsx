import { formatSpeed } from '../format';
import { GaugeIcon } from './Icons';

/** Presets, in bytes per second. Zero is "no ceiling". */
const PRESETS = [0, 512 * 1024, 1e6, 2e6, 5e6, 10e6, 25e6, 50e6, 100e6];

interface SpeedLimitProps {
  readonly value: number;
  readonly onChange: (bytesPerSecond: number) => void;
}

/**
 * The total-throughput ceiling.
 *
 * A menu rather than a slider: the useful values span three orders of
 * magnitude, which no linear track can offer precisely. A limit set on the
 * command line that is not one of the presets is added to the list, so the
 * control always shows what is actually in force.
 */
export function SpeedLimit({ value, onChange }: SpeedLimitProps) {
  const options = PRESETS.includes(value) ? PRESETS : [...PRESETS, value].sort((a, b) => a - b);

  return (
    <label className="speedcap" htmlFor="speed-limit" title="Ceiling on total download rate">
      <GaugeIcon />
      <span className="speedcap__label">Limit</span>
      <select
        id="speed-limit"
        className="speedcap__select"
        value={value}
        onChange={(e) => onChange(Number(e.target.value))}
      >
        {options.map((option) => (
          <option key={option} value={option}>
            {option === 0 ? 'Unlimited' : formatSpeed(option)}
          </option>
        ))}
      </select>
    </label>
  );
}
