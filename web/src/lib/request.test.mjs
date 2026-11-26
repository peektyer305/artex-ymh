import assert from "node:assert/strict";
import test from "node:test";
import { request } from "./request.ts";

function setup(responses) {
  const calls = [];
  let token = "old";
  let refreshes = 0;
  let logouts = 0;
  const dependencies = {
    fetch: async (path, init) => { calls.push({ path, init }); return responses.shift(); },
    token: () => token,
    refresh: async () => { refreshes++; token = "new"; return true; },
    unauthorized: () => { logouts++; },
  };
  return { calls, dependencies, counts: () => ({ refreshes, logouts }) };
}

test("JSON, text and binary requests refresh once and rebuild authorization", async () => {
  for (const path of ["/api/tasks", "/api/report", "/api/workspace/download?path=a"]) {
    const state = setup([new Response(null, { status: 401 }), new Response("ok")]);
    assert.equal(await (await request(path, {}, state.dependencies)).text(), "ok");
    assert.equal(state.calls[0].init.headers.get("Authorization"), "Bearer old");
    assert.equal(state.calls[1].init.headers.get("Authorization"), "Bearer new");
    assert.deepEqual(state.counts(), { refreshes: 1, logouts: 0 });
  }
});

test("FormData remains replayable and its boundary is left to fetch", async () => {
  const body = new FormData(); body.append("file", new Blob(["data"]), "a.txt");
  const state = setup([new Response(null, { status: 401 }), new Response("{}")]);
  await request("/api/chat/upload", { method: "POST", body }, state.dependencies);
  assert.equal(state.calls.length, 2);
  assert.equal(state.calls[1].init.body, body);
  assert.equal(state.calls[1].init.headers.has("Content-Type"), false);
});

test("auth endpoints do not recurse and repeated 401 logs out", async () => {
  const state = setup([new Response(null, { status: 401 })]);
  await assert.rejects(request("/api/auth/login", {}, state.dependencies), { status: 401 });
  assert.deepEqual(state.counts(), { refreshes: 0, logouts: 1 });
  const repeated = setup([new Response(null, { status: 401 }), new Response(null, { status: 401 })]);
  await assert.rejects(request("/api/tasks", {}, repeated.dependencies), { status: 401 });
  assert.deepEqual(repeated.counts(), { refreshes: 1, logouts: 1 });
});

test("server JSON errors are preserved and network errors are not retried", async () => {
  const state = setup([new Response(JSON.stringify({ error: "保存できません" }), { status: 409 })]);
  await assert.rejects(request("/api/tasks", {}, state.dependencies), { message: "保存できません", status: 409 });
  let calls = 0;
  state.dependencies.fetch = async () => { calls++; throw new Error("network"); };
  await assert.rejects(request("/api/tasks", { method: "POST" }, state.dependencies), /network/);
  assert.equal(calls, 1);
});
