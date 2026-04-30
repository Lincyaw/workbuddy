interface HookLatencySparklineProps {
  samples: number[];
}

export function HookLatencySparkline({ samples }: HookLatencySparklineProps) {
  if (samples.length === 0) {
    return <span class="sparkline-placeholder">warming</span>;
  }
  const width = 108;
  const height = 28;
  const max = Math.max(...samples, 1);
  const min = Math.min(...samples, 0);
  const span = Math.max(max - min, 1);
  const step = samples.length > 1 ? width / (samples.length - 1) : width;
  const path = samples
    .map((sample, index) => {
      const x = index * step;
      const y = height - ((sample - min) / span) * (height - 4) - 2;
      return `${index === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(' ');

  return (
    <svg
      class="hook-sparkline"
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label="Hook latency sparkline"
    >
      <path d={`M0,${height - 2} L${width},${height - 2}`} class="hook-sparkline-baseline" />
      <path d={path} class="hook-sparkline-line" />
    </svg>
  );
}
