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
  // The joined player's role; "admin" unlocks the setup wizard + admin actions.
  role: string;
  // True once the server has an imported pack (a published version exists).
  configured: boolean;
  // "1.20.1 · fabric" style summary for the dashboard header, when known.
  loader_summary: string | null;
  settings: Settings;
}

// --- Admin setup + dashboard payloads (returned by invoke commands) ---------

export interface SetupStep {
  phase: string;
  message: string;
  done: boolean;
  error: string | null;
  at: string | null;
}

export interface SetupPack {
  name: string;
  mc_version: string;
  loader: { id: string; version: string };
}

export interface SetupStatus {
  configured: boolean;
  running: boolean;
  phase: string;
  steps: SetupStep[];
  pack: SetupPack | null;
  invite_code: string | null;
  error: string;
}

export interface StartSetupPayload {
  description: string;
  claude_token: string;
  memory_mb?: number;
  server_dir?: string;
  server_port?: number;
  allow_curseforge?: boolean;
  curseforge_key?: string;
}

export interface ModEntry {
  name: string;
  path: string;
  side: string;
  platform: string;
  project_id: unknown;
  version: string;
}

export interface VersionEntry {
  number: number;
  created_at: string;
  summary: string;
  request_id: string | null;
  status: string | null;
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
