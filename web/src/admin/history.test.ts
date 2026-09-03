import { describe, expect, it } from "vitest";
import { appendSample, csvFilename, EXPORT_HISTORY, toCSV, type Sample } from "./history";

function sample(t: number, waiting = 0): Sample {
  return { t, waiting, active: 1, admitted: 2 };
}

describe("appendSample", () => {
  it("returns a new array rather than mutating the old one", () => {
    // The panels are memoised on this array: a push would leave a sparkline
    // frozen on its first sample, which shipped as a bug once.
    const first = [sample(1)];
    const second = appendSample(first, sample(2));
    expect(first).toHaveLength(1);
    expect(second).toHaveLength(2);
    expect(second).not.toBe(first);
  });

  it("starts a series when the room has no history yet", () => {
    expect(appendSample(undefined, sample(1))).toEqual([sample(1)]);
  });

  it("keeps the most recent samples once the buffer is full", () => {
    let series: Sample[] = [];
    for (let i = 0; i < EXPORT_HISTORY + 10; i++) {
      series = appendSample(series, sample(i));
    }
    expect(series).toHaveLength(EXPORT_HISTORY);
    expect(series[0]?.t).toBe(10);
    expect(series[series.length - 1]?.t).toBe(EXPORT_HISTORY + 9);
  });
});

describe("toCSV", () => {
  it("writes a header and one row per sample", () => {
    const csv = toCSV([sample(Date.parse("2026-11-20T10:00:00Z"), 42)]);
    expect(csv).toBe(
      "timestamp,waiting,active,total_admitted\n2026-11-20T10:00:00.000Z,42,1,2\n",
    );
  });

  it("writes just the header when nothing was recorded", () => {
    expect(toCSV([])).toBe("timestamp,waiting,active,total_admitted\n");
  });
});

describe("csvFilename", () => {
  it("stamps the room and the local time", () => {
    // Constructed from parts so the expectation does not depend on the zone
    // the tests happen to run in.
    const when = new Date(2026, 10, 20, 9, 5);
    expect(csvFilename("shop", when)).toBe("anteroom-shop-20261120-0905.csv");
  });

  it("keeps an awkward room name usable as a filename", () => {
    const when = new Date(2026, 10, 20, 9, 5);
    expect(csvFilename("shop/eu one", when)).toBe("anteroom-shop-eu-one-20261120-0905.csv");
    expect(csvFilename("///", when)).toBe("anteroom-room-20261120-0905.csv");
  });
});
