import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { register } from "node:module";
import test from "node:test";

register("./test-utils/api-loader.mjs", import.meta.url);
process.env.NEXT_PUBLIC_MOCK = "0";
process.env.NEXT_PUBLIC_SSE_BASE = "";

const { api, http, sseUrl } = await import("./api.ts");
const { tasksApi } = await import("./api/tasks.ts");
const { chatApi } = await import("./api/chat.ts");
const { reportsApi } = await import("./api/reports.ts");

test("the compatibility entrypoint keeps the feature methods available", () => {
  const originalMethods = JSON.parse(readFileSync(new URL("./test-utils/api-methods.json", import.meta.url), "utf8"));
  assert.deepEqual(Object.keys(api).sort(), originalMethods);
  assert.equal(api.tasks, tasksApi.tasks);
  assert.equal(api.chatUpload, chatApi.chatUpload);
  assert.equal(api.report, reportsApi.report);
});

test("task responses still normalize null lists and preserve the active task", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url, init) => {
    calls.push({ url, init });
    return Response.json({ tasks: null, active: "12" });
  });
  assert.deepEqual(await api.tasks(), { tasks: [], active: "12" });
  assert.equal(calls[0].url, "/api/tasks");
});

test("attachment upload preserves multipart data and the task scope", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url, init) => {
    calls.push({ url, init });
    return Response.json({ attachments: [{ name: "note.txt", path: "uploads/note.txt", size: 5 }] });
  });
  const file = new File(["hello"], "note.txt", { type: "text/plain" });
  const result = await api.chatUpload("task", "12", [file]);
  assert.equal(result.attachments[0].name, "note.txt");
  assert.equal(calls[0].url, "/api/chat/upload?scope=task&id=12");
  assert.equal(calls[0].init.body.get("file").name, "note.txt");
  assert.equal(calls[0].init.headers.has("Content-Type"), false);
});

test("reports remain text and SSE tickets append safely to existing query strings", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url, init) => {
    calls.push({ url, init });
    if (url === "/api/sse-ticket") return Response.json({ ticket: "one time+ticket" });
    return new Response("# Report\n");
  });
  assert.equal(await api.report("12"), "# Report\n");
  assert.equal(calls[0].url, "/api/report?task=12");
  assert.equal(await sseUrl("/api/logs/stream?since=5"), "/api/logs/stream?since=5&ticket=one%20time%2Bticket");
  assert.equal(calls[1].init.method, "POST");
});

test("the public http helper still reports backend JSON errors", async (t) => {
  t.mock.method(globalThis, "fetch", async () => Response.json({ error: "保存できません" }, { status: 409 }));
  await assert.rejects(http("/tasks"), { message: "保存できません", status: 409 });
});

test("session polling requests forward their abort signal to fetch", async (t) => {
  const calls = [];
  t.mock.method(globalThis, "fetch", async (url, init) => {
    calls.push({ url, signal: init.signal });
    if (url.includes("/tokens")) return Response.json({ total: {}, sessions: null, workers: null });
    if (url.includes("/chat/status")) return Response.json({ running: false });
    if (url.includes("/llm/resolution")) return Response.json({});
    return Response.json({ items: [] });
  });
  const controller = new AbortController();
  await api.tokenStats("12", controller.signal);
  await api.chatStatus("12", controller.signal);
  await api.taskLLMResolution("12", controller.signal);
  await api.interceptTask("12", controller.signal);
  assert.deepEqual(
    calls.map((call) => call.url),
    [
      "/api/exploration/tokens?task=12",
      "/api/tasks/12/chat/status",
      "/api/tasks/12/llm/resolution",
      "/api/intercept/task/12",
    ],
  );
  assert.ok(calls.every((call) => call.signal === controller.signal));
});
