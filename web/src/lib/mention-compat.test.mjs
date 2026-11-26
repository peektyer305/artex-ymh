import assert from "node:assert/strict";
import test from "node:test";
import { mentionToken, selectedMentions, mentionSearch } from "./chat-mentions.ts";
test("mention storage uses stable keys and recognizes both older display formats", () => {
  assert.equal(mentionToken({kind:"finding",id:12,label:"example",description:""}), "@[finding#12 example]");
  for (const value of ["@[finding#12 example]", "@[ファインディング#12 example]", "@[漏洞#12 example]"]) {
    assert.equal(selectedMentions(value)[0].label,"ファインディング #12 · example");
  }
  assert.equal(mentionSearch("ファインディング SQL").kind, "finding");
  assert.equal(mentionSearch("漏洞SQL").query, "SQL");
  assert.equal(selectedMentions("@[unknown#1 example]").length,0);
});
