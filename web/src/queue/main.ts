/**
 * The waiting page. It holds one connection open to anteroom, turns the board
 * over as the queue moves, and reloads the moment the visitor is admitted.
 *
 * Every failure path ends in "keep waiting, reload soon" rather than an error:
 * a visitor who sees a broken page will reload by hand, and that is exactly
 * the traffic anteroom exists to absorb.
 */
import { FlapBoard } from "./flap";
import { Countdown } from "./countdown";
import type { Phase } from "../shared";
import "./queue.css";

type Update = {
  position: number;
  waiting: number;
  eta_secs: number;
  paused: boolean;
  phase: Phase;
  admits_at_ms?: number;
  now_ms: number;
};

type Connection = "live" | "reconnecting";

const RELOAD_JITTER_MS = 4000;

function main(): void {
  const root = document.getElementById("anteroom");
  if (!root) return;

  // The board and countdown are alternatives: a scheduled room that has not
  // opened shows time remaining, and only becomes a queue once it has.
  const boardEl = root.querySelector<HTMLElement>("[data-role='board']");
  const countdownEl = root.querySelector<HTMLElement>("[data-role='countdown']");
  const etaEl = requireEl(root, "[data-role='eta']");
  const waitingEl = requireEl(root, "[data-role='waiting']");
  const connectionEl = requireEl(root, "[data-role='connection']");
  const noticeEl = requireEl(root, "[data-role='notice']");

  const eventsPath = root.dataset.events ?? "/__anteroom/events";
  const refreshSeconds = Number(root.dataset.refresh ?? "10");
  const isLottery = root.dataset.lottery === "1";

  const board = boardEl ? new FlapBoard(boardEl) : null;
  const countdown = countdownEl
    ? new Countdown(countdownEl, () => {
        // The doors are due. Reload rather than guess: the server decides
        // whether the room has really opened.
        window.setTimeout(() => window.location.reload(), 1500);
      })
    : null;

  let phase: Phase = (root.dataset.phase as Phase) ?? "queueing";
  const initial: Update = {
    position: Number(root.dataset.position ?? "0"),
    waiting: Number(root.dataset.waiting ?? "0"),
    eta_secs: Number(root.dataset.eta ?? "0"),
    paused: root.dataset.paused === "1",
    phase,
    admits_at_ms: Number(root.dataset.admitsAt ?? "0"),
    now_ms: Number(root.dataset.now ?? "0") || Date.now(),
  };

  if (countdown && initial.admits_at_ms) {
    countdown.set(initial.admits_at_ms, initial.now_ms);
  }
  // The server already rendered the first position; show it without a flip so
  // the page does not appear to change the moment it loads.
  if (board && initial.position > 0) {
    board.set(initial.position, false);
    render(initial);
  }
  if (root.dataset.degraded === "1") {
    setNotice("We can't reach the queue right now. Your place is held — this page keeps trying.");
  }

  function render(update: Update): void {
    // Crossing out of a scheduled window changes the whole shape of the page,
    // so let the server render the new one rather than rebuilding it here.
    if (update.phase !== phase) {
      phase = update.phase;
      window.location.reload();
      return;
    }

    if (phase === "before" || phase === "draw") {
      if (countdown && update.admits_at_ms) {
        countdown.set(update.admits_at_ms, update.now_ms);
      }
      etaEl.textContent = update.waiting > 0 ? `${format(update.waiting)} already in` : "";
      waitingEl.textContent = isLottery ? "In the draw" : "Waiting to open";
      return;
    }

    if (board && update.position > 0) board.set(update.position);

    if (phase === "closed") {
      etaEl.textContent = "This room has closed.";
    } else if (update.paused) {
      etaEl.textContent = "Admissions are paused. You keep your place.";
    } else if (update.position === 1) {
      etaEl.textContent = "You're next.";
    } else if (update.eta_secs > 0) {
      etaEl.textContent = `About ${formatWait(update.eta_secs)} to go.`;
    } else {
      etaEl.textContent = "Working out how long this will take.";
    }

    waitingEl.textContent = update.waiting > 0 ? `${format(update.waiting)} waiting` : "";
  }

  function setConnection(state: Connection): void {
    connectionEl.dataset.state = state;
    connectionEl.textContent = state === "live" ? "Live" : "Reconnecting";
  }

  function setNotice(message: string): void {
    noticeEl.textContent = message;
    noticeEl.hidden = message === "";
  }

  // If the stream cannot be kept up, fall back to reloading the page. The
  // jitter keeps a crowd of waiting visitors from returning in lockstep.
  let reloadTimer: number | undefined;
  function scheduleReload(): void {
    if (reloadTimer !== undefined) return;
    const delay = refreshSeconds * 1000 + Math.random() * RELOAD_JITTER_MS;
    reloadTimer = window.setTimeout(() => window.location.reload(), delay);
  }
  function cancelReload(): void {
    window.clearTimeout(reloadTimer);
    reloadTimer = undefined;
  }

  const source = new EventSource(eventsPath);

  source.addEventListener("open", () => {
    setConnection("live");
    setNotice("");
    cancelReload();
  });

  source.addEventListener("position", (event) => {
    setConnection("live");
    cancelReload();
    const update = parse(event);
    if (update) render(update);
  });

  source.addEventListener("stalled", () => {
    setNotice("We can't reach the queue right now. Your place is held — this page keeps trying.");
  });

  source.addEventListener("admitted", () => {
    source.close();
    cancelReload();
    countdown?.stop();
    setConnection("live");
    etaEl.textContent = "You're in. Taking you through…";
    boardEl?.classList.add("board--through");
    // A short beat so the message registers before the page changes.
    window.setTimeout(() => window.location.reload(), 600);
  });

  source.addEventListener("error", () => {
    // EventSource retries on its own; the reload is the backstop for when it
    // cannot, such as a proxy that refuses to hold the connection open.
    setConnection("reconnecting");
    scheduleReload();
  });
}

function parse(event: Event): Update | null {
  if (!(event instanceof MessageEvent)) return null;
  try {
    return JSON.parse(event.data) as Update;
  } catch {
    return null;
  }
}

function requireEl(root: ParentNode, selector: string): HTMLElement {
  const el = root.querySelector<HTMLElement>(selector);
  if (!el) throw new Error(`anteroom: missing element ${selector}`);
  return el;
}

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

function format(n: number): string {
  return n.toLocaleString();
}

main();
