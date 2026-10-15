import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getTransmissionProfiles, reviewTransmissionProfile, recoverTransmissionProfile } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const profile = { id: "a".repeat(32), state: "added", options: { Paused: false }, limits: { uploadKB: 10, downloadKB: 20, ratio: 1.5, idleMinutes: 60 } };
const review = { profile, actor: "0:admin", next: "configure", fingerprint: "b".repeat(64) };
test("Transmission settings list and review do not configure or start tasks", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; if (calls === 1) { assert.equal(path, "/api/v1/downloads/transmission-profiles?page=2"); assert.equal(options.method, undefined); } else { assert.match(path, /\/review$/); assert.deepEqual(JSON.parse(options.body), {}); } return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? [profile] : review })); });
  await assert.rejects(getTransmissionProfiles("token", 0)); await getTransmissionProfiles("token", 2); await reviewTransmissionProfile("token", profile.id); assert.equal(calls, 2);
});
test("Transmission profile recovery carries separate immutable settings/start authority", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.match(path, /\/recover$/); assert.deepEqual(JSON.parse(options.body), { confirm: true, fingerprint: review.fingerprint, confirmSettings: true, confirmStart: false }); return new Response(JSON.stringify({ code: 0, success: true, data: { ...profile, state: "configured" } })); });
  await assert.rejects(recoverTransmissionProfile("token", review)); await assert.rejects(recoverTransmissionProfile("token", review, true)); await assert.rejects(recoverTransmissionProfile("token", { ...review, next: "start" }, true, true)); assert.equal(calls, 0); await recoverTransmissionProfile("token", review, true, true); assert.equal(calls, 1);
});
test("lost profile response never repeats settings, startup or torrent addition", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); }); await assert.rejects(recoverTransmissionProfile("token", review, true, true)); assert.equal(calls, 1);
});
test("profile review rejects mismatched identity and unknown execution steps", async (t) => {
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: { ...review, next: "add-again" } }))); await assert.rejects(reviewTransmissionProfile("token", profile.id));
});
