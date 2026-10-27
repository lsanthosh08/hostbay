import { sparkPoints } from "./monitor";

const W = 120;
const H = 32;

/** A line of recent values in inline SVG, so the monitor adds no chart
 *  library. top fixes the scale (100 for a percentage); without it the
 *  largest value fills the box. */
export function Sparkline({ values, top, label }: { values: number[]; top?: number; label: string }) {
  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      className="w-full h-8 text-accent"
      role="img"
      aria-label={label}
    >
      <polyline
        points={sparkPoints(values, W, H, top)}
        fill="none"
        stroke="currentColor"
        strokeWidth={1.5}
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
