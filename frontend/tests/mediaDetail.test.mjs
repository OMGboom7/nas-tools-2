import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
async function load(path) { const { outputText } = ts.transpileModule(readFileSync(new URL(path, import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } }); return import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`); }
const { getMediaDetail, getMediaSeason, getRelatedMedia, getMediaPlayback, subscribeMediaDetail } = await load("../src/api/client.ts");
const { mediaDetailPath, parseMediaDetailPath, isMediaSeasonSubscribed } = await load("../src/utils/mediaDetail.ts");
const cache = { displayOnly: true, state: "unavailable", generation: "", libraries: [] };
const detail = { id: "200", kind: "tv", sourceId: "200", title: "Series", year: "2025", seasons: [{ number: 0 }, { number: 1 }, { number: 2 }], cast: [], crew: [], genres: [], facts: [], warnings: [], subscriptions: [{ id: "1", season: "S01" }], subscribed: true, subscriptionsKnown: true, cache };
const response = (data) => new Response(JSON.stringify({ code: 0, success: true, data }));
test("detail navigation supports canonical media identities and rejects unsafe paths", () => {
  assert.equal(mediaDetailPath("电影", "100"), "/media/movie/100"); assert.equal(mediaDetailPath("动漫", "BG:300"), "/media/tv/BG%3A300"); assert.deepEqual(parseMediaDetailPath("/media/tv/BG%3A300"), { kind: "tv", id: "BG:300" });
  for (const path of ["/media/movie/../bad", "/media/tv/%2Fetc", "/media/person/1", "/media/movie/BG%3A300", "/media/movie/0", "/media/tv/%xx"]) assert.equal(parseMediaDetailPath(path), null);
});
test("details and special episode queries are abortable reads, never mutations", async (t) => {
  const controller = new AbortController(); let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(options.method, undefined); assert.equal(options.headers.Authorization, "token"); assert.equal(options.signal, controller.signal); if (calls === 1) { assert.equal(path, "/api/v1/media/details/tv/200"); return response(detail); } assert.equal(path, "/api/v1/media/details/tv/200/seasons/0?page=1"); return response({ id: "200", season: 0, page: 1, total: 1, episodes: [{ id: 1, season: 0, number: 0 }], cache }); });
  await getMediaDetail("token", "tv", "200", controller.signal); const result = await getMediaSeason("token", "200", 0, 1, controller.signal); assert.equal(result.episodes[0].number, 0); assert.equal(calls, 2);
});
test("media response identity and season mismatch are not accepted", async (t) => {
  t.mock.method(globalThis, "fetch", async () => response({ ...detail, id: "201" })); await assert.rejects(getMediaDetail("token", "tv", "200"));
  t.mock.method(globalThis, "fetch", async () => response({ id: "200", season: 1, page: 1, episodes: [{ number: 1, season: 1 }], cache })); await assert.rejects(getMediaSeason("token", "200", 0));
});
test("invalid detail selectors, seasons and pages do not reach the network", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("unexpected"); });
  await assert.rejects(getMediaDetail("token", "movie", "BG:300")); await assert.rejects(getMediaDetail("token", "tv", "../bad")); await assert.rejects(getMediaSeason("token", "200", -1)); await assert.rejects(getMediaSeason("token", "200", 0, 0)); await assert.rejects(getRelatedMedia("token", "tv", "200", "other")); assert.equal(calls, 0);
});
test("related media reads use the correct kind, relation and requested page", async (t) => {
  t.mock.method(globalThis, "fetch", async (path, options) => { assert.equal(path, "/api/v1/media/details/tv/200/related/recommendations?page=2"); assert.equal(options.method, undefined); return response({ category: "recommendations", page: 2, items: [{ id: "201", type: "TV" }] }); });
  const result = await getRelatedMedia("token", "tv", "200", "recommendations", 2); assert.equal(result.items[0].type, "TV");
});
test("an existing TV season does not disable subscription to a different season or specials", () => {
  assert.equal(isMediaSeasonSubscribed(detail, 1), true); assert.equal(isMediaSeasonSubscribed(detail, 2), false); assert.equal(isMediaSeasonSubscribed(detail, 0), false);
  assert.equal(isMediaSeasonSubscribed({ ...detail, subscriptions: [{ id: "2", season: "" }] }, 0), false); assert.equal(isMediaSeasonSubscribed({ ...detail, subscriptions: [{ id: "2", season: "S00" }] }, 0), true); assert.equal(isMediaSeasonSubscribed({ ...detail, subscriptionsKnown: false }, 1), false);
});
test("subscription requires explicit confirmation and preserves the exact zero season", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/subscriptions"); assert.equal(options.method, "POST"); const body = JSON.parse(options.body); assert.equal(body.season, "0"); assert.equal(body.mediaId, "200"); assert.equal(body.type, "TV"); assert.equal(body.fuzzyMatch, false); assert.equal(body.cachedPresence, undefined); return response({ id: "2" }); });
  await assert.rejects(subscribeMediaDetail("token", detail, 0)); await assert.rejects(subscribeMediaDetail("token", detail, undefined, true)); await assert.rejects(subscribeMediaDetail("token", detail, 9, true)); assert.equal(calls, 0); await subscribeMediaDetail("token", detail, 0, true); assert.equal(calls, 1);
});
test("lost subscription acknowledgement never automatically resubmits", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("lost"); }); await assert.rejects(subscribeMediaDetail("token", detail, 1, true)); assert.equal(calls, 1);
});
test("playback only accepts a matching live-verified safe web URL", async (t) => {
  t.mock.method(globalThis, "fetch", async (path, options) => { assert.equal(path, "/api/v1/media/details/tv/200/playback"); assert.equal(options.method, undefined); return response({ id: "200", kind: "tv", liveVerified: true, link: "https://play.test/web/index.html#!/details?id=one" }); });
  assert.match(await getMediaPlayback("token", "tv", "200"), /^https:/);
  for (const link of ["javascript:alert(1)", "https://user:secret@play.test", "https://play.test?api_key=secret"]) { t.mock.method(globalThis, "fetch", async () => response({ id: "200", kind: "tv", liveVerified: true, link })); await assert.rejects(getMediaPlayback("token", "tv", "200")); }
  t.mock.method(globalThis, "fetch", async () => response({ id: "200", kind: "tv", liveVerified: false, link: "https://play.test" })); await assert.rejects(getMediaPlayback("token", "tv", "200"));
});
