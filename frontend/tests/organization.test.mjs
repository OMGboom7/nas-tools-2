import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const source = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getOrganizationRoots, previewOrganization, getOrganizationJobs, getOrganizationUnknown, getOrganizationHistory, getOrganizationJob, reviewOrganizationUnknown, detachOrganizationUnknown, createOrganizationJob, controlOrganizationJob, abandonOrganizationJob, reviewUnknownDelete, deleteUnknownRecord, getUnknownDeletions, restoreUnknownRecord } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);

test("unknown deletion review and audit listing are readonly and preserve exact large IDs and NULL", async (t) => {
  const id = "9223372036854775807";
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.method, undefined);
    assert.equal(options.headers.Authorization, "token");
    assert.equal(path, calls === 1 ? `/api/v1/organization/unknown/${id}/delete-review` : "/api/v1/organization/unknown/deletions");
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { record: { id, path: null, dest: null, mode: null, state: null }, eligible: true, fingerprint: "a".repeat(64) } : [] }));
  });
  const review = await reviewUnknownDelete("token", id);
  assert.equal(review.record.id, id);
  assert.equal(review.record.path, null);
  assert.deepEqual(await getUnknownDeletions("token"), []);
  assert.equal(calls, 2);
});

test("record deletion and restore carry independent explicit consent without source removal or transfer", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.method, "POST");
    assert.equal(options.headers.Authorization, "token");
    assert.equal(path, calls <= 2 ? "/api/v1/organization/unknown/7/delete" : "/api/v1/organization/unknown/deletions/a%2Fb/restore");
    const body = JSON.parse(options.body);
    assert.deepEqual(body, calls <= 2 ? { confirm: true, confirmRecordDeletion: calls === 2, reviewFingerprint: "b".repeat(64) } : { confirm: true, confirmRecordRestore: calls === 4 });
    return new Response(JSON.stringify({ code: 0, success: true, data: { id: "a/b" } }));
  });
  await deleteUnknownRecord("token", "7", "b".repeat(64));
  await deleteUnknownRecord("token", "7", "b".repeat(64), true);
  await restoreUnknownRecord("token", "a/b");
  await restoreUnknownRecord("token", "a/b", true);
  assert.equal(calls, 4);
});

test("record mutation errors never retry, restore automatically or fall back to file execution", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => { calls++; return new Response(JSON.stringify({ code: 503, success: false, message: "pending receipt" }), { status: 503 }); });
  await assert.rejects(deleteUnknownRecord("token", "7", "c".repeat(64), true));
  await assert.rejects(restoreUnknownRecord("token", "receipt", true));
  assert.equal(calls, 2);
});
const eligibility = ts.transpileModule(readFileSync(new URL("../src/pages/organizationState.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { canAbandonOrganizationJob, buildOrganizationAssignment, buildOrganizationEpisodeSelection } = await import(`data:text/javascript;base64,${Buffer.from(eligibility.outputText).toString("base64")}`);

test("batch episode selection requires explicit TV identity, season, template and paired filter", () => {
  assert.equal(buildOrganizationEpisodeSelection("200", "1", "Show.{date:ti}.{ep}.mkv", "", "", "").episodeFormat, "Show.{date:ti}.{ep}.mkv");
  assert.equal(buildOrganizationEpisodeSelection("200", "1", "{{Show}}.{ep:4}.mkv", "", "", "").episodeFormat, "{{Show}}.{ep:4}.mkv");
  assert.throws(() => buildOrganizationEpisodeSelection("200", "1", "{{ep}}.mkv", "", "", ""));
  assert.deepEqual(buildOrganizationEpisodeSelection("200", "1", "Wrong.{ep}.mkv", "11", "11", "-10"), { tmdbId: "200", season: 1, episodeFormat: "Wrong.{ep}.mkv", startEpisode: 11, endEpisode: 11, episodeOffset: -10 });
  assert.deepEqual(buildOrganizationEpisodeSelection("200", "0", "Wrong.{ep}.mkv", "", "", ""), { tmdbId: "200", season: 0, episodeFormat: "Wrong.{ep}.mkv" });
  for (const args of [
    ["0200", "1", "Wrong.{ep}.mkv", "", "", ""],
    ["200", "", "Wrong.{ep}.mkv", "", "", ""],
    ["200", "1", "Wrong.{a}.mkv", "", "", ""],
    ["200", "1", "Wrong.{ep}.mkv", "11", "", ""],
    ["200", "1", "Wrong.{ep}.mkv", "11", "10", ""],
    ["200", "1", "Wrong.{ep}.mkv", "", "", "-1.5"],
    ["200", "1", "字".repeat(200)+"{ep}", "", "", ""]
  ]) assert.throws(() => buildOrganizationEpisodeSelection(...args));
});

test("batch episode rule follows reviewed preview and save, never runs or broadens requests", async (t) => {
  const episodeSelection = buildOrganizationEpisodeSelection("200", "1", "Wrong.{ep}.mkv", "11", "11", "-10");
  const input = { sourceId: "s", targetId: "t", path: "Season", mode: "move", episodeSelection };
  const fingerprint = "d".repeat(64);
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, calls === 1 ? "/api/v1/organization/plan" : "/api/v1/organization/jobs");
    assert.deepEqual(JSON.parse(options.body), calls === 1 ? input : { ...input, fingerprint });
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { previewOnly: true, fingerprint, items: [{ source: "Wrong.10.mkv", status: "filtered" }] } : { id: "saved", episodeSelection } }));
  });
  assert.equal((await previewOrganization("token", input)).items[0].status, "filtered");
  assert.deepEqual((await createOrganizationJob("token", { ...input, fingerprint })).episodeSelection, episodeSelection);
  assert.equal(calls, 2);
});

