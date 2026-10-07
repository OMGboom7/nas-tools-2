import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getOrganizationRoots, previewOrganization, getOrganizationJobs, createOrganizationJob, controlOrganizationJob } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);

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

for (const mode of ["link", "softlink"]) {
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
