import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getPTAdmission, reviewPTAdmission, applyPTAdmission } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const review = { downloaderId: 1, name: "TR", type: "transmission", enabled: true, fingerprint: "a".repeat(64), active: false, grant: null };
test("PT admission status and scope review never enable workers or write downloaders", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    if (calls === 1) { assert.equal(path, "/api/v1/downloads/pt-admission?downloaderId=1"); assert.equal(options.method, undefined); }
    else { assert.equal(path, "/api/v1/downloads/pt-admission/review"); assert.deepEqual(JSON.parse(options.body), { downloaderId: 1, enabled: true }); }
    return new Response(JSON.stringify({ success: true, code: 0, data: review }));
  });
  await getPTAdmission("token", 1); await reviewPTAdmission("token", 1, true); assert.equal(calls, 2);
});
test("PT admission enabling requires independent scope, executor, unknown hold and unlimited seed consents", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++; assert.equal(path, "/api/v1/downloads/pt-admission/apply");
    assert.deepEqual(JSON.parse(options.body), { downloaderId: 1, enabled: true, fingerprint: review.fingerprint, confirm: true, confirmOldExecutorsStopped: true, confirmUnknownHold: true, confirmUnlimitedSeeding: true });
    return new Response(JSON.stringify({ code: 0, success: true }));
  });
  await assert.rejects(applyPTAdmission("token", review));
  await assert.rejects(applyPTAdmission("token", review, true));
  await assert.rejects(applyPTAdmission("token", review, true, true));
  await assert.rejects(applyPTAdmission("token", review, true, true, true));
  await assert.rejects(applyPTAdmission("token", { ...review, type: "aria2" }, true, true, true, true));
  assert.equal(calls, 0); await applyPTAdmission("token", review, true, true, true, true); assert.equal(calls, 1);
});
test("PT withdrawal only carries explicit future-admission revocation, never task removal", async (t) => {
  t.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, "/api/v1/downloads/pt-admission/apply");
    assert.deepEqual(JSON.parse(options.body), { downloaderId: 1, enabled: false, fingerprint: review.fingerprint, confirm: true, confirmOldExecutorsStopped: false, confirmUnknownHold: false, confirmUnlimitedSeeding: false });
    return new Response(JSON.stringify({ code: 0, success: true }));
  });
  await applyPTAdmission("token", { ...review, enabled: false }, true);
});
test("PT lost grant response is never replayed and cross-downloader reviews are rejected", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); });
  await assert.rejects(applyPTAdmission("token", review, true, true, true, true)); assert.equal(calls, 1);
  await assert.rejects(getPTAdmission("token", NaN)); await assert.rejects(reviewPTAdmission("token", 0, true)); assert.equal(calls, 1);
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: { ...review, downloaderId: 2 } })));
  await assert.rejects(reviewPTAdmission("token", 1, true)); await assert.rejects(getPTAdmission("token", 1));
});