test("manual part auto, set and clear remain distinct for movie and TV", () => {
  const file = { source: "Movie.mkv", kind: "media", identity: "1:2", size: 100, modified: "1791500000123456789" };
  const make = (mode, value = "") => buildOrganizationAssignment(file, "movie", "300", "", "", "", "", "", mode, value);
  assert.equal(Object.hasOwn(make("auto"), "partOverride"), false);
  assert.equal(make("set", " cd01 ").partOverride, "CD01");
  assert.equal(JSON.parse(JSON.stringify(make("clear"))).partOverride, "");
  for (const part of ["PARTIII", "PARTC", "PARTAB", "DVD2", "DISK10", "DISC12"]) assert.equal(make("set", part).partOverride, part);
  for (const part of ["", "../CD1", "CD1/CD2", "CD123", "CUSTOM", "PARTIIII", "CD1\0"]) assert.throws(() => make("set", part));
  const tv = buildOrganizationAssignment(file, "tv", "200", "1", "2", "", "", "", "clear");
  assert.equal(tv.episode, 2);
  assert.equal(tv.partOverride, "");
});

test("manual part clearing follows reviewed preview and save without execution", async (t) => {
  const assignment = buildOrganizationAssignment({ source: "Movie.CD1.mkv", kind: "media", identity: "1:2", size: 100, modified: "1791500000123456789" }, "movie", "300", "", "", "", "", "", "clear");
  const input = { sourceId: "s", targetId: "t", path: "Movie.CD1.mkv", mode: "copy", assignments: [assignment] };
  const fingerprint = "c".repeat(64);
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, calls === 1 ? "/api/v1/organization/plan" : "/api/v1/organization/jobs");
    assert.deepEqual(JSON.parse(options.body), calls === 1 ? input : { ...input, fingerprint });
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { previewOnly: true, fingerprint, items: [{ partOverride: "" }] } : { id: "saved", items: [{ partOverride: "" }] } }));
  });
  assert.equal((await previewOrganization("token", input)).items[0].partOverride, "");
  assert.equal((await createOrganizationJob("token", { ...input, fingerprint })).items[0].partOverride, "");
  assert.equal(calls, 2);
});

test("release parts survive readonly preview and persisted job loading", async (t) => {
  let calls = 0;
  const items = [{ source: "Movie.2026.CD1.mkv", part: "CD1" }, { source: "Movie.2026.CD2.en.srt", part: "CD2" }, { source: "old.mkv" }];
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.headers.Authorization, "token");
    assert.equal(path, calls === 1 ? "/api/v1/organization/plan" : "/api/v1/organization/jobs/job");
    assert.equal(options.method, calls === 1 ? "POST" : undefined);
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { previewOnly: true, fingerprint: "a".repeat(64), items } : { id: "job", items } }));
  });
  assert.deepEqual((await previewOrganization("token", { sourceId: "s", targetId: "t", path: "", mode: "copy" })).items, items);
  assert.deepEqual((await getOrganizationJob("token", "job")).items, items);
  assert.equal(calls, 2);
});

