import { useState } from "react";

type Props = {
  onSubmit: (token: string) => void;
  message: string | null;
};

/**
 * The dashboard holds no session of its own: the operator supplies the admin
 * token from the config file, and it is kept in sessionStorage so it does not
 * outlive the tab.
 */
export function TokenGate({ onSubmit, message }: Props) {
  const [token, setToken] = useState("");

  return (
    <main className="gate">
      <h1 className="gate__title">Control room</h1>
      <p className="gate__lede">
        Enter the admin token from your anteroom config to see the rooms and change how fast
        visitors are let through.
      </p>

      <form
        className="gate__form"
        onSubmit={(e) => {
          e.preventDefault();
          const trimmed = token.trim();
          if (trimmed !== "") onSubmit(trimmed);
        }}
      >
        <label className="field">
          <span className="field__label">Admin token</span>
          <input
            className="field__input field__input--wide"
            name="admin_token"
            type="password"
            autoComplete="current-password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
          />
        </label>
        <button type="submit" className="button button--primary" disabled={token.trim() === ""}>
          Open the control room
        </button>
      </form>

      {message === null ? null : (
        <p className="gate__error" role="alert">
          {message}
        </p>
      )}
    </main>
  );
}
