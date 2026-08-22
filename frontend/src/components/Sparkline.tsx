interface SparklineProps {
  readonly series: number[];
  readonly width?: number;
  readonly height?: number;
}

/**
 * The rolling throughput graph. Scaled to the window's own peak rather than
 * an absolute maximum, so a slow transfer still shows shape instead of a
 * flat line along the bottom.
 */
export function Sparkline({ series, width = 132, height = 34 }: SparklineProps) {
  const points = series.length > 1 ? series : [0, 0];
  const peak = Math.max(...points, 1);

  const step = width / (points.length - 1);
  const y = (v: number) => height - (v / peak) * (height - 3) - 1.5;

  const line = points.map((v, i) => `${i * step},${y(v)}`).join(' ');
  const area = `0,${height} ${line} ${width},${height}`;
  const latest = points[points.length - 1] ?? 0;

  return (
    <svg
      className="spark"
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      aria-hidden="true"
    >
      <defs>
        <linearGradient id="spark-fill" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="var(--accent-b)" stopOpacity="0.38" />
          <stop offset="100%" stopColor="var(--accent-b)" stopOpacity="0" />
        </linearGradient>
      </defs>
      <polygon points={area} fill="url(#spark-fill)" />
      <polyline
        points={line}
        fill="none"
        stroke="var(--accent-b)"
        strokeWidth="1.5"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
      {latest > 0 && (
        <circle cx={width} cy={y(latest)} r="2.5" fill="var(--accent-b)" />
      )}
    </svg>
  );
}
