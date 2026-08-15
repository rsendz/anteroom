import { memo, useState } from "react";
import type { Room } from "../api";
import { Sparkline } from "./Sparkline";

type Props = {
  room: Room;
  history: number[];
  busy: boolean;
  onPause: (room: string, paused: boolean) => void;
  onFlush: (room: string) => void;
  onConfig: (room: string, rate: number, maxActive: number) => void;
};

export const RoomPanel = memo(function RoomPanel({
  room,
  history,
  busy,
  onPause,
  onFlush,
  onConfig,
}: Props) {
  const [confirmingFlush, setConfirmingFlush] = useState(false);

  return (
    <section className="panel" aria-labelledby={`room-${room.room}`}>
      <header className="panel__head">
        <div>
          <h2 className="panel__name" id={`room-${room.room}`}>
            {room.room}
          </h2>
          <p className="panel__route">
            {room.match_host === "" ? "any host" : room.match_host}
            <span className="panel__arrow"> &#8594; </span>
            {room.origin}
          </p>
        </div>
        <span className={`badge ${room.paused ? "badge--paused" : "badge--open"}`}>
          {room.paused ? "Paused" : "Admitting"}
        </span>
      </header>

      <div className="tiles">
        <Tile label="Waiting" value={format(room.waiting)} />
        <Tile label="On the site" value={`${format(room.active)} / ${format(room.max_active)}`} />
        <Tile label="Let in per second" value={formatRate(room.rate)} />
        <Tile label="Admitted so far" value={format(room.total_admitted)} />
      </div>

      <Sparkline values={history} label={`${room.room} queue depth`} />

      <dl className="ledger">
        <Ledger label="Joined" value={room.total_joined} />
        <Ledger label="Gave up" value={room.total_abandoned} />
        <Ledger label="Sessions ended" value={room.total_expired} />
        <Ledger label="Idle limit" value={`${room.session_ttl_secs}s`} />
      </dl>

      {/* Refusals are worth calling out rather than burying in the ledger: a
          climbing count during ordinary traffic means the per-address limit is
          turning real people away, which is invisible from the origin. */}
      {room.total_refused > 0 ? (
        <p className="warn">
          <strong>{format(room.total_refused)}</strong> turned away by the limit of{" "}
          {format(room.join_limit)} joins per address every {room.join_window_secs}s. If these are
          real visitors behind an office or mobile network, raise{" "}
          <code>join_limit_per_ip</code>.
        </p>
      ) : null}

      {/* Keyed on the live settings: when they change (here or from another
          operator's browser) the inputs reset to what the room is actually
          doing, rather than showing a stale edit. */}
      <RateForm
        key={`${room.rate}:${room.max_active}`}
        room={room}
        busy={busy}
        onSubmit={onConfig}
      />

      <div className="panel__actions">
        <button
          type="button"
          className="button"
          disabled={busy}
          onClick={() => onPause(room.room, !room.paused)}
        >
          {room.paused ? "Resume admissions" : "Pause admissions"}
        </button>

        {confirmingFlush ? (
          <span className="confirm">
            <span className="confirm__question">
              Empty the queue? {format(room.waiting)} waiting will be sent away.
            </span>
            <button
              type="button"
              className="button button--danger"
              disabled={busy}
              onClick={() => {
                onFlush(room.room);
                setConfirmingFlush(false);
              }}
            >
              Empty it
            </button>
            <button type="button" className="button button--quiet" onClick={() => setConfirmingFlush(false)}>
              Keep it
            </button>
          </span>
        ) : (
          <button
            type="button"
            className="button button--quiet"
            disabled={busy || room.waiting === 0}
            onClick={() => setConfirmingFlush(true)}
          >
            Empty the queue
          </button>
        )}
      </div>
    </section>
  );
});

function RateForm({
  room,
  busy,
  onSubmit,
}: {
  room: Room;
  busy: boolean;
  onSubmit: (room: string, rate: number, maxActive: number) => void;
}) {
  const [rate, setRate] = useState(String(room.rate));
  const [maxActive, setMaxActive] = useState(String(room.max_active));

  const rateValue = Number(rate);
  const maxValue = Number(maxActive);
  const valid = rateValue > 0 && maxValue > 0 && Number.isFinite(rateValue) && Number.isFinite(maxValue);
  const changed = rateValue !== room.rate || maxValue !== room.max_active;

  return (
    <form
      className="rate-form"
      onSubmit={(e) => {
        e.preventDefault();
        if (valid) onSubmit(room.room, rateValue, maxValue);
      }}
    >
      <label className="field">
        <span className="field__label">Let in per second</span>
        <input
          className="field__input"
          type="number"
          min="0.1"
          step="0.1"
          value={rate}
          onChange={(e) => setRate(e.target.value)}
        />
      </label>
      <label className="field">
        <span className="field__label">Allowed on the site</span>
        <input
          className="field__input"
          type="number"
          min="1"
          step="1"
          value={maxActive}
          onChange={(e) => setMaxActive(e.target.value)}
        />
      </label>
      <button type="submit" className="button button--primary" disabled={busy || !valid || !changed}>
        Apply
      </button>
      {changed ? (
        <button
          type="button"
          className="button button--quiet"
          onClick={() => {
            setRate(String(room.rate));
            setMaxActive(String(room.max_active));
          }}
        >
          Reset
        </button>
      ) : null}
    </form>
  );
}

function Tile({ label, value }: { label: string; value: string }) {
  return (
    <div className="tile">
      <span className="tile__value">{value}</span>
      <span className="tile__label">{label}</span>
    </div>
  );
}

function Ledger({ label, value }: { label: string; value: number | string }) {
  return (
    <div className="ledger__row">
      <dt>{label}</dt>
      <dd>{typeof value === "number" ? format(value) : value}</dd>
    </div>
  );
}

function format(n: number): string {
  return n.toLocaleString();
}

function formatRate(rate: number): string {
  return rate >= 10 ? String(Math.round(rate)) : String(rate);
}
