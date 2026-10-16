import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getMediaSchedule, reviewMediaSchedule, activateMediaSchedule, stopMediaSchedule } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const catalog = { server: "emby", fingerprint: "a".repeat(64), libraries: [{ id: "films", name: "Films", type: "movie" }] };
const review = { definition: { server: "emby", configDigest: catalog.fingerprint, authorityDigest: "b".repeat(64), libraries: ["films"], intervalSeconds: 3600, actor: "admin" }, revision: "", replacesEnabled: false, fingerprint: "c".repeat(64) };
test("scheduled media review and status do not enable execution", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(options.headers.Authorization, "token"); if (path.endsWith("/review")) { assert.equal(options.method, "POST"); assert.deepEqual(JSON.parse(options.body), { libraries: ["films"], hours: 1, configFingerprint: catalog.fingerprint }); } else assert.equal(options.method, undefined); return new Response(JSON.stringify({ success: true, code: 0, data: path.endsWith("/review") ? review : { schedule: { enabled: false }, runs: [], workerRunning: false } })); });
  await getMediaSchedule("token"); await reviewMediaSchedule("token", catalog, ["films"], 1); assert.equal(calls, 2);
});
test("invalid schedules never reach the server", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("unexpected"); });
  for (const hours of [0, -1, 0.001, 8761, NaN, Infinity]) await assert.rejects(reviewMediaSchedule("token", catalog, ["films"], hours));
  for (const ids of [[], ["missing"], ["films", "films"]]) await assert.rejects(reviewMediaSchedule("token", catalog, ids, 1));
  await assert.rejects(activateMediaSchedule("token", review)); await assert.rejects(activateMediaSchedule("token", review, true)); await assert.rejects(stopMediaSchedule("token", "d".repeat(32))); assert.equal(calls, 0);
});
test("schedule activation binds reviewed scope and stop is separately confirmed", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; const body = JSON.parse(options.body); if (path.endsWith("/activate")) assert.deepEqual(body, { libraries: ["films"], hours: 1, configFingerprint: catalog.fingerprint, fingerprint: review.fingerprint, confirm: true, legacyStopped: true }); else assert.deepEqual(body, { revision: "d".repeat(32), confirm: true }); return new Response(JSON.stringify({ success: true, code: 0, data: { enabled: path.endsWith("/activate"), revision: "d".repeat(32) } })); });
  await activateMediaSchedule("token", review, true, true); await stopMediaSchedule("token", "d".repeat(32), true); assert.equal(calls, 2);
});
test("lost authorization or stop response never automatically retries", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); });
  await assert.rejects(activateMediaSchedule("token", review, true, true)); assert.equal(calls, 1); await assert.rejects(stopMediaSchedule("token", "d".repeat(32), true)); assert.equal(calls, 2);
});
test("review rejects changed library scope and period", async (t) => {
  for (const definition of [{ ...review.definition, libraries: ["different"] }, { ...review.definition, intervalSeconds: 7200 }, { ...review.definition, server: "plex" }]) {
    t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ success: true, code: 0, data: { ...review, definition } }))); await assert.rejects(reviewMediaSchedule("token", catalog, ["films"], 1));
  }
});
