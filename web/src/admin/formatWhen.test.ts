import { afterEach, describe, expect, it, vi } from "vitest";
import { formatWhen } from "./components/RoomPanel";

// formatWhen is relative to now, so the clock is pinned rather than left to
// make the expectations drift with real time.
const NOW = new Date("2026-11-20T10:00:00Z").getTime();

afterEach(() => {
  vi.useRealTimers();
});

function at(offsetMs: number): string {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
  return formatWhen(NOW + offsetMs);
}

describe("formatWhen", () => {
  it("counts down in seconds when the doors are nearly open", () => {
    expect(at(30_000)).toMatch(/\(in 30s\)$/);
    expect(at(89_000)).toMatch(/\(in 89s\)$/);
  });

  it("switches to minutes once seconds stop being readable", () => {
    expect(at(90_000)).toMatch(/\(in 2 min\)$/);
    expect(at(30 * 60_000)).toMatch(/\(in 30 min\)$/);
  });

  // Past 90 minutes a countdown is noise; an operator wants the date instead.
  it("gives the full date once the countdown stops being useful", () => {
    const far = at(48 * 3_600_000);
    expect(far).not.toMatch(/\(in /);
    expect(far.startsWith("at ")).toBe(true);
  });

  // A time that has already passed must not render as a negative countdown.
  it("shows a bare clock time for anything already past", () => {
    expect(at(-60_000)).not.toMatch(/\(in /);
    expect(at(0)).not.toMatch(/\(in /);
  });
});
