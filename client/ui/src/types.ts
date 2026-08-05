// Wire types mirroring the payloads the Rust core emits on the
// `state_changed`, `progress`, and `feed_updated` events. These are a contract
// with src-tauri/src/state.rs — keep the field names in sync.

export type Phase =
  | "FirstRun"
  | "Reconciling"
  | "UpToDate"
  | "UpdateAvailable"
  | "Applying"
  | "ApplyError"
  | "Offline";

export type Connection = "ok" | "connecting" | "error";

export interface InFlightRequest {
  player_name: string;
  text: string;
}

export interface Settings {
  server_url: string;
  player_name: string | null;
  minecraft_dir: string | null;
  autostart: boolean;
}

export interface UiState {
  phase: Phase;
  connection: Connection;
  pack_name: string;
  applied_version: number | null;
  current_version: number | null;
  update_summary: string | null;
  // Whether the Apply action is offered (false when a min-client upgrade is required).
  apply_enabled: boolean;
  error: string | null;
  // Who is currently blocking new requests (one in-flight request globally), if any.
  in_flight_request: InFlightRequest | null;
  // Non-null when the daemon requires a newer client than we are.
  min_client_notice: string | null;
  settings: Settings;
}

export interface Progress {
  bytes_done: number;
  bytes_total: number;
  files_done: number;
  files_total: number;
  message: string | null;
}

export type FeedItem =
  | {
      kind: "version";
      id: string;
      number: number;
      summary: string;
      created_at: string;
    }
  | {
      kind: "request";
      id: string;
      player_name: string;
      text: string;
      status: string;
      question: string | null;
      error: string | null;
      version: number | null;
      created_at: string;
      // True when the question is addressed to this player and needs an answer.
      awaiting_answer: boolean;
    };
