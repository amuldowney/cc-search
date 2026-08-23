import assert from "node:assert/strict";
import test from "node:test";
import { CcSearchClient, CcSearchError } from "../src/index.ts";

test("search sends structured query parameters", async () => {
  let requested = "";
  const client = new CcSearchClient({
    baseUrl: "http://127.0.0.1:8765/",
    fetch: async (input) => {
      requested = String(input);
      return {
        status: 200,
        json: async () => ({
          results: [{ id: "m1", sessionId: "s1" }],
          total: 1,
          truncated: false,
          relaxed: false,
          budget: { limit: 60000, spent: 2, dropped: 0, shrunk: false },
        }),
      };
    },
  });

  const result = await client.search({ pattern: "auth flow", full: true, limit: 3 });
  assert.equal(new URL(requested).searchParams.get("pattern"), "auth flow");
  assert.equal(new URL(requested).searchParams.get("full"), "true");
  assert.equal(result.results[0]?.id, "m1");
});

test("HTTP errors are typed", async () => {
  const client = new CcSearchClient({
    fetch: async () => ({
      status: 404,
      json: async () => ({ version: 1, error: "missing", code: "not_found" }),
    }),
  });

  await assert.rejects(
    () => client.read("missing"),
    (error: unknown) =>
      error instanceof CcSearchError && error.status === 404 && error.code === "not_found",
  );
});
