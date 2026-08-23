/**
 * The countdown shown while a scheduled room is still shut.
 *
 * It runs against an offset from the server's clock rather than the device's,
 * because a visitor whose laptop is a few minutes fast should not be told the
 * doors have opened when they have not.
 */
export class Countdown {
  private readonly el: HTMLElement;
  private target = 0;
  private skewMS = 0;
  private timer: number | undefined;
  private readonly onElapsed: () => void;

  constructor(el: HTMLElement, onElapsed: () => void) {
    this.el = el;
    this.onElapsed = onElapsed;
  }

  /**
   * @param targetMS when the doors open, in server time
   * @param serverNowMS the server's clock at the moment it sent targetMS
   */
  set(targetMS: number, serverNowMS: number): void {
    if (targetMS <= 0) return;
    this.target = targetMS;
    this.skewMS = serverNowMS - Date.now();
    this.start();
  }

  stop(): void {
    window.clearInterval(this.timer);
    this.timer = undefined;
  }

  private start(): void {
    if (this.timer !== undefined) return;
    this.tick();
    this.timer = window.setInterval(() => this.tick(), 1000);
  }

  private tick(): void {
    const remaining = this.target - (Date.now() + this.skewMS);
    if (remaining <= 0) {
      this.el.textContent = "any moment now";
      this.stop();
      this.onElapsed();
      return;
    }
    this.el.textContent = formatRemaining(remaining);
  }
}

/** Renders as h:mm:ss, dropping the hours until they matter. */
export function formatRemaining(ms: number): string {
  const total = Math.ceil(ms / 1000);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const seconds = total % 60;

  const pad = (n: number) => String(n).padStart(2, "0");
  if (hours > 0) return `${hours}:${pad(minutes)}:${pad(seconds)}`;
  return `${minutes}:${pad(seconds)}`;
}
