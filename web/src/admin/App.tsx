import { useCallback, useEffect, useMemo, useState } from "react";
import { Api, ApiError } from "./api";
import { useRooms } from "./useRooms";
import { RoomPanel } from "./components/RoomPanel";
import { TokenGate } from "./components/TokenGate";

const TOKEN_KEY = "anteroom.admin-token";

export function App({ apiPath }: { apiPath: string }) {
  const [token, setToken] = useState(() => sessionStorage.getItem(TOKEN_KEY) ?? "");
  const [gateMessage, setGateMessage] = useState<string | null>(null);

  const signIn = useCallback((next: string) => {
    sessionStorage.setItem(TOKEN_KEY, next);
    setGateMessage(null);
    setToken(next);
  }, []);

  const signOut = useCallback((message: string | null) => {
    sessionStorage.removeItem(TOKEN_KEY);
    setGateMessage(message);
    setToken("");
  }, []);

  if (token === "") {
    return <TokenGate onSubmit={signIn} message={gateMessage} />;
  }
  // Keyed on the token so switching credentials rebuilds the polling state
  // rather than mixing results from two sessions.
  return <Dashboard key={token} apiPath={apiPath} token={token} onSignOut={signOut} />;
}

function Dashboard({
  apiPath,
  token,
  onSignOut,
}: {
  apiPath: string;
  token: string;
  onSignOut: (message: string | null) => void;
}) {
  const api = useMemo(() => new Api(apiPath, token), [apiPath, token]);
  const { rooms, history, status, error, unauthorized, loading, refresh } = useRooms(api);
  const [busyRoom, setBusyRoom] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  // A rejected token means the operator has to supply a new one; sending them
  // back to the gate is more useful than an error they cannot act on.
  useEffect(() => {
    if (unauthorized) {
      onSignOut("That token was not accepted. Check admin_token in your config.");
    }
  }, [unauthorized, onSignOut]);

  const act = useCallback(
    async (room: string, run: () => Promise<unknown>) => {
      setBusyRoom(room);
      setActionError(null);
      try {
        await run();
        refresh();
      } catch (err) {
        setActionError(err instanceof ApiError ? err.message : "That change did not go through.");
      } finally {
        setBusyRoom(null);
      }
    },
    [refresh],
  );

  const handlePause = useCallback(
    (room: string, paused: boolean) => void act(room, () => api.setPaused(room, paused)),
    [act, api],
  );
  const handleFlush = useCallback(
    (room: string) => void act(room, () => api.flush(room)),
    [act, api],
  );
  const handleConfig = useCallback(
    (room: string, rate: number, maxActive: number) =>
      void act(room, () => api.setConfig(room, { rate, max_active: maxActive })),
    [act, api],
  );

  const totalWaiting = rooms.reduce((sum, room) => sum + room.waiting, 0);

  return (
    <div className="shell">
      <header className="masthead">
        <p className="sign">
          <span className="sign__mark">&#9679;</span>
          <span>Anteroom</span>
        </p>
        <h1 className="masthead__title">Control room</h1>
        <p className="masthead__summary">
          {loading
            ? "Reading the rooms…"
            : `${rooms.length} ${rooms.length === 1 ? "room" : "rooms"} · ${totalWaiting.toLocaleString()} waiting`}
        </p>
        <button type="button" className="button button--quiet" onClick={() => onSignOut(null)}>
          Forget token
        </button>
      </header>

      {/* The loudest thing on the page when it applies: an operator must
          never discover by accident that their protection is off. */}
      {status !== null && status.failing_open ? (
        <p className="banner banner--alarm" role="alert">
          <strong>Letting everyone through.</strong> {status.message} Unreachable for{" "}
          {status.unhealthy_secs}s.
        </p>
      ) : null}
      {status !== null && !status.queue_healthy && !status.failing_open ? (
        <p className="banner" role="alert">
          {status.message} Unreachable for {status.unhealthy_secs}s
          {status.fail_open_enabled
            ? `; letting visitors through after ${status.fail_open_after_secs}s.`
            : "."}
        </p>
      ) : null}

      {error === null ? null : (
        <p className="banner" role="alert">
          {error}
        </p>
      )}
      {actionError === null ? null : (
        <p className="banner" role="alert">
          {actionError}
        </p>
      )}

      {!loading && rooms.length === 0 ? (
        <p className="empty">No rooms are configured. Add one to your anteroom config and restart.</p>
      ) : null}

      <div className="panels">
        {rooms.map((room) => (
          <RoomPanel
            key={room.room}
            room={room}
            history={history.get(room.room) ?? []}
            busy={busyRoom === room.room}
            onPause={handlePause}
            onFlush={handleFlush}
            onConfig={handleConfig}
          />
        ))}
      </div>
    </div>
  );
}
