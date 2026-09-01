import { describe, expect, it } from "vitest";
import { formatRemaining } from "./countdown";
import { formatWait } from "./format";

describe("formatRemaining", () => {
  it("drops the hours until they matter", () => {
    expect(formatRemaining(45_000)).toBe("0:45");
    expect(formatRemaining(90_000)).toBe("1:30");
    expect(formatRemaining(3_600_000)).toBe("1:00:00");
    expect(formatRemaining(3_725_000)).toBe("1:02:05");
  });

  it("pads so the digits never jump around as the clock runs", () => {
    expect(formatRemaining(65_000)).toBe("1:05");
    expect(formatRemaining(3_665_000)).toBe("1:01:05");
  });

  // Rounding up matters: showing 0:00 while there is still time left would
  // tell a visitor the doors are open when they are not.
  it("rounds part-seconds up", () => {
    expect(formatRemaining(1)).toBe("0:01");
    expect(formatRemaining(1_500)).toBe("0:02");
    expect(formatRemaining(0)).toBe("0:00");
  });
});

describe("formatWait", () => {
  it("rounds short waits to a nearby five seconds", () => {
    expect(formatWait(3)).toBe("5 seconds");
    expect(formatWait(12)).toBe("15 seconds");
    expect(formatWait(59)).toBe("60 seconds");
  });

  it("says a minute rather than 1 minutes", () => {
    expect(formatWait(60)).toBe("a minute");
    expect(formatWait(61)).toBe("2 minutes");
    expect(formatWait(600)).toBe("10 minutes");
  });

  it("switches to hours once minutes stop being useful", () => {
    expect(formatWait(3_600)).toBe("an hour");
    expect(formatWait(9_000)).toBe("2.5 hours");
  });

  // Never promises sooner than five seconds, even at zero: the caller decides
  // whether to show an estimate at all, and this only phrases one.
  it("floors at five seconds rather than promising none", () => {
    expect(formatWait(0)).toBe("5 seconds");
  });
});
