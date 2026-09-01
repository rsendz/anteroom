/**
 * Phrasing helpers for the waiting page. Kept apart from main.ts, which runs
 * the page as a side effect of being imported and so cannot be loaded by a
 * test.
 *
 * etaText in internal/httpserver/visitor.go says the same things in Go, for
 * the first render and for visitors without JavaScript. The two have to agree.
 */

/** Rounds a wait to something a person would actually say out loud. */
export function formatWait(seconds: number): string {
  if (seconds < 60) {
    const rounded = Math.max(5, Math.ceil(seconds / 5) * 5);
    return `${rounded} seconds`;
  }
  const minutes = Math.ceil(seconds / 60);
  if (minutes < 60) return minutes === 1 ? "a minute" : `${minutes} minutes`;
  const hours = Math.round(minutes / 6) / 10;
  return hours === 1 ? "an hour" : `${hours} hours`;
}

/** Groups digits the way the visitor's own locale does. */
export function format(n: number): string {
  return n.toLocaleString();
}
