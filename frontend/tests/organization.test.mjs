import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getOrganizationRoots, previewOrganization, getOrganizationJobs, createOrganizationJob, controlOrganizationJob, abandonOrganizationJob } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);

test("abandonment has independent explicit consents and never authorizes source removal", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/jobs/job%2Fid/abandon-unpublished");
    assert.equal(options.headers.Authorization, "token");
    assert.deepEqual(JSON.parse(options.body), { confirm: true, confirmDiscardStaging: calls === 2, confirmOldExecutorsStopped: calls === 2 });
    return new Response(JSON.stringify({ code: 0, success: true, data: { id: "job/id", state: "abandoned" } }));
  });
  await abandonOrganizationJob("token", "job/id");
  await abandonOrganizationJob("token", "job/id", true, true);
  assert.equal(calls, 2);
});

test("abandonment receipt failure is never automatically retried", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => {
    calls++;
    return new Response(JSON.stringify({ code: 503, success: false, message: "receipt unavailable" }), { status: 503 });
  });
  await assert.rejects(abandonOrganizationJob("token", "job", true, true));
  assert.equal(calls, 1);
});

test("organization preview uses selected configured roots and has no execution retry", async (t) => {
  const input = { sourceId: "source", targetId: "target", path: "Movies", mode: "move" };
  const data = { previewOnly: true, fingerprint: "a".repeat(64), items: [{ source: "Movie.mkv", kind: "media", size: 100, modified: "1791300000123456789", status: "available" }], mode: "move", skipped: 0 };
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/plan");
    assert.equal(options.method, "POST");
    assert.equal(options.headers.Authorization, "test-token");
    assert.deepEqual(JSON.parse(options.body), input);
    return new Response(JSON.stringify({ code: 0, success: true, data }));
  });
  assert.deepEqual(await previewOrganization("test-token", input), data);
  assert.equal(calls, 1);
});

test("creating a reviewed job does not execute it", async (t) => {
  const input = { sourceId: "s", targetId: "t", path: ".", mode: "copy", fingerprint: "b".repeat(64) };
  const data = { id: "saved-job", state: "ready", items: [] };
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/jobs");
    assert.equal(options.headers.Authorization, "token");
    assert.deepEqual(JSON.parse(options.body), input);
    return new Response(JSON.stringify({ code: 0, success: true, data }));
  });
  assert.deepEqual(await createOrganizationJob("token", input), data);
  assert.equal(calls, 1);
});

test("copy execution failure is never automatically retried or replaced by reconciliation", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/jobs/job%2Fid/execute");
    assert.deepEqual(JSON.parse(options.body), { confirm: true });
    return new Response(JSON.stringify({ code: 503, success: false, message: "journal unavailable" }), { status: 503 });
  });
  await assert.rejects(controlOrganizationJob("token", "job/id", "execute"));
  assert.equal(calls, 1);
});

test("saved copy jobs reload without triggering execution", async (t) => {
  t.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, "/api/v1/organization/jobs");
    assert.equal(options.method, undefined);
    return new Response(JSON.stringify({ code: 0, success: true, data: [{ id: "job", state: "needs_review" }] }));
  });
  assert.equal((await getOrganizationJobs("token"))[0].state, "needs_review");
});

test("organization client rejects an execution-shaped response", async (t) => {
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: { previewOnly: false } })));
  await assert.rejects(previewOrganization("test-token", { sourceId: "source", targetId: "target", path: ".", mode: "copy" }));
});

test("organization root loading is authenticated and preserves source/target roles", async (t) => {
  const data = { sources: [{ id: "s", path: "/downloads", label: "source", type: "" }], targets: [] };
  t.mock.method(globalThis, "fetch", async (path, options) => {
    assert.equal(path, "/api/v1/organization/roots");
    assert.equal(options.headers.Authorization, "test-token");
    return new Response(JSON.stringify({ code: 0, success: true, data }));
  });
  assert.deepEqual(await getOrganizationRoots("test-token"), data);
});

for (const mode of ["link", "softlink", "move"]) {
  test(`${mode} job creation preserves reviewed mode without copy fallback or execution`, async (t) => {
    const input = { sourceId: "s", targetId: "t", path: ".", mode, fingerprint: "c".repeat(64) };
    let calls = 0;
    t.mock.method(globalThis, "fetch", async (path, options) => {
      calls++;
      assert.equal(path, "/api/v1/organization/jobs");
      assert.equal(options.headers.Authorization, "token");
      assert.deepEqual(JSON.parse(options.body), input);
      return new Response(JSON.stringify({ code: 0, success: true, data: { id: "job", mode, state: "ready" } }));
    });
    assert.equal((await createOrganizationJob("token", input)).mode, mode);
    assert.equal(calls, 1);
  });
}

for (const action of ["execute", "resume-move", "resume-publication"]) {
  test(`move ${action} only sends source-removal authorization when explicitly provided`, async (t) => {
    let calls = 0;
    t.mock.method(globalThis, "fetch", async (path, options) => {
      calls++;
      assert.equal(path, `/api/v1/organization/jobs/job%2Fid/${action}`);
      assert.equal(options.headers.Authorization, "token");
      assert.deepEqual(JSON.parse(options.body), calls === 1 ? { confirm: true } : { confirm: true, confirmSourceRemoval: true });
      return new Response(JSON.stringify({ code: 0, success: true, data: { id: "job/id", mode: "move" } }));
    });
    await controlOrganizationJob("token", "job/id", action);
    await controlOrganizationJob("token", "job/id", action, true);
    assert.equal(calls, 2);
  });
  test(`move ${action} failure never retries or falls back to copying/reconciliation`, async (t) => {
    let calls = 0;
    t.mock.method(globalThis, "fetch", async (path, options) => {
      calls++;
      assert.equal(path, `/api/v1/organization/jobs/job/${action}`);
      assert.deepEqual(JSON.parse(options.body), { confirm: true, confirmSourceRemoval: true });
      return new Response(JSON.stringify({ code: 409, success: false, message: "saved proof requires review" }), { status: 409 });
    });
    await assert.rejects(controlOrganizationJob("token", "job", action, true));
    assert.equal(calls, 1);
  });
}

test("move jobs load stored source/target roots and cleanup receipts without a mutation", async (t) => {
  const data = [{ id: "job", mode: "move", state: "needs_review", sourceRoot: "/original-downloads", targetRoot: "/original-library", items: [{ state: "completed", reason: "Move target recovery cleanup pending" }] }];
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/jobs");
    assert.equal(options.method, undefined);
    return new Response(JSON.stringify({ code: 0, success: true, data }));
  });
  assert.deepEqual(await getOrganizationJobs("token"), data);
  assert.equal(calls, 1);
});
