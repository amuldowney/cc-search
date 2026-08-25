// Generated from openapi/cc-search.json by scripts/generate-clients.mjs.
export interface Message {
  id: string;
  sessionId: string;
  timestamp: string;
  type: string;
  activityId?: string;
  activityRole?: "start" | "response" | "terminal" | "child";
  parentActivityId?: string;
  parentSessionId?: string;
  childSessionId?: string;
  preview?: string;
  content?: string;
  charCount: number;
}

export interface Budget {
  limit: number;
  spent: number;
  dropped: number;
  shrunk: boolean;
}

export interface MessageResponse {
  results: Message[];
  total: number;
  truncated: boolean;
  relaxed: boolean;
  budget: Budget;
}

export interface Health {
  version: number;
  status: "ok";
}

export interface RebuildResponse {
  version: number;
  sessionsIndexed: number;
  messagesIndexed: number;
  filesSkipped: number;
}

export interface SearchParams {
  pattern: string;
  limit?: number;
  window_messages?: number;
  window_hours?: number;
  session?: string;
  type?: string;
  include_current?: boolean;
  any?: boolean;
  raw?: boolean;
  all?: boolean;
  full?: boolean;
  preview_length?: number;
  budget?: number;
}

export interface LastParams {
  count?: number;
  hours?: number;
  session?: string;
  type?: string;
  all?: boolean;
  full?: boolean;
  preview_length?: number;
  budget?: number;
}

export interface ReadParams {
  before?: number;
  after?: number;
  all?: boolean;
  full?: boolean;
  preview_length?: number;
  budget?: number;
}

export interface CcSearchClientOptions {
  baseUrl?: string;
  fetch?: FetchLike;
}

export type FetchLike = (
  input: string,
  init?: { method?: string; headers?: Record<string, string> },
) => Promise<{ status: number; json(): Promise<unknown> }>;

export class CcSearchError extends Error {
  readonly status: number;
  readonly code?: string;
  readonly body: unknown;

  constructor(status: number, message: string, code?: string, body?: unknown) {
    super(message);
    this.name = "CcSearchError";
    this.status = status;
    this.code = code;
    this.body = body;
  }
}

export class CcSearchClient {
  private readonly baseUrl: string;
  private readonly fetchImpl: FetchLike;

  constructor(options: CcSearchClientOptions = {}) {
    this.baseUrl = (options.baseUrl ?? "http://127.0.0.1:8765").replace(/\/$/, "");
    const fetchImpl = options.fetch ?? globalThis.fetch?.bind(globalThis);
    if (!fetchImpl) {
      throw new Error("cc-search client requires global fetch or an injected fetch implementation");
    }
    this.fetchImpl = fetchImpl;
  }

  async health(): Promise<Health> {
    return this.request<Health>("GET", "/v1/health");
  }

  async openapi(): Promise<Record<string, unknown>> {
    return this.request<Record<string, unknown>>("GET", "/openapi.json");
  }

  async search(params: SearchParams): Promise<MessageResponse> {
    return this.request<MessageResponse>("GET", "/v1/search", params);
  }

  async last(params: LastParams = {}): Promise<MessageResponse> {
    return this.request<MessageResponse>("GET", "/v1/last", params);
  }

  async read(id: string, params: ReadParams = {}): Promise<MessageResponse> {
    return this.request<MessageResponse>("GET", "/v1/read", { id, ...params });
  }

  async rebuild(session?: string): Promise<RebuildResponse> {
    return this.request<RebuildResponse>("POST", "/v1/rebuild", session === undefined ? {} : { session });
  }

  private async request<T>(
    method: string,
    path: string,
    params: Record<string, string | number | boolean | undefined> = {},
  ): Promise<T> {
    const url = new URL(path, `${this.baseUrl}/`);
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined) url.searchParams.set(key, String(value));
    }
    const response = await this.fetchImpl(url.toString(), {
      method,
      headers: { accept: "application/json" },
    });
    const body = await response.json();
    if (response.status < 200 || response.status >= 300) {
      const errorBody = body as { error?: unknown; code?: unknown };
      throw new CcSearchError(
        response.status,
        typeof errorBody.error === "string" ? errorBody.error : `cc-search returned HTTP ${response.status}`,
        typeof errorBody.code === "string" ? errorBody.code : undefined,
        body,
      );
    }
    return body as T;
  }
}
