import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getOrganizationRoots, previewOrganization } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);

test("organization preview uses selected configured roots and has no execution retry", async (t) => {
  const input = { sourceId: "source", targetId: "target", path: "Movies", mode: "move" };
  const data = { previewOnly: true, items: [{ source: "Movie.mkv", kind: "media", size: 100, modified: "1791300000123456789", status: "available" }], mode: "move", skipped: 0 };
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
