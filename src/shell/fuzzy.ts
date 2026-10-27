// Fuzzy matching for the host search and the command palette: the query's
// letters must appear in order; matches at word starts and in runs score more.

const isWordStart = (text: string, i: number) => i === 0 || /[\s\-_.@:/]/.test(text[i - 1]);

/** A score for `text` against `query` (higher is better), or null when it does not match. */
export function fuzzyScore(query: string, text: string): number | null {
  const q = query.trim().toLowerCase();
  const s = text.toLowerCase();
  let score = 0;
  let at = 0;
  let prev = -2;
  for (const ch of q) {
    if (ch === " ") continue;
    const i = s.indexOf(ch, at);
    if (i < 0) return null;
    score += 1 + (isWordStart(s, i) ? 8 : 0) + (i === prev + 1 ? 4 : 0) - Math.min(i - at, 5) * 0.2;
    prev = i;
    at = i + 1;
  }
  // Between equal matches, the shorter text is the closer one.
  return score - s.length * 0.01;
}

/** The items that match `query`, best first; all of them, in order, for an empty query. */
export function fuzzyRank<T>(items: T[], query: string, text: (t: T) => string): T[] {
  if (!query.trim()) return items;
  return items
    .map((item) => ({ item, score: fuzzyScore(query, text(item)) }))
    .filter((r): r is { item: T; score: number } => r.score !== null)
    .sort((a, b) => b.score - a.score)
    .map((r) => r.item);
}
