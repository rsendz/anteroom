/** Typed client for the anteroom admin API. */

export type Snapshot = {
  room: string;
  waiting: number;
  active: number;
  rate: number;
  max_active: number;
  session_ttl_secs: number;
  abandon_after_secs: number;
  paused: boolean;
  total_joined: number;
  total_admitted: number;
  total_expired: number;
  total_abandoned: number;
  total_refused: number;
  join_limit: number;
  join_window_secs: number;
};

export type Room = Snapshot & {
  match_host: string;
  origin: string;
};

export type ConfigPatch = {
  rate?: number;
  max_active?: number;
  session_ttl_secs?: number;
  abandon_after_secs?: number;
};

/** Thrown for any non-2xx response, carrying the server's own message. */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }

  /** True when the token was rejected, so the caller can re-prompt for it. */
  get unauthorized(): boolean {
    return this.status === 401;
  }
}

/** Server-side health, answerable even when the queue store is unreachable. */
export type Status = {
  queue_healthy: boolean;
  fail_open_enabled: boolean;
  failing_open: boolean;
  unhealthy_secs: number;
  fail_open_after_secs: number;
  message?: string;
};

export class Api {
  constructor(
    private readonly base: string,
    private readonly token: string,
  ) {}

  listRooms(signal?: AbortSignal): Promise<{ rooms: Room[] }> {
    return this.request("rooms", { signal });
  }

  status(signal?: AbortSignal): Promise<Status> {
    return this.request("status", { signal });
  }

  setConfig(room: string, patch: ConfigPatch): Promise<Snapshot> {
    return this.request(`rooms/${encodeURIComponent(room)}/config`, {
      method: "PUT",
      body: JSON.stringify(patch),
    });
  }

  setPaused(room: string, paused: boolean): Promise<Snapshot> {
    const action = paused ? "pause" : "resume";
    return this.request(`rooms/${encodeURIComponent(room)}/${action}`, { method: "POST" });
  }

  flush(room: string): Promise<{ removed: number }> {
    return this.request(`rooms/${encodeURIComponent(room)}/flush`, { method: "POST" });
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const response = await fetch(this.base + path, {
      ...init,
      headers: {
        Authorization: `Bearer ${this.token}`,
        "Content-Type": "application/json",
        ...init.headers,
      },
    });

    if (!response.ok) {
      throw new ApiError(await errorMessage(response), response.status);
    }
    return (await response.json()) as T;
  }
}

async function errorMessage(response: Response): Promise<string> {
  if (response.status === 401) return "That token was not accepted.";
  try {
    const body = (await response.json()) as { error?: string };
    if (body.error) return body.error;
  } catch {
    // Fall through to the status text below.
  }
  return `Request failed (${response.status})`;
}
