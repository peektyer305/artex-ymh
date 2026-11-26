import {
  emptyState,
  intentToSession,
  keyForSession,
  mainSessionId,
  mainSessionKey,
  mergeBySeq,
  sessionKeyOf,
  workerMessageCharCount,
} from "../app/(main)/function/tasks/detail/_tabs/_lib/session-model.ts";
import assert from "node:assert/strict";
import test from "node:test";

const activity = (seq, extra = {}) => ({
  seq,
  worker: "mainagent",
  ts: "2026-11-26T08:00:00Z",
  kind: "text",
  summary: String(seq),
  ...extra,
});

test("session routing keeps main segments, planner and workers separate", () => {
  assert.equal(sessionKeyOf(activity(1, { main_seg: 3 })), "main:3");
  assert.equal(sessionKeyOf(activity(2, { worker: "planner" })), "plan");
  assert.equal(sessionKeyOf(activity(3, { worker: "work#2", intent_id: "14" })), "intent:14");
  assert.equal(sessionKeyOf(activity(4, { kind: "llm_failover" })), "system");
  assert.equal(keyForSession({ id: mainSessionId(3), role: "mainagent", seg: 3 }), mainSessionKey(3));
});

test("history and live records merge without losing or duplicating messages", () => {
  const history = [activity(1), activity(2)];
  const live = [activity(2, { summary: "updated" }), activity(3)];
  const merged = mergeBySeq(history, live);
  assert.deepEqual(
    merged.map((item) => item.seq),
    [1, 2, 3],
  );
  assert.equal(merged[1].summary, "updated");
  assert.equal(history[1].summary, "2");
  assert.deepEqual(mergeBySeq(merged, live), merged);
});

test("new session caches are independent and count Unicode characters correctly", () => {
  const first = emptyState(),
    second = emptyState();
  first.items.push(activity(1));
  assert.equal(second.items.length, 0);
  assert.equal(workerMessageCharCount("あ😀A"), 3);
});

test("worker session state follows the persisted intent state", () => {
  const session = intentToSession({
    id: "12",
    state: "paused",
    payload: '{"summary":"example"}',
    ts: "2026-11-26T08:00:00Z",
  });
  assert.equal(session.status, "paused");
  assert.equal(session.intent_id, "12");
  assert.equal(keyForSession(session), "intent:12");
});
