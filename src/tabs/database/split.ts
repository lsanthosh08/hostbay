/** The query editor block (its tab strip, toolbar and text) starts about six
 *  lines tall, so the results get the rest of the tab. */
export const DEFAULT_EDITOR = 190;
/** The editor never shrinks below its tab strip, toolbar and two lines. */
export const MIN_EDITOR = 120;
/** The results keep room for their status line, the filter bar and a few rows. */
export const MIN_RESULTS = 240;
/** What an arrow key on the separator moves it by. */
export const SPLIT_STEP = 24;

/** The editor's height in px, kept between the minimum editor and what
 *  leaves the results their minimum out of `total`, the height both share.
 *  In a `total` too small for both, the editor keeps its minimum. An unknown
 *  `total` (0, before the first measure) leaves the height as asked. */
export function clampEditor(editor: number, total: number): number {
  const asked = Number.isNaN(editor) ? DEFAULT_EDITOR : editor;
  if (!(total > 0)) return Math.max(MIN_EDITOR, asked);
  return Math.min(Math.max(MIN_EDITOR, asked), Math.max(MIN_EDITOR, total - MIN_RESULTS));
}
