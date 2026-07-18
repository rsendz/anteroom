import { memo } from "react";

type Props = {
  values: number[];
  label: string;
};

const WIDTH = 200;
const HEIGHT = 36;

/**
 * The queue depth over the last couple of minutes. No axes or gridlines: the
 * only question it answers is "is the line growing or draining", and the
 * exact numbers are already on the tiles beside it.
 */
export const Sparkline = memo(function Sparkline({ values, label }: Props) {
  if (values.length < 2) {
    return <div className="sparkline sparkline--empty">Gathering history…</div>;
  }

  const peak = Math.max(...values, 1);
  const step = WIDTH / (values.length - 1);
  const points = values
    .map((value, i) => {
      const x = i * step;
      // Leave a pixel of headroom so a flat line at the peak stays visible.
      const y = HEIGHT - 1 - (value / peak) * (HEIGHT - 2);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");

  const latest = values[values.length - 1] ?? 0;
  const first = values[0] ?? 0;
  const trend = latest > first ? "growing" : latest < first ? "draining" : "steady";

  return (
    <svg
      className="sparkline"
      viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      preserveAspectRatio="none"
      role="img"
      aria-label={`${label}: ${trend}, peak ${peak}, now ${latest}`}
    >
      <polyline className="sparkline__area" points={`0,${HEIGHT} ${points} ${WIDTH},${HEIGHT}`} />
      <polyline className="sparkline__line" points={points} />
    </svg>
  );
});
