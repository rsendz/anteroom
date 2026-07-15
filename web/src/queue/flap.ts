/**
 * A split-flap display, the kind that clacks over on a station departure
 * board. It is here because a number that visibly turns over is the clearest
 * proof a queue is moving — which is the one thing a waiting visitor wants to
 * know, and the reason they would otherwise keep hitting reload.
 */

const FLIP_MS = 260;

export class FlapBoard {
  private readonly root: HTMLElement;
  private readonly digits: FlapDigit[] = [];
  private width: number;
  private value = -1;

  /** @param minWidth how many flaps to show even for a small number. */
  constructor(root: HTMLElement, minWidth = 2) {
    this.root = root;
    this.width = minWidth;
    // Clear the server-rendered number this display is taking over from.
    this.root.replaceChildren();
    this.root.setAttribute("role", "status");
    this.root.setAttribute("aria-live", "polite");
    this.setWidth(minWidth);
  }

  /** Renders `next`, turning over only the flaps whose digit changed. */
  set(next: number, animate = true): void {
    if (next === this.value) return;
    const text = String(Math.max(0, Math.trunc(next)));
    this.setWidth(Math.max(this.width, text.length));

    const padded = text.padStart(this.width, "0");
    for (let i = 0; i < this.width; i++) {
      this.digits[i]?.set(padded[i] ?? "0", animate);
    }
    this.value = next;
    // Screen readers get the number as a number, not as separate flaps.
    this.root.setAttribute("aria-label", `Position ${text} in line`);
  }

  private setWidth(width: number): void {
    while (this.digits.length < width) {
      const digit = new FlapDigit();
      // Prepend, so growing past a power of ten adds a leading flap.
      this.root.prepend(digit.el);
      this.digits.unshift(digit);
    }
    this.width = width;
  }
}

class FlapDigit {
  readonly el: HTMLElement;
  private readonly topFace: HTMLElement;
  private readonly bottomFace: HTMLElement;
  private current = "0";
  private timer: number | undefined;

  constructor() {
    this.el = element("div", "flap");
    this.topFace = face("top", this.current);
    this.bottomFace = face("bottom", this.current);
    this.el.append(this.topFace, this.bottomFace);
  }

  set(next: string, animate: boolean): void {
    if (next === this.current) return;
    const previous = this.current;
    this.current = next;

    if (!animate || prefersReducedMotion()) {
      setFace(this.topFace, next);
      setFace(this.bottomFace, next);
      return;
    }

    // A card falls: the old top half swings down to reveal the new one behind
    // it, then the new bottom half swings up over the old.
    window.clearTimeout(this.timer);
    this.el.querySelectorAll(".flap__leaf").forEach((leaf) => leaf.remove());

    const fallingTop = face("top", previous, "flap__leaf flap__leaf--top");
    const risingBottom = face("bottom", next, "flap__leaf flap__leaf--bottom");

    setFace(this.topFace, next);
    setFace(this.bottomFace, previous);
    this.el.append(fallingTop, risingBottom);

    // Let the leaves paint at their start angle before animating them.
    requestAnimationFrame(() => this.el.classList.add("flap--turning"));

    this.timer = window.setTimeout(() => {
      this.el.classList.remove("flap--turning");
      fallingTop.remove();
      risingBottom.remove();
      setFace(this.bottomFace, next);
    }, FLIP_MS);
  }
}

function element(tag: string, className: string): HTMLElement {
  const el = document.createElement(tag);
  el.className = className;
  return el;
}

/**
 * Builds one half of a flap. The glyph is drawn at the full height of the card
 * inside a half-height window, so the two halves stay in exact register and
 * the seam falls across the middle of the digit, where a real hinge would be.
 */
function face(half: "top" | "bottom", digit: string, extra = ""): HTMLElement {
  const el = element("div", `flap__face flap__face--${half} ${extra}`.trim());
  const glyph = element("span", "flap__glyph");
  glyph.textContent = digit;
  el.append(glyph);
  return el;
}

function setFace(el: HTMLElement, digit: string): void {
  const glyph = el.querySelector(".flap__glyph");
  if (glyph) glyph.textContent = digit;
}

function prefersReducedMotion(): boolean {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}
