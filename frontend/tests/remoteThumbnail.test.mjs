import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import test from "node:test";
import ts from "typescript";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

const settings = { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX };
const require = createRequire(import.meta.url);
const resolveURL = (name) => pathToFileURL(require.resolve(name)).href;
const compile = (source) => ts.transpileModule(source, { compilerOptions: settings }).outputText;
const asModule = (source) => `data:text/javascript;base64,${Buffer.from(source).toString("base64")}`;
const apiModule = asModule(compile(readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8")));
const { getIntakeScraper, saveIntakeScraperOptions } = await import(apiModule);
const source = readFileSync(new URL("../src/pages/IntakeScraperManager.tsx", import.meta.url), "utf8")
  .replace('"react"', JSON.stringify(resolveURL("react")))
  .replace('"../api/client"', JSON.stringify(apiModule));
const component = compile(source).replace('"react/jsx-runtime"', JSON.stringify(resolveURL("react/jsx-runtime")));
const { IntakeScraperManager } = await import(asModule(component));
const status = { enabled: false, fingerprint: "b".repeat(64), options: { scraper_nfo: {}, scraper_pic: { tv: { episode_thumb: true, episode_thumb_ffmpeg: true } } }, fields: { scraper_nfo: {}, scraper_pic: { tv: { episode_thumb: false, episode_thumb_ffmpeg: false } } }, warnings: [], receipts: [] };

test("remote thumbnail UI explains verified cloud reads, budgets and no automatic retry without enabling writes", (t) => {
  let requests = 0;
  t.mock.method(globalThis, "fetch", async () => { requests++; throw new Error("render must be readonly"); });
  const markup = renderToStaticMarkup(createElement(IntakeScraperManager, { token: "token", enabled: true, onSessionExpired() {}, onChanged() {} }));
  for (const text of ["Rclone／MinIO", "完整视频核验", "1 TiB", "512 MiB／90 秒", "不落盘保存第二份视频", "蓝光不猜选内部片段", "不自动重试"]) assert.ok(markup.includes(text), text);
  assert.equal(requests, 0);
});

test("remote thumbnail failure receipts remain partial on readonly refresh and never trigger a decoder retry", async (t) => {
  let requests = 0;
  const receipt = { id: "receipt", jobId: "remote-job", name: "Show.S01E02", state: "partial", results: [{ name: "Show.S01E02-thumb.jpg", state: "blocked", reason: "video thumbnail unavailable or media changed; no image published" }] };
  t.mock.method(globalThis, "fetch", async (path, options) => { requests++; assert.equal(path, "/api/v1/organization/scraper"); assert.equal(options.method, undefined); return new Response(JSON.stringify({ code: 0, success: true, data: { ...status, receipts: [receipt] } })); });
  assert.deepEqual((await getIntakeScraper("token")).receipts, [receipt]);
  assert.equal(requests, 1);
});

test("enabling the reviewed FFmpeg option only saves settings and never retries a lost save or enables background scraping", async (t) => {
  let requests = 0;
  t.mock.method(globalThis, "fetch", async (path, options) => { requests++; assert.equal(path, "/api/v1/organization/scraper/options"); assert.equal(options.method, "PUT"); assert.deepEqual(JSON.parse(options.body), { fingerprint: status.fingerprint, options: status.options, confirm: true }); throw new Error("lost response"); });
  await assert.rejects(saveIntakeScraperOptions("token", status.fingerprint, status.options, true));
  assert.equal(requests, 1);
});
