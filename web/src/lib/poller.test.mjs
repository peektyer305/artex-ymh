import assert from "node:assert/strict";
import test from "node:test";
import { createPoller } from "./poller.ts";
test("manual and scheduled refreshes share the in-flight request", async () => {
  let calls=0, release;
  const gate=new Promise(resolve => {release=resolve}); const values=[];
  const poller=createPoller(async()=>{calls++; await gate; return calls},x=>values.push(x),0);
  const first=poller.refresh(), second=poller.refresh();
  assert.equal(first,second); await Promise.resolve(); assert.equal(calls,1);
  release(); await first; assert.deepEqual(values,[1]);
  await poller.refresh(); assert.equal(calls,2); poller.stop();
});
test("stop aborts the request and discards late results", async () => {
  let release,signal;const gate=new Promise(resolve=>{release=resolve});const values=[];
  const poller=createPoller(async s=>{signal=s; await gate;return 42},x=>values.push(x),0);
  const pending=poller.refresh();await Promise.resolve();poller.stop();
  assert.equal(signal.aborted,true);release();await pending;assert.deepEqual(values,[]);
});
test("a failed request reports the error and permits the next refresh", async () => {
  let calls=0;const errors=[],values=[];
  const poller=createPoller(async()=>{if (++calls===1)throw new Error("offline");return 3},x=>values.push(x),0,e=>errors.push(e));
  await poller.refresh();await poller.refresh();poller.stop();
  assert.equal(errors.length,1);assert.deepEqual(values,[3]);
});