test("filename episode rules and offsets stay explicit and do not override server bounds", () => {
  const file = { source: "Show.11.mkv", kind: "media", identity: "1:2", size: 100, modified: "1791500000123456789" };
  const selection = buildOrganizationAssignment(file, "tv", "200", "1", "", "", "Show.{ep}.mkv", "-10");
  assert.equal(buildOrganizationAssignment(file, "tv", "200", "1", "", "", "Show.{date:ti}.{ep}.mkv", "-10").episodeFormat, "Show.{date:ti}.{ep}.mkv");
  assert.equal(buildOrganizationAssignment(file, "tv", "200", "1", "", "", "{{Show}}.{ep:4}.mkv", "-10").episodeFormat, "{{Show}}.{ep:4}.mkv");
  assert.throws(() => buildOrganizationAssignment(file, "tv", "200", "1", "", "", "{{ep}}.mkv", "0"));
  assert.equal(selection.episodeFormat, "Show.{ep}.mkv");
  assert.equal(selection.episodeOffset, -10);
  assert.equal(selection.episode, undefined);
  assert.equal(selection.endEpisode, undefined);
  assert.equal(buildOrganizationAssignment(file, "tv", "200", "1", "11", "13", "", "-10").episode, 11);
  for (const offset of ["1.5", "100000", "EP-10", "NaN"]) assert.throws(() => buildOrganizationAssignment(file, "tv", "200", "1", "", "", "Show.{ep}.mkv", offset));
  assert.throws(() => buildOrganizationAssignment(file, "tv", "200", "1", "", "", "Show.{a}.mkv", "0"));
  const movie = buildOrganizationAssignment(file, "movie", "300", "", "", "", "Show.{ep}.mkv", "-10");
  assert.equal(movie.episodeFormat, undefined);
  assert.equal(movie.episodeOffset, undefined);
});

test("episode rules follow the same reviewed preview and creation without execution", async (t) => {
  const selection = buildOrganizationAssignment({ source: "Show.11.mkv", kind: "media", identity: "1:2", size: 100, modified: "1791500000123456789" }, "tv", "200", "1", "", "", "Show.{ep}.mkv", "-10");
  const input = { sourceId: "s", targetId: "t", path: "Show.11.mkv", mode: "copy", assignments: [selection] };
  const fingerprint = "b".repeat(64);
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, calls === 1 ? "/api/v1/organization/plan" : "/api/v1/organization/jobs");
    assert.deepEqual(JSON.parse(options.body), calls === 1 ? input : { ...input, fingerprint });
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { previewOnly: true, fingerprint } : { id: "saved", items: [{ episodeFormat: selection.episodeFormat, episodeOffset: -10 }] } }));
  });
  await previewOrganization("token", input);
  const saved = await createOrganizationJob("token", { ...input, fingerprint });
  assert.equal(saved.items[0].episodeOffset, -10);
  assert.equal(calls, 2);
});

test("manual identity preserves the exact source snapshot and requires explicit TV season/episode", () => {
  const file = { source: "unknown.mkv", kind: "media", identity: "1:2", size: 100, modified: "1791500000123456789" };
  assert.deepEqual(buildOrganizationAssignment(file, "movie", "300", "", "", ""), { source: file.source, identity: file.identity, size: file.size, modified: file.modified, kind: "movie", tmdbId: "300" });
  assert.deepEqual(buildOrganizationAssignment(file, "tv", "200", "0", "1", "3"), { source: file.source, identity: file.identity, size: file.size, modified: file.modified, kind: "tv", tmdbId: "200", season: 0, episode: 1, endEpisode: 3 });
  for (const id of ["", "0300", "../300", "9223372036854775808"]) assert.throws(() => buildOrganizationAssignment(file, "movie", id, "", "", ""));
  assert.throws(() => buildOrganizationAssignment(file, "tv", "200", "", "1", ""));
  assert.throws(() => buildOrganizationAssignment(file, "tv", "200", "1", "3", "2"));
  assert.throws(() => buildOrganizationAssignment({ ...file, kind: "subtitle" }, "movie", "300", "", "", ""));
});

test("unknown listing and linked job loading are authenticated and readonly", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.method, undefined);
    assert.equal(options.headers.Authorization, "token");
    assert.equal(path, calls === 1 ? "/api/v1/organization/unknown?page=2&keyword=movie+name" : "/api/v1/organization/jobs/old%2Fjob");
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { items: [], total: 0, page: 2 } : { id: "old/job", unknownRecordId: 7, unknownState: "pending" } }));
  });
  await getOrganizationUnknown("token", 2, "movie name");
  const job = await getOrganizationJob("token", "old/job");
  assert.equal(job.unknownState, "pending");
  assert.equal(calls, 2);
});

test("unknown compatibility retry confirms only the saved receipt and never retries files", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/jobs/job/reconcile-unknown");
    assert.deepEqual(JSON.parse(options.body), { confirm: true });
    return new Response(JSON.stringify({ code: 503, success: false, message: "receipt pending" }), { status: 503 });
  });
  await assert.rejects(controlOrganizationJob("token", "job", "reconcile-unknown"));
  assert.equal(calls, 1);
});

