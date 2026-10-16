import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getQbProfiles, reviewQbProfile, recoverQbProfile, applyPTAdmission } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const profile = { id: "a".repeat(32), infoHash: "b".repeat(40), fingerprint: "qb:" + "c".repeat(64), seedingPolicyId: "d".repeat(64) };
const review = { profile, current: { hash: profile.infoHash }, next: "configure", fingerprint: "e".repeat(64) };
test("qB PT list and review read exact typed receipts without sending settings or additions", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; if (calls === 1) { assert.equal(path, "/api/v1/downloads/qbittorrent-profiles?page=2"); assert.equal(options.method, undefined); } else { assert.equal(path, `/api/v1/downloads/qbittorrent-profiles/${profile.id}/review`); assert.deepEqual(JSON.parse(options.body), {}); } return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? [profile] : review })); });
  await getQbProfiles("token", 2); await reviewQbProfile("token", profile.id); assert.equal(calls, 2);
});
test("qB PT settings and startup require separate reviewed authority", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, `/api/v1/downloads/qbittorrent-profiles/${profile.id}/recover`); assert.deepEqual(JSON.parse(options.body), { confirm: true, fingerprint: review.fingerprint, confirmSettings: true, confirmStart: false }); return new Response(JSON.stringify({ code: 0, success: true })); });
  await assert.rejects(recoverQbProfile("token", review)); await assert.rejects(recoverQbProfile("token", review, true)); await assert.rejects(recoverQbProfile("token", { ...review, next: "start" }, true, true)); assert.equal(calls, 0); await recoverQbProfile("token", review, true, true); assert.equal(calls, 1);
});
test("qB PT lost response never replays writes and malformed cross-task reviews are rejected", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); }); await assert.rejects(recoverQbProfile("token", review, true, true)); assert.equal(calls, 1);
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ success: true, code: 0, data: { ...review, current: { hash: "f".repeat(40) } } })));
  await assert.rejects(reviewQbProfile("token", profile.id)); await assert.rejects(getQbProfiles("token", 0));
});
test("qB admission keeps the same four explicit configuration-scoped consents", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/downloads/pt-admission/apply"); const body = JSON.parse(options.body); assert.equal(body.downloaderId, 1); assert.equal(body.confirmUnknownHold, true); assert.equal(body.confirmUnlimitedSeeding, true); return new Response(JSON.stringify({ success: true, code: 0 })); });
  const grant = { downloaderId: 1, type: "qbittorrent", enabled: true, fingerprint: "a".repeat(64) };
  await assert.rejects(applyPTAdmission("token", grant, true, true, true)); assert.equal(calls, 0); await applyPTAdmission("token", grant, true, true, true, true); assert.equal(calls, 1);
});
