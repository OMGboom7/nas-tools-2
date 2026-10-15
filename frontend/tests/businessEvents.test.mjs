import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getBusinessEvents, dismissBusinessDelivery } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
test("business event refresh only reads committed receipts without sending", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/notifications/events"); assert.equal(options.method, undefined); assert.equal(options.headers.Authorization, "token"); return new Response(JSON.stringify({ code: 0, success: true, data: [{ id: "event", deliveries: [{ state: "uncertain" }] }] })); });
  assert.equal((await getBusinessEvents("token"))[0].deliveries[0].state, "uncertain"); assert.equal(calls, 1);
});
test("delivery dismissal requires exact ID, reason and independent confirmation", async (t) => {
  let calls = 0; const id = "a".repeat(64);
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, `/api/v1/notifications/events/${id}/dismiss`); assert.deepEqual(JSON.parse(options.body), { confirm: true, reason: "reviewed" }); return new Response(JSON.stringify({ code: 0, success: true })); });
  await assert.rejects(dismissBusinessDelivery("token", id, "reviewed")); await assert.rejects(dismissBusinessDelivery("token", "invalid", "reviewed", true)); await assert.rejects(dismissBusinessDelivery("token", id, " ", true)); assert.equal(calls, 0); await dismissBusinessDelivery("token", id, "reviewed", true); assert.equal(calls, 1);
});
test("lost dismissal response never resends a notification or repeats disposition", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost response"); }); await assert.rejects(dismissBusinessDelivery("token", "a".repeat(64), "reviewed", true)); assert.equal(calls, 1);
});
test("event receipts reject malformed or unauthenticated responses", async (t) => {
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: {} }))); await assert.rejects(getBusinessEvents("token"));
});
