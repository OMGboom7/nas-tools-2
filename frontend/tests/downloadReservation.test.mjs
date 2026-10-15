import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { reviewDownloadReservation, recoverDownloadReservation } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const review = { batchId: "batch/id", actor: "admin", reason: "restart", missing: [1], jobs: ["original"], plans: [], omitted: [], fingerprint: "a".repeat(64) };
test("reservation review binds the original batch and reason without execution authority", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/organization/downloads/batches/batch%2Fid/reservation-review"); assert.deepEqual(JSON.parse(options.body), { reason: "restart" }); assert.equal(options.headers.Authorization, "token"); return new Response(JSON.stringify({ code: 0, success: true, data: review })); });
  await assert.rejects(reviewDownloadReservation("token", "batch/id", " ")); assert.equal(calls, 0);
  assert.equal((await reviewDownloadReservation("token", "batch/id", "restart")).missing[0], 1); assert.equal(calls, 1);
});
test("reservation completion requires separate confirmation, stopped executors and omitted consent", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/organization/downloads/batches/batch%2Fid/recover-reservation"); assert.deepEqual(JSON.parse(options.body), { reason: "restart", fingerprint: review.fingerprint, confirm: true, confirmOldExecutorsStopped: true, confirmOmitted: true }); return new Response(JSON.stringify({ code: 0, success: true, data: { id: "batch/id", state: "ready" } })); });
  const omitted = { ...review, omitted: [{ path: "notes.nfo" }] };
  await assert.rejects(recoverDownloadReservation("token", "batch/id", review)); await assert.rejects(recoverDownloadReservation("token", "batch/id", review, true)); await assert.rejects(recoverDownloadReservation("token", "other", review, true, true)); await assert.rejects(recoverDownloadReservation("token", "batch/id", omitted, true, true)); assert.equal(calls, 0);
  assert.equal((await recoverDownloadReservation("token", "batch/id", omitted, true, true, true)).state, "ready"); assert.equal(calls, 1);
});
test("lost reservation response never retries completion or starts file execution", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path) => { calls++; assert.match(path, /recover-reservation$/); throw new Error("lost response"); });
  await assert.rejects(recoverDownloadReservation("token", "batch/id", review, true, true)); assert.equal(calls, 1);
});
test("reservation responses must match the reviewed batch identity", async (t) => {
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: { ...review, batchId: "other", id: "other" } })));
  await assert.rejects(reviewDownloadReservation("token", "batch/id", "reason")); await assert.rejects(recoverDownloadReservation("token", "batch/id", review, true, true));
});
