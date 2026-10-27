import type { MonitorProc } from "../../api";

export type ProcKey = keyof MonitorProc;

/** A copy of procs sorted by key: numbers as numbers, text as text. */
export function sortProcs(procs: MonitorProc[], key: ProcKey, dir: "asc" | "desc"): MonitorProc[] {
  const sign = dir === "asc" ? 1 : -1;
  return [...procs].sort((a, b) => {
    const x = a[key];
    const y = b[key];
    const c = typeof x === "number" && typeof y === "number" ? x - y : String(x).localeCompare(String(y));
    return sign * c || a.pid - b.pid;
  });
}

/** An SVG polyline's points for values across a w×h box, 0 at the bottom
 *  and top (or the largest value) at the top. Fewer than two values draw
 *  nothing. */
export function sparkPoints(values: number[], w: number, h: number, top?: number): string {
  if (values.length < 2) return "";
  const max = top ?? Math.max(...values);
  const y = (v: number) => (max > 0 ? h - (Math.min(Math.max(v, 0), max) / max) * h : h);
  const step = w / (values.length - 1);
  return values.map((v, i) => `${+(i * step).toFixed(2)},${+y(v).toFixed(2)}`).join(" ");
}
