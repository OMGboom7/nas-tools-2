import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const client = readFileSync(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(client, {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 },
});
const { getTorrentTransferReceipt, submitPausedTransfer } = await import("data:text/javascript;base64," + Buffer.from(outputText).toString("base64"));
const id = "a".repeat(64);

test("receipt lookup reads the exact transfer and preserves an unknown start", async t => {
  const record = { receipt: id, targetStage: "verified", actions: [
    { action: "start", state: "unknown", updated: "2026-11-25T00:00:00Z" },
  ] };
  const fetch = t.mock.method(globalThis, "fetch", async (url, init) => {
    assert.equal(url, "/api/v1/torrent-transfer/records/" + id);
    assert.equal(init.headers.Authorization, "test-token");
    return Response.json({ code: 0, result: record });
  });
  assert.deepEqual(await getTorrentTransferReceipt("test-token", id), record);
  await assert.rejects(getTorrentTransferReceipt("test-token", "invalid"), { code: 400 });
  assert.equal(fetch.mock.callCount(), 1);
  record.receipt = "b".repeat(64);
  await assert.rejects(getTorrentTransferReceipt("test-token", id), { code: 502 });
});

test("paused submission returns the original receipt for a previous attempt", async t => {
  const prior = {receipt: "b".repeat(64), state: "unknown"};
  const fetch = t.mock.method(globalThis, "fetch", async (url, init) => {
    assert.equal(url, "/api/v1/torrent-transfer/records/" + id + "/submit");
    assert.equal(init.method, "POST");
    assert.deepEqual(JSON.parse(init.body), {confirmPaused: true});
    return Response.json({code: 0, result: prior});
  });
  assert.deepEqual(await submitPausedTransfer("fixture", id, new AbortController().signal), prior);
  assert.equal(fetch.mock.callCount(), 1);
});
