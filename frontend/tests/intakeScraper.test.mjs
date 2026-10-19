import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8"), { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { getIntakeScraper, saveIntakeScraperOptions, setIntakeScraperEnabled } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const status = { enabled: false, fingerprint: "a".repeat(64), options: { scraper_nfo: { movie: { basic: true } }, scraper_pic: {} }, fields: { scraper_nfo: { movie: { basic: false } }, scraper_pic: {} }, warnings: [], receipts: [{ id: "receipt", jobId: "job", state: "running", results: [] }] };
test("intake scraper refresh is readonly and never resumes interrupted publication", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/organization/scraper"); assert.equal(options.method, undefined); assert.equal(options.headers.Authorization, "token"); return new Response(JSON.stringify({ code: 0, success: true, data: status })); });
  assert.equal((await getIntakeScraper("token")).receipts[0].state, "running"); assert.equal(calls, 1);
});
test("saving reviewed scraper options has independent consent and never enables the YAML switch", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/organization/scraper/options"); assert.equal(options.method, "PUT"); assert.deepEqual(JSON.parse(options.body), { fingerprint: status.fingerprint, options: status.options, confirm: true }); return new Response(JSON.stringify({ code: 0, success: true, data: status })); });
  await assert.rejects(saveIntakeScraperOptions("token", status.fingerprint, status.options)); await assert.rejects(saveIntakeScraperOptions("token", "invalid", status.options, true)); assert.equal(calls, 0);
  assert.equal((await saveIntakeScraperOptions("token", status.fingerprint, status.options, true)).enabled, false); assert.equal(calls, 1);
});
test("scraper switch requires explicit boolean and consent; lost response never retries writes", async (t) => {
  let calls = 0; t.mock.method(globalThis, "fetch", async (path, options) => { calls++; assert.equal(path, "/api/v1/organization/scraper/enabled"); assert.deepEqual(JSON.parse(options.body), { enabled: true, confirm: true }); throw new Error("lost response"); });
  await assert.rejects(setIntakeScraperEnabled("token", true)); assert.equal(calls, 0); await assert.rejects(setIntakeScraperEnabled("token", true, true)); assert.equal(calls, 1);
});
test("invalid scraper responses are rejected instead of claiming success or reenabling writes", async (t) => {
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: { ...status, receipts: null } }))); await assert.rejects(getIntakeScraper("token"));
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ code: 0, success: true, data: status }))); await assert.rejects(setIntakeScraperEnabled("token", true, true));
});
