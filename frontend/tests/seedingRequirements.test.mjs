import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

// Compile with the project's existing TypeScript dependency, without requiring
// a new test runner or Node's version-specific TypeScript support.
const source = readFileSync(new URL("../src/utils/seedingRequirements.ts", import.meta.url), "utf8");
const { outputText } = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022 } });
const { seedingRequirements } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);

test("missing seeding requirements stay unknown, explicit zero stays known", () => {
  assert.deepEqual(seedingRequirements({}), ["做种时长未知", "分享率要求未知"]);
  assert.deepEqual(seedingRequirements({ minimumSeedTime: 0, minimumRatio: 0 }), ["最低做种 0 秒", "最低分享率 0"]);
  assert.deepEqual(seedingRequirements({ minimumRatio: 0.8 }), ["做种时长未知", "最低分享率 0.8"]);
});

test("seeding duration retains seconds and fractions of hours", () => {
  for (const [seconds, expected] of [[1, "1 秒"], [61, "1 分 1 秒"], [90000, "1 天 1 小时"], [90061, "1 天 1 小时 1 分 1 秒"]]) {
    assert.equal(seedingRequirements({ minimumSeedTime: seconds })[0], `最低做种 ${expected}`);
  }
});

test("malformed API values are not displayed as valid requirements", () => {
  for (const value of [-1, NaN, Infinity, "0", null]) {
    assert.deepEqual(seedingRequirements({ minimumSeedTime: value, minimumRatio: value }), ["做种时长未知", "分享率要求未知"]);
  }
  assert.equal(seedingRequirements({ minimumSeedTime: 0.5 })[0], "做种时长未知");
});
