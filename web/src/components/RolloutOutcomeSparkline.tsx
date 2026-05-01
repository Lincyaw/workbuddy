interface RolloutOutcomeSparklineProps {
  outcomes: boolean[];
}

export function RolloutOutcomeSparkline({ outcomes }: RolloutOutcomeSparklineProps) {
  if (outcomes.length === 0) {
    return <span class="sparkline-placeholder">warming</span>;
  }
  const width = 132;
  const height = 26;
  const gap = 2;
  const glyphWidth = Math.max(1.5, (width - gap * (outcomes.length - 1)) / outcomes.length);

  return (
    <svg
      class="hook-sparkline rollout-outcome-sparkline"
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label="Rollout group success rate sparkline"
    >
      {outcomes.map((success, index) => {
        const x = index * (glyphWidth + gap);
        const barHeight = success ? height - 6 : Math.max(6, height / 2);
        const y = height - barHeight;
        return (
          <rect
            key={index}
            x={x.toFixed(2)}
            y={y.toFixed(2)}
            width={glyphWidth.toFixed(2)}
            height={barHeight.toFixed(2)}
            rx="1"
            class={success ? 'rollout-outcome-glyph is-success' : 'rollout-outcome-glyph is-warning'}
          />
        );
      })}
    </svg>
  );
}
