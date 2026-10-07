import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { controlSubscription, getSubscriptions } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);

test("reconciliation uses the authenticated native endpoint and returns restored progress", async (t) => {
  const progress = { submitted: 1, remaining: [3], completed: false, uncertain: false };
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/subscriptions/TV/2/reconcile");
    assert.equal(options.method, "POST");
    assert.equal(options.headers.Authorization, "test-token");
    assert.equal(options.body, undefined);
    return new Response(JSON.stringify({ code: 0, success: true, data: progress }));
  });
  assert.deepEqual(await controlSubscription("test-token", "TV", "2", "reconcile"), progress);
  assert.equal(calls, 1);
});

test("negative verification is an error and is never retried as a submission", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path) => {
    calls++;
    assert.equal(path, "/api/v1/subscriptions/MOV/1/reconcile");
    return new Response(JSON.stringify({ code: 409, success: false, message: "pending evidence retained" }), { status: 409 });
  });
  await assert.rejects(controlSubscription("test-token", "MOV", "1", "reconcile"));
  assert.equal(calls, 1);
});

test("subscription lists preserve the pending submission flag", async (t) => {
  const data = { items: [{ id: "2", type: "TV", pendingSubmission: true }], history: [] };
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data })));
  assert.deepEqual(await getSubscriptions("test-token"), data);
});
