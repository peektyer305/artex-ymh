import assert from "node:assert/strict";
import test from "node:test";
import { fmtBytes, fmtTokens, fmtDurationSeconds, fmtDurationMs, fmtTime } from "./format.ts";
test("formatting handles boundaries and invalid data", () => {
  assert.equal(fmtBytes(0), "0 B"); assert.equal(fmtBytes(1024), "1.0 KB");
  assert.equal(fmtBytes(-1), "—"); assert.equal(fmtBytes(null), "—");
  assert.equal(fmtTokens(1234), "1.2k"); assert.equal(fmtTokens(2000000), "2.0M");
  assert.equal(fmtTime("not-a-date"), "—"); assert.equal(fmtTime(null), "—");
});
test("seconds and milliseconds have explicit, equivalent units", () => {
  assert.equal(fmtDurationSeconds(90), "1m 30s");
  assert.equal(fmtDurationMs(90000), fmtDurationSeconds(90));
  assert.equal(fmtDurationSeconds(0), "0s"); assert.equal(fmtDurationMs(-1), "—");
});
