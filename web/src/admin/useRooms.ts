import { useCallback, useEffect, useRef, useState } from "react";
import { Api, ApiError, type Room, type Status } from "./api";
import { appendSample, type Sample } from "./history";

const POLL_MS = 2000;

export type RoomHistory = Map<string, Sample[]>;

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
  // it, and the setRooms below re-renders them on the same tick.
  const history = useRef<RoomHistory>(new Map());
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

        const t = Date.now();
        for (const room of next) {
          // appendSample returns a new array rather than pushing: the panels
          // are memoised on this prop, so mutating it in place would leave
          // them rendering the first sample forever.
          history.current.set(
            room.room,
            appendSample(history.current.get(room.room), {
              t,
              waiting: room.waiting,
              active: room.active,
              admitted: room.total_admitted,
            }),
          );
        }
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
