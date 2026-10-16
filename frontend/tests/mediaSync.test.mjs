import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getMediaSyncLibraries, getMediaSyncStatus, synchronizeMediaLibraries } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const catalog = { server: "emby", fingerprint: "a".repeat(64), libraries: [{ id: "films", name: "Films", type: "movie" }] };
test("media sync browsing is read-only and rejects invalid pagination", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(options.method, undefined); assert.equal(options.headers.Authorization, "token"); return new Response(JSON.stringify({ success: true, code: 0, data: path.endsWith("libraries") ? catalog : { displayOnly: true, current: true, snapshot: { items: [], page: 2 } } })); });
  await assert.rejects(getMediaSyncStatus("token", 0)); await getMediaSyncStatus("token", 2); await getMediaSyncLibraries("token"); assert.equal(calls, 2);
});
test("media sync requires exact selected libraries and independent legacy-stop confirmation", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/library/sync"); assert.equal(options.method, "POST"); assert.deepEqual(JSON.parse(options.body), { libraries: ["films"], confirm: true, legacyStopped: true, fingerprint: catalog.fingerprint }); return new Response(JSON.stringify({ success: true, code: 0, data: { server: "emby", configDigest: catalog.fingerprint, generation: "b".repeat(32) } })); });
  await assert.rejects(synchronizeMediaLibraries("token", catalog, ["films"])); await assert.rejects(synchronizeMediaLibraries("token", catalog, ["films"], true)); await assert.rejects(synchronizeMediaLibraries("token", catalog, ["unknown"], true, true)); await assert.rejects(synchronizeMediaLibraries("token", catalog, ["films", "films"], true, true)); await assert.rejects(synchronizeMediaLibraries("token", catalog, [], true, true)); assert.equal(calls, 0);
  await synchronizeMediaLibraries("token", catalog, ["films"], true, true); assert.equal(calls, 1);
});
test("lost synchronization response never resubmits and mismatched scope cannot report success", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost response"); });
  await assert.rejects(synchronizeMediaLibraries("token", catalog, ["films"], true, true)); assert.equal(calls, 1);
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ success: true, code: 0, data: { server: "plex", generation: "b".repeat(32), configDigest: catalog.fingerprint } })));
  await assert.rejects(synchronizeMediaLibraries("token", catalog, ["films"], true, true));
});
