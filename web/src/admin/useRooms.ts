import { useCallback, useEffect, useRef, useState } from "react";
import { Api, ApiError, type Room, type Status } from "./api";

/** How many samples the sparklines keep. At 2s a poll, this is two minutes. */
const HISTORY = 60;
const POLL_MS = 2000;

export type RoomHistory = Map<string, number[]>;

export type RoomsState = {
  rooms: Room[];
  history: RoomHistory;
  status: Status | null;
  error: string | null;
  unauthorized: boolean;
  loading: boolean;
  refresh: () => void;
};

/**
 * Polls the room list on a timer. Each cycle is scheduled after the previous
 * one finishes rather than on an interval, so a slow or hanging request never
 * stacks up behind itself.
 */
export function useRooms(api: Api): RoomsState {
  const [rooms, setRooms] = useState<Room[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [unauthorized, setUnauthorized] = useState(false);
  const [loading, setLoading] = useState(true);

  // History is a ref: it changes on every poll but only the sparklines read
  // it, and they re-render anyway when `rooms` updates.
  const history = useRef<RoomHistory>(new Map());
  const [, bumpHistory] = useState(0);
  const timer = useRef<number | undefined>(undefined);

  const [status, setStatus] = useState<Status | null>(null);

  const poll = useCallback(
    async (signal: AbortSignal) => {
      // Health first, and separately: it is answerable while the queue store
      // is down, which is exactly when someone is watching this screen.
      try {
        const health = await api.status(signal);
        if (!signal.aborted) setStatus(health);
      } catch {
        // Reported through the room error below.
      }

      try {
        const { rooms: next } = await api.listRooms(signal);
        if (signal.aborted) return;

        for (const room of next) {
          const series = history.current.get(room.room) ?? [];
          series.push(room.waiting);
          if (series.length > HISTORY) series.shift();
          history.current.set(room.room, series);
        }
        bumpHistory((n) => n + 1);
        setRooms(next);
        setError(null);
        setUnauthorized(false);
      } catch (err) {
        if (signal.aborted) return;
        if (err instanceof ApiError && err.unauthorized) {
          setUnauthorized(true);
        }
        setError(err instanceof Error ? err.message : "Could not reach anteroom.");
      } finally {
        if (!signal.aborted) setLoading(false);
      }
    },
    [api],
  );

  useEffect(() => {
    const controller = new AbortController();
    let stopped = false;

    const cycle = async () => {
      await poll(controller.signal);
      if (stopped || controller.signal.aborted) return;
      timer.current = window.setTimeout(cycle, POLL_MS);
    };
    void cycle();

    return () => {
      stopped = true;
      controller.abort();
      window.clearTimeout(timer.current);
    };
  }, [poll]);

  const refresh = useCallback(() => {
    const controller = new AbortController();
    void poll(controller.signal);
  }, [poll]);

  return { rooms, history: history.current, status, error, unauthorized, loading, refresh };
}
