import { memo } from "react";

type Props = {
  values: number[];
  label: string;
};

const WIDTH = 200;
const HEIGHT = 36;

/**
 * The queue depth over the last couple of minutes. No axes or gridlines: the
 * only question it answers is "is the line growing or draining", and the exact
 * numbers are already on the tiles beside it.
 *
 * It is scaled to the range actually observed rather than from zero, because a
 * queue moving between 300 and 310 would otherwise be a flat line pinned to
 * the top and show nothing at all. That is also why there is no area fill: a
 * filled shape under an axis that does not start at zero reads as a magnitude
 * it is not.
 */
export const Sparkline = memo(function Sparkline({ values, label }: Props) {
  if (values.length < 2) {
    return <div className="sparkline sparkline--empty">Gathering history…</div>;
  }

  const low = Math.min(...values);
  const high = Math.max(...values);
  const span = high - low;
  const step = WIDTH / (values.length - 1);

  const y = (value: number) => {
    // A queue that has not moved sits in the middle rather than dividing by a
    // zero range and landing anywhere.
    if (span === 0) return HEIGHT / 2;
    return HEIGHT - 2 - ((value - low) / span) * (HEIGHT - 4);
  };
  const points = values.map((value, i) => `${(i * step).toFixed(1)},${y(value).toFixed(1)}`).join(" ");

  const latest = values[values.length - 1] ?? 0;
  const first = values[0] ?? 0;
  const trend = latest > first ? "growing" : latest < first ? "draining" : "steady";

  return (
    <div className="sparkline-row">
      <svg
        className="sparkline"
        viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
        preserveAspectRatio="none"
        role="img"
        aria-label={`${label}: ${trend}, between ${low} and ${high}, now ${latest}`}
      >
        <polyline className="sparkline__line" points={points} />
      </svg>
      <span className="sparkline__caption">
        {span === 0 ? "steady" : `${trend} · ${low.toLocaleString()}–${high.toLocaleString()}`}
      </span>
    </div>
  );
});