test("unknown detach requires a reviewed snapshot and independent confirmation without source removal", async (t) => {
  let calls = 0;
  const fingerprint = "e".repeat(64);
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.headers.Authorization, "token");
    if (calls === 1) {
      assert.equal(path, "/api/v1/organization/jobs/job%2Fid/unknown-status");
      assert.equal(options.method, undefined);
      return new Response(JSON.stringify({ code: 0, success: true, data: { fingerprint, eligible: true } }));
    }
    assert.equal(path, "/api/v1/organization/jobs/job%2Fid/detach-unknown");
    assert.deepEqual(JSON.parse(options.body), { confirm: true, confirmDetachUnknown: calls === 3, reviewFingerprint: fingerprint, reason: "changed record" });
    return new Response(JSON.stringify({ code: 0, success: true, data: { id: "job/id", unknownState: "detached" } }));
  });
  const review = await reviewOrganizationUnknown("token", "job/id");
  await detachOrganizationUnknown("token", "job/id", review.fingerprint, "changed record");
  await detachOrganizationUnknown("token", "job/id", review.fingerprint, "changed record", true);
  assert.equal(calls, 3);
});

test("unknown detach failure does not retry or fall back to transfer", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => {
    calls++;
    return new Response(JSON.stringify({ code: 503, success: false, message: "audit pending" }), { status: 503 });
  });
  await assert.rejects(detachOrganizationUnknown("token", "job", "f".repeat(64), "changed record", true));
  assert.equal(calls, 1);
});

test("history selection is readonly and its snapshot follows preview and creation", async (t) => {
  let calls = 0;
  const historyRecord = { id: 7, fingerprint: "a".repeat(64) };
  const input = { sourceId: "s", targetId: "t", path: "Movie.mkv", mode: "copy", historyRecord };
  const fingerprint = "b".repeat(64);
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.headers.Authorization, "token");
    if (calls === 1) {
      assert.equal(path, "/api/v1/organization/history?page=2&keyword=wrong+title");
      assert.equal(options.method, undefined);
      return new Response(JSON.stringify({ code: 0, success: true, data: { items: [{ ...historyRecord, title: "Wrong" }] } }));
    }
    assert.equal(path, calls === 2 ? "/api/v1/organization/plan" : "/api/v1/organization/jobs");
    assert.deepEqual(JSON.parse(options.body), calls === 2 ? input : { ...input, fingerprint });
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 2 ? { previewOnly: true, fingerprint } : { id: "new-job", historyRecordId: 7 } }));
  });
  await getOrganizationHistory("token", 2, "wrong title");
  await previewOrganization("token", input);
  await createOrganizationJob("token", { ...input, fingerprint });
  assert.equal(calls, 3);
});

test("manual preview and creation carry the same assignments and never execute", async (t) => {
  const assignments = [{ source: "unknown.mkv", identity: "1:2", size: 100, modified: "1791500000123456789", kind: "movie", tmdbId: "300" }];
  const input = { sourceId: "s", targetId: "t", path: ".", mode: "copy", assignments };
  const fingerprint = "d".repeat(64);
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(options.method, "POST");
    assert.equal(options.headers.Authorization, "token");
    assert.equal(path, calls === 1 ? "/api/v1/organization/plan" : "/api/v1/organization/jobs");
    assert.deepEqual(JSON.parse(options.body), calls === 1 ? input : { ...input, fingerprint });
    return new Response(JSON.stringify({ code: 0, success: true, data: calls === 1 ? { previewOnly: true, fingerprint } : { id: "saved", state: "ready" } }));
  });
  const preview = await previewOrganization("token", input);
  await createOrganizationJob("token", { ...input, fingerprint: preview.fingerprint });
  assert.equal(calls, 2);
});

test("partial staging is eligible for explicit disposal, not uncertain or published work", () => {
  const job = (...states) => ({ state: "needs_review", items: states.map((state) => ({ state })) });
  assert.equal(canAbandonOrganizationJob(job("staging", "planned")), true);
  assert.equal(canAbandonOrganizationJob(job("staging", "prepared")), true);
  for (const state of ["running", "completed", "moving", "quarantined", "unknown"]) assert.equal(canAbandonOrganizationJob(job("staging", state)), false);
  assert.equal(canAbandonOrganizationJob(job("planned")), false);
  assert.equal(canAbandonOrganizationJob({ state: "abandoned", items: [{ state: "abandoned" }] }), false);
  assert.equal(canAbandonOrganizationJob({ state: "abandoning", items: [{ state: "discarding" }] }), true);
});

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

test("legacy re-identification provenance loads as a readonly saved job", async (t) => {
  const legacyOrigin = { action: "redo", kind: "unidentification", recordId: "9223372036854775807", actor: "admin" };
  let calls = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => {
    calls++;
    assert.equal(path, "/api/v1/organization/jobs/legacy-job");
    assert.equal(options.method, undefined);
    assert.equal(options.headers.Authorization, "token");
    return new Response(JSON.stringify({ code: 0, success: true, data: { id: "legacy-job", legacyOrigin, state: "needs_review" } }));
  });
  assert.deepEqual((await getOrganizationJob("token", "legacy-job")).legacyOrigin, legacyOrigin);
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
