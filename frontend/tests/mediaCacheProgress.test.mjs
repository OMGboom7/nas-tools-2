import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
async function load(path) {
  const { outputText } = ts.transpileModule(readFileSync(new URL(path, import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
}
const { getMediaSyncProgress, lookupMediaCache, getDiscovery } = await load("../src/api/client.ts");
const { mediaCacheLabel } = await load("../src/utils/mediaCache.ts");
const cache = { displayOnly: true, state: "ready", generation: "a".repeat(32), updated: "2026-10-16", server: "emby", libraries: ["films"] };
const progress = { stage: "publishing", librariesDone: 1, librariesTotal: 1, itemsDone: 1, episodesDone: 0, pagesRead: 1 };
const activity = { id: "a".repeat(32), kind: "manual", state: "collecting", snapshotGeneration: "", progress };
test("cache badges never treat unknown or negative records as live absence or complete series", () => {
  assert.match(mediaCacheLabel(cache, { state: "present", episodes: 1, seasons: 1 }, true), /不代表全集/);
  assert.match(mediaCacheLabel(cache, { state: "present", episodes: 0, seasons: 0 }, false), /非实时/);
  assert.match(mediaCacheLabel(cache, { state: "not-recorded", episodes: 0, seasons: 0 }, false), /不代表当前缺失/);
  for (const state of ["unsynced", "scope-changed", "unavailable"]) assert.match(mediaCacheLabel({ ...cache, state }, { state: "present" }, false), /不可用/);
  assert.match(mediaCacheLabel(cache, { state: "unknown-future-state" }, false), /不可用/); assert.match(mediaCacheLabel(undefined, undefined, false), /不可用/);
});
test("display cache lookup sends identities only and never submits download or subscription", async (t) => {
  let calls = 0; const items = [{ tmdbid: "100", type: "movie", title: "Movie", year: "2026" }];
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/library/sync/lookup"); assert.deepEqual(JSON.parse(options.body), { items }); return new Response(JSON.stringify({ success: true, code: 0, data: { cache, results: [{ state: "present", episodes: 0, seasons: 0 }] } })); });
  await assert.rejects(lookupMediaCache("token", [])); const result = await lookupMediaCache("token", items); assert.equal(result.cache.displayOnly, true); assert.equal(calls, 1);
});
test("progress polling is abortable read-only and publication preparation is not completed", async (t) => {
  let calls = 0; const controller = new AbortController();
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/library/sync/progress"); assert.equal(options.method, undefined); assert.equal(options.signal, controller.signal); return new Response(JSON.stringify({ success: true, code: 0, data: { activities: [activity], indeterminate: true } })); });
  const result = await getMediaSyncProgress("token", controller.signal); assert.equal(result[0].state, "collecting"); assert.equal(result[0].snapshotGeneration, ""); assert.equal(calls, 1);
});
test("progress rejects malformed and uncommitted success and never retries lost reads", async (t) => {
  for (const invalid of [{ ...activity, state: "completed" }, { ...activity, state: "new-state" }, { ...activity, progress: null }, { ...activity, progress: { ...progress, itemsDone: -1 } }]) {
    t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ success: true, code: 0, data: { activities: [invalid] } }))); await assert.rejects(getMediaSyncProgress("token"));
  }
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); }); await assert.rejects(getMediaSyncProgress("token")); assert.equal(calls, 1);
});
test("discovery preserves independent cached and subscribed fields without automatic actions", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path) => { calls++; assert.equal(path, "/api/v1/discovery"); return new Response(JSON.stringify({ success: true, code: 0, data: { cache, items: [{ id: "100", type: "MOV", subscribed: false, cachedPresence: { state: "present" } }] } })); });
  const result = await getDiscovery("token", "trending"); assert.equal(result.items[0].subscribed, false); assert.equal(result.items[0].cachedPresence.state, "present"); assert.equal(calls, 1);
});
