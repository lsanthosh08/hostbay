/** Entries whose text (or name) holds every word of the filter, any case. */
export function matching<T>(items: T[], filter: string, text: (x: T) => string): T[] {
  const words = filter.toLowerCase().split(/\s+/).filter(Boolean);
  return items.filter((x) => {
    const s = text(x).toLowerCase();
    return words.every((w) => s.includes(w));
  });
}

/** A request's first line, for a list row. */
export const firstLine = (s: string) => s.trim().split("\n", 1)[0];
