import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

register("./test-utils/api-loader.mjs", import.meta.url);
process.env.NEXT_PUBLIC_MOCK = "0";

const { api } = await import("./api.ts");

test("finding statistics pass the abort signal to the request", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (path, init) => {
    calls.push({ path, signal: init.signal });
    return Response.json({ total: 3, tasks: [], vulnclasses: [] });
  });
  const controller = new AbortController();
  assert.equal((await api.findingStats(controller.signal)).total, 3);
  assert.deepEqual(calls, [{ path: "/api/exploration/findings/stats", signal: controller.signal }]);
});
