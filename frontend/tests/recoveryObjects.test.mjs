import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {createRequire} from "node:module";
import {pathToFileURL} from "node:url";
import test from "node:test";
import ts from "typescript";
import {createElement} from "react";
import {renderToStaticMarkup} from "react-dom/server";

const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>`data:text/javascript;base64,${Buffer.from(s).toString("base64")}`;
const client=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=url(compile(readFileSync(new URL("../src/api/recoveryObjects.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(client)));
const api=await import(apiURL);
const selection={rootId:"root",driver:"minio",path:".nastool-remote/"+"a".repeat(32),decision:"independent complete recovery unit",confirmRead:true};
const preview={...selection,requestId:"b".repeat(32),fingerprint:"c".repeat(64),files:1,directories:1,links:0,bytes:8,unknown:false,owners:[{kind:"organization",id:"d".repeat(32),fingerprint:"e".repeat(64),state:"active",related:false}],sample:[".","payload"]};
const record={id:"f".repeat(64),state:"ready",rootId:"root",driver:"minio",path:selection.path,actor:"admin",decision:selection.decision,created:"now",updated:"now",reason:"saved only",total:2,done:0};
const review={disposal:record,fingerprint:"1".repeat(64),canContinue:true,canReturn:false,evidence:"full remaining inventory verified"};
const consent={confirm:true,confirmPermanentDeletion:true,confirmOnlyCopyRisk:true,confirmOldExecutorsStopped:true,confirmCloseRelated:true,confirmRemoteExclusive:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("recovery browsing and full review never request deletion or accept an expanded root/provider",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/preview"));assert.deepEqual(JSON.parse(options.body),selection);return response(preview);});assert.equal((await api.previewRecoveryObjects("token",selection)).owners[0].id.length,32);assert.equal(calls,1);
  await assert.rejects(api.previewRecoveryObjects("token",{...selection,confirmRead:false}));assert.equal(calls,1);
  for(const bad of [{...preview,driver:"rclone"},{...preview,path:"other"},{...preview,rootId:"foreign"},{...preview,sample:["../outside"]}]){t.mock.method(globalThis,"fetch",async()=>response(bad));await assert.rejects(api.previewRecoveryObjects("token",selection));}
});
test("new recovery task needs all independent irreversible grants and is not itself a delete call",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/disposals"));assert.ok(!path.endsWith("/continue"));const body=JSON.parse(options.body);assert.equal(body.confirmOnlyCopyRisk,true);assert.equal(body.requestId,preview.requestId);return response(record);});
  for(const key of Object.keys(consent)){await assert.rejects(api.createRecoveryDisposal("token",selection,preview,{...consent,[key]:false}));}assert.equal(calls,0);
  assert.equal((await api.createRecoveryDisposal("token",selection,preview,consent)).state,"ready");assert.equal(calls,1);
});
test("lost creation response or wrong-scope receipt is never retried",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost receipt");});await assert.rejects(api.createRecoveryDisposal("token",selection,preview,consent));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>response({...record,driver:"rclone"}));await assert.rejects(api.createRecoveryDisposal("token",selection,preview,consent));
});
test("continuation requires fresh full review and sends exactly one independently authorized attempt",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/continue"));assert.deepEqual(JSON.parse(options.body),{fingerprint:review.fingerprint,confirmRead:true,...consent});throw new Error("lost deletion response");});
  await assert.rejects(api.continueRecoveryDisposal("token",{...review,canContinue:false},consent));await assert.rejects(api.continueRecoveryDisposal("token",review,{...consent,confirmOnlyCopyRisk:false}));assert.equal(calls,0);await assert.rejects(api.continueRecoveryDisposal("token",review,consent));assert.equal(calls,1);
});
test("return-capture is a separate local no-overwrite action, never inherited from delete grants",async t=>{
  let calls=0;const v={...review,canContinue:false,canReturn:true,disposal:{...record,driver:"",capture:".nastool-dispose-"+record.id}};t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/return-capture"));const body=JSON.parse(options.body);assert.equal(body.confirmPermanentDeletion,undefined);return response({...v.disposal,state:"returned"});});
  await assert.rejects(api.returnRecoveryCapture("token",v));assert.equal(calls,0);assert.equal((await api.returnRecoveryCapture("token",v,true,true,true)).state,"returned");assert.equal(calls,1);await assert.rejects(api.returnRecoveryCapture("token",{...v,disposal:record},true,true,true));assert.equal(calls,1);
});
test("stop is explicit, preserves partial receipts and never turns them into success",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/close"));assert.deepEqual(JSON.parse(options.body),{confirm:true,confirmClose:true});return response({...record,state:"closed",done:1});});await assert.rejects(api.stopRecoveryDisposal("token",record.id));assert.equal(calls,0);assert.equal((await api.stopRecoveryDisposal("token",record.id,true,true)).done,1);assert.equal(calls,1);
});
test("independent recovery consent states unique-copy, closure, retention and non-atomic risks",async()=>{
  const require=createRequire(import.meta.url);const jsx=pathToFileURL(require.resolve("react/jsx-runtime")).href,react=pathToFileURL(require.resolve("react")).href;
  const source=compile(readFileSync(new URL("../src/pages/RecoveryObjectManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(jsx)).replace('"react"',JSON.stringify(react)).replace('"../api/client"',JSON.stringify(client)).replace('"../api/recoveryObjects"',JSON.stringify(apiURL));
  const {RecoveryDisposalConsent}=await import(url(source));const html=renderToStaticMarkup(createElement(RecoveryDisposalConsent,{value:{},onChange(){},remote:true,disabled:false}));for(const text of ["唯一副本","永久删除","关联整理任务","不重开","非原子","不强制清除旧版本"])assert.ok(html.includes(text),text);
});
