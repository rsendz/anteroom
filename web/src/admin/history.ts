/**
 * The queue history the dashboard keeps in the browser, and the CSV an
 * operator downloads from it.
 *
 * Nothing here talks to the server: the samples are the polls the dashboard is
 * already making, so exporting them costs no request and works for whatever
 * the operator happened to have open during an incident.
 */

/** One poll of one room. */
export type Sample = {
  /** Unix milliseconds, from the browser's clock at the moment of the poll. */
  t: number;
  waiting: number;
  active: number;
  admitted: number;
};

/** How many samples a sparkline draws. At a 2s poll, two minutes. */
export const SPARK_HISTORY = 60;

/**
 * How many samples are kept for export. An hour at a 2s poll, which is long
 * enough to cover a drop and still only tens of kilobytes a room.
 */
export const EXPORT_HISTORY = 1800;

/**
 * A new array every time, never a push: the panels are memoised on this array,
 * so mutating it in place would leave a sparkline frozen on its first sample.
 */
export function appendSample(series: Sample[] | undefined, sample: Sample): Sample[] {
  return [...(series ?? []), sample].slice(-EXPORT_HISTORY);
}

/**
 * The history as CSV. ISO-8601 timestamps because they sort as text and every
 * spreadsheet reads them, and `total_admitted` alongside the depth because
 * "the queue stayed at 800" means something different depending on whether
 * anyone was being let through at the time.
 */
export function toCSV(samples: Sample[]): string {
  const rows = ["timestamp,waiting,active,total_admitted"];
  for (const s of samples) {
    rows.push(`${new Date(s.t).toISOString()},${s.waiting},${s.active},${s.admitted}`);
  }
  return rows.join("\n") + "\n";
}

/** A filename that sorts by time and survives a room named awkwardly. */
export function csvFilename(room: string, when: Date = new Date()): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  const stamp =
    `${when.getFullYear()}${pad(when.getMonth() + 1)}${pad(when.getDate())}` +
    `-${pad(when.getHours())}${pad(when.getMinutes())}`;
  const safe = room.replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "") || "room";
  return `anteroom-${safe}-${stamp}.csv`;
}
