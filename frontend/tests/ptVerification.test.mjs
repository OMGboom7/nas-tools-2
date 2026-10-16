import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getPTVerification, reviewPTVerification, applyPTVerification } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const input = { downloaderId: 1, taskId: "a".repeat(40), mode: "attest", minutes: 15, note: "已在站点账户逐项核实本任务全部义务完成", actions: { stop: true, remove: false, move: false } };
const review = { ...input, fingerprint: "b".repeat(64), reviewExpires: new Date(Date.now() + 240000).toISOString(), evidence: { hash: input.taskId, addedAt: 1700000000 }, current: { record: null, revoked: false } };
test("PT manual verification reads and reviews without writing or executing any task", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++; if (calls === 1) { assert.equal(path, `/api/v1/downloads/pt-verification?downloaderId=1&taskId=${input.taskId}`); assert.equal(options.method, undefined); }
    else { assert.equal(path, "/api/v1/downloads/pt-verification/review"); assert.deepEqual(JSON.parse(options.body), input); }
    return new Response(JSON.stringify({ success: true, code: 0, data: calls === 1 ? { record: null, revoked: false } : review }));
  });
  await getPTVerification("token", input); await reviewPTVerification("token", input); assert.equal(calls, 2);
});
test("PT operator attestation requires four independent consents without editable thresholds", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/downloads/pt-verification/record"); assert.deepEqual(JSON.parse(options.body), { ...input, fingerprint: review.fingerprint, confirm: true, confirmAccountReviewed: true, confirmAutomationScope: true, confirmOldExecutorsStopped: true }); return new Response(JSON.stringify({ success: true, code: 0 })); });
  await assert.rejects(applyPTVerification("token", review)); await assert.rejects(applyPTVerification("token", review, true)); await assert.rejects(applyPTVerification("token", review, true, true)); await assert.rejects(applyPTVerification("token", review, true, true, true)); assert.equal(calls, 0);
  await applyPTVerification("token", review, true, true, true, true); assert.equal(calls, 1);
});
test("PT revoke is explicit, does not delete original obligations or execute a task", async (t) => {
  const revoke = { ...review, mode: "revoke", minutes: 0, note: "", actions: { stop: false, remove: false, move: false } };
  t.mock.method(globalThis, "fetch", async (path, options) => { assert.equal(path, "/api/v1/downloads/pt-verification/revoke"); const body = JSON.parse(options.body); assert.equal(body.confirm, true); assert.equal(body.confirmAccountReviewed, false); assert.equal(body.mode, "revoke"); return new Response(JSON.stringify({ success: true, code: 0 })); });
  await applyPTVerification("token", revoke, true);
});
test("PT stale reviews and mismatched task/action scopes never authorize writes", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; return new Response(JSON.stringify({ success: true, code: 0, data: { ...review, actions: { ...input.actions, remove: true } } })); });
  await assert.rejects(applyPTVerification("token", { ...review, reviewExpires: "2000-01-01T00:00:00Z" }, true, true, true, true)); assert.equal(calls, 0);
  await assert.rejects(reviewPTVerification("token", input)); assert.equal(calls, 1);
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ success: true, code: 0, data: { ...review, evidence: { hash: "c".repeat(40), addedAt: 1 } } })));
  await assert.rejects(reviewPTVerification("token", input));
});
test("PT lost attestation response never replays authority or a downloader operation", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); }); await assert.rejects(applyPTVerification("token", review, true, true, true, true)); assert.equal(calls, 1);
});
