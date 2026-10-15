import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const source = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { reviewDownloadCompletion } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
test("completion review preserves exact downloader/task IDs and stays readonly", async (t) => {
  let calls = 0; const input = { downloaderId: "9223372036854775807", taskId: "a".repeat(40) };
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/organization/downloads/completion/review"); assert.equal(options.headers.Authorization, "token"); assert.deepEqual(JSON.parse(options.body), input); return new Response(JSON.stringify({ code: 0, success: true, data: { reviewOnly: true, eligible: false, reasons: ["still checking"], fingerprint: "a".repeat(64) } })); });
  const review = await reviewDownloadCompletion("token", input); assert.equal(review.eligible, false); assert.equal(calls, 1);
});
test("completion errors never retry, tag, stop, delete or organize tasks", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path) => { calls++; assert.equal(path, "/api/v1/organization/downloads/completion/review"); return new Response(JSON.stringify({ code: 409, success: false, message: "evidence changed" }), { status: 409 }); });
  await assert.rejects(reviewDownloadCompletion("token", { downloaderId: "1", taskId: "a".repeat(40) })); assert.equal(calls, 1);
});
test("completion client rejects execution-shaped or unbound review responses", async (t) => {
  for (const data of [{ reviewOnly: false, fingerprint: "a".repeat(64) }, { reviewOnly: true }, { reviewOnly: true, fingerprint: "wrong" }]) { t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data }))); await assert.rejects(reviewDownloadCompletion("token", { downloaderId: "1", taskId: "a".repeat(40) })); t.mock.restoreAll(); }
});
