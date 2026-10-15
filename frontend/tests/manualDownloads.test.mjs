import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getManualDownloadReceipts, reconcileManualDownload } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
test("manual receipt pages are readonly and preserve exact downloader/task IDs", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/downloads/submissions?page=2"); assert.equal(options.method, undefined); assert.equal(options.headers.Authorization, "token"); return new Response(JSON.stringify({ code: 0, success: true, data: [{ id: "a".repeat(32), downloaderId: "9223372036854775807", state: "pending" }] })); });
  await assert.rejects(getManualDownloadReceipts("token", 0)); assert.equal(calls, 0); assert.equal((await getManualDownloadReceipts("token", 2))[0].downloaderId, "9223372036854775807"); assert.equal(calls, 1);
});
test("manual reconciliation needs confirmation and cannot supply new downloader or torrent authority", async (t) => {
  let calls = 0; const id = "a".repeat(32);
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, `/api/v1/downloads/submissions/${id}/reconcile`); assert.deepEqual(JSON.parse(options.body), { confirm: true }); return new Response(JSON.stringify({ code: 0, success: true, data: { id, state: "submitted" } })); });
  await assert.rejects(reconcileManualDownload("token", id)); await assert.rejects(reconcileManualDownload("token", "invalid", true)); assert.equal(calls, 0); assert.equal((await reconcileManualDownload("token", id, true)).state, "submitted"); assert.equal(calls, 1);
});
test("ambiguous manual reconciliation never retries or submits a new download", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost response"); }); await assert.rejects(reconcileManualDownload("token", "a".repeat(32), true)); assert.equal(calls, 1);
});
test("manual reconciliation rejects mismatched or still pending receipts", async (t) => {
  const id = "a".repeat(32); t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: { id, state: "pending" } }))); await assert.rejects(reconcileManualDownload("token", id, true));
});
