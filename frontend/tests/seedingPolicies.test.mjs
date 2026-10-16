import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getSeedingEvidence, reviewSeedingPolicy, bindSeedingPolicy } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const scope = { downloaderId: 1, taskId: "a".repeat(40), resourceId: "go-search-" + "b".repeat(32) };
const review = { policy: { downloaderId: 1, infoHash: scope.taskId, requirements: { seconds: 900, ratio: 1.5, hr: null } }, fingerprint: "c".repeat(64), needsUnknownHold: true };
test("PT evidence and source review are read only, with exact task/source scope", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    if (calls === 1) {
      assert.equal(path, `/api/v1/downloads/seeding?downloaderId=1&taskId=${scope.taskId}`); assert.equal(options.method, undefined);
      return new Response(JSON.stringify({ code: 0, success: true, data: { registered: false, policies: [], evidence: { hash: scope.taskId, seconds: null, ratio: null } } }));
    }
    assert.equal(path, "/api/v1/downloads/seeding/review"); assert.deepEqual(JSON.parse(options.body), scope);
    return new Response(JSON.stringify({ code: 0, success: true, data: review }));
  });
  assert.equal((await getSeedingEvidence("token", scope)).evidence.seconds, null);
  await reviewSeedingPolicy("token", scope); assert.equal(calls, 2);
});
test("PT binding requires all separate consents and never accepts edited thresholds", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++; assert.equal(path, "/api/v1/downloads/seeding/bind");
    assert.deepEqual(JSON.parse(options.body), { ...scope, fingerprint: review.fingerprint, confirm: true, confirmOldExecutorsStopped: true, confirmUnknownHold: true });
    return new Response(JSON.stringify({ code: 0, success: true, data: {} }));
  });
  await assert.rejects(bindSeedingPolicy("token", scope, review));
  await assert.rejects(bindSeedingPolicy("token", scope, review, true));
  await assert.rejects(bindSeedingPolicy("token", scope, review, true, true));
  await assert.rejects(bindSeedingPolicy("token", { ...scope, downloaderId: 2 }, review, true, true, true));
  assert.equal(calls, 0); await bindSeedingPolicy("token", scope, review, true, true, true); assert.equal(calls, 1);
});
test("PT ambiguous response never replays binding or sends downloader actions", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); });
  await assert.rejects(bindSeedingPolicy("token", scope, review, true, true, true)); assert.equal(calls, 1);
});
test("PT client rejects invalid hashes, nonnumeric IDs and cross-task evidence", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; return new Response(JSON.stringify({ success: true, code: 0, data: { ...review, policy: { ...review.policy, infoHash: "d".repeat(40) }, policies: [], evidence: { hash: "d".repeat(40) } } })); });
  await assert.rejects(getSeedingEvidence("token", { downloaderId: 0, taskId: scope.taskId }));
  await assert.rejects(getSeedingEvidence("token", { downloaderId: NaN, taskId: scope.taskId }));
  await assert.rejects(getSeedingEvidence("token", { downloaderId: 1, taskId: "x".repeat(40) }));
  assert.equal(calls, 0); await assert.rejects(getSeedingEvidence("token", scope)); await assert.rejects(reviewSeedingPolicy("token", scope)); assert.equal(calls, 2);
});
