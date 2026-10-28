import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import test from "node:test";
import ts from "typescript";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>`data:text/javascript;base64,${Buffer.from(s).toString("base64")}`;
const client=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=url(compile(readFileSync(new URL("../src/api/mediaRemoval.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(client)));
const api=await import(apiURL);
const s={historyIds:["9007199254740993"],flag:"del_dest",paths:[],includeNfo:true,cleanupDirectories:false,decision:"complete explicit historical media disposal",confirmRead:true};
const unit={ordinal:0,rootId:"root",driver:"minio",path:"Movie/movie.mkv",role:"library",absent:false,files:1,directories:0,links:0,bytes:8,sample:["."],events:[{historyId:s.historyIds[0],role:"library",path:"Movie/movie.mkv"}]};
const preview={id:"a".repeat(64),state:"preview",actor:"admin",requestId:"b".repeat(32),decision:s.decision,created:"",updated:"",reason:"",fingerprint:"c".repeat(64),creationFingerprint:"c".repeat(64),canContinue:true,historyIds:s.historyIds,records:[{id:s.historyIds[0],title:"Movie",year:"2026",type:"电影",mode:"Minio复制",tmdbId:"100",sourcePath:"/source",sourceFilename:"original.mkv",destPath:"/bucket/library/Movie",destFilename:"movie.mkv"}],dedupeRows:2,units:[unit],owners:[]};
const ready={...preview,state:"ready",fingerprint:"",canContinue:false,created:"now",updated:"now",reason:"saved without execution",units:[{...unit,disposal:{id:"d".repeat(64),state:"ready",rootId:unit.rootId,driver:unit.driver,path:unit.path,actor:"admin",created:"now",updated:"now",decision:s.decision,reason:"saved",total:1,done:0}}]};
const review={...ready,fingerprint:"e".repeat(64),canContinue:true};
const grants={confirm:true,confirmHistoryDeletion:true,confirmDedupeReset:true,confirmPermanentDeletion:true,confirmOnlyCopyRisk:true,confirmExpandedScope:true,confirmOldExecutorsStopped:true,confirmCloseRelated:true,confirmRemoteExclusive:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));

test("media removal preview preserves exact 64-bit history strings and is read-only",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/preview"));assert.deepEqual(JSON.parse(options.body),s);return response(preview);});
  assert.equal((await api.previewMediaRemoval("token",s)).records[0].id,"9007199254740993");assert.equal(calls,1);
  for(const ids of [[9007199254740993],["1","1"],["9223372036854775808"],["01"]])await assert.rejects(api.previewMediaRemoval("token",{...s,historyIds:ids}));assert.equal(calls,1);
  await assert.rejects(api.previewMediaRemoval("token",{...s,confirmRead:false}));assert.equal(calls,1);
});
test("record, dedupe, permanent, auxiliary, writer, closure and remote grants are independent",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/media-removals");const b=JSON.parse(options.body);assert.equal(b.requestId,preview.requestId);assert.equal(b.confirmDedupeReset,true);return response(ready);});
  for(const key of Object.keys(grants))await assert.rejects(api.createMediaRemoval("token",s,preview,{...grants,[key]:false}));assert.equal(calls,0);
  assert.equal((await api.createMediaRemoval("token",s,preview,grants)).state,"ready");assert.equal(calls,1);
});
test("wrong provider, root, path, history or creation receipt is rejected without retry",async t=>{
  for(const wrong of [{...ready,id:"f".repeat(64)},{...ready,historyIds:["2"]},{...ready,creationFingerprint:"f".repeat(64)},{...ready,units:[{...ready.units[0],driver:"rclone"}]},{...ready,units:[{...ready.units[0],rootId:"other"}]},{...ready,units:[{...ready.units[0],path:"other.mkv"}]}]){let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(wrong)});await assert.rejects(api.createMediaRemoval("token",s,preview,grants));assert.equal(calls,1);}
});
test("lost media-removal save or continue response is surfaced after exactly one call",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost receipt")});await assert.rejects(api.createMediaRemoval("token",s,preview,grants));assert.equal(calls,1);
  await assert.rejects(api.continueMediaRemoval("token",{...review,canContinue:false},grants));assert.equal(calls,1);
  await assert.rejects(api.continueMediaRemoval("token",review,grants));assert.equal(calls,2);
});
test("record-only deletion never needs or silently adds file deletion consent",async t=>{
  const p={...preview,units:[]},r={...ready,units:[]};const c={...grants,confirmPermanentDeletion:false,confirmOnlyCopyRisk:false,confirmExpandedScope:false,confirmRemoteExclusive:false};let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;const b=JSON.parse(options.body);assert.equal(b.flag,"");assert.equal(b.confirmPermanentDeletion,false);return response(r)});
  assert.equal((await api.createMediaRemoval("token",{...s,flag:""},p,c)).units.length,0);assert.equal(calls,1);
  await assert.rejects(api.createMediaRemoval("token",{...s,flag:""},p,{...c,confirmDedupeReset:false}));assert.equal(calls,1);
});
test("whole-batch stop is a separate call and never claims partial work completed",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/close"));assert.deepEqual(JSON.parse(options.body),{confirm:true,confirmClose:true});return response({...ready,state:"closed"})});
  await assert.rejects(api.stopMediaRemoval("token",ready));assert.equal(calls,0);assert.equal((await api.stopMediaRemoval("token",ready,true)).state,"closed");assert.equal(calls,1);
});
test("capture return requires its separate local no-overwrite authority",async t=>{
  const d={...ready.units[0].disposal,driver:"",state:"needs_review",capture:"Movie/.nastool-dispose-"+"d".repeat(64)};
  const v={...review,units:[{...unit,driver:"",disposal:d,review:{disposal:d,fingerprint:"f".repeat(64),canContinue:false,canReturn:true,evidence:"whole capture retained"}}]};let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.ok(path.endsWith("/return-capture"));const b=JSON.parse(options.body);assert.equal(b.confirmPermanentDeletion,undefined);assert.equal(b.ordinal,0);return response({...v,state:"closed",canContinue:false})});
  await assert.rejects(api.returnMediaRemovalCapture("token",v,0));assert.equal(calls,0);assert.equal((await api.returnMediaRemovalCapture("token",v,0,true,true)).state,"closed");assert.equal(calls,1);
});
test("expired authentication and malformed receipts never trigger a write retry",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:401,message:"expired"}),{status:401})});await assert.rejects(api.reviewMediaRemoval("token",review.id),e=>e.code===401);assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>new Response("incomplete"));await assert.rejects(api.continueMediaRemoval("token",review,grants));
  assert.equal(api.validMediaRemoval({...review,units:[{...unit,absent:true}]}),false);
  assert.equal(api.validMediaRemoval({...review,units:[{...unit,bytes:1125899906842624},{...unit,ordinal:1,bytes:1}]}),false);
});
test("linked full-series deletion stays readable above the manual limit and continues only in its own manager",async t=>{
  const historyIds=Array.from({length:151},(_,i)=>String(i+1)),records=historyIds.map(id=>({...preview.records[0],id}));
  const many={...review,mediaSyncReceipt:"f".repeat(64),historyIds,records,units:[]};
  assert.equal(api.validMediaRemoval(many),true);
  assert.equal(api.validMediaRemoval({...many,mediaSyncReceipt:undefined}),false);
  assert.equal(api.validMediaRemoval({...many,mediaSyncReceipt:"broken"}),false);
  assert.equal(api.validMediaRemoval({...many,historyIds:[...historyIds,"152"]}),false);
  const tooMany=Array.from({length:20001},(_,i)=>String(i+1));
  assert.equal(api.validMediaRemoval({...many,historyIds:tooMany,records:tooMany.map(id=>({...preview.records[0],id}))}),false);
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(many)});
  assert.equal((await api.reviewMediaRemoval("token",many.id)).historyIds.length,151);
  await assert.rejects(api.continueMediaRemoval("token",many,grants),e=>e.code===409&&e.message.includes("Emby 同步删除"));
  await assert.rejects(api.previewMediaRemoval("token",{...s,historyIds}));
  assert.equal(calls,1);
});
test("media removal UI separately explains history reset, complete directories and irreversible risk",async()=>{
  const require=createRequire(import.meta.url),jsx=pathToFileURL(require.resolve("react/jsx-runtime")).href,react=pathToFileURL(require.resolve("react")).href;
  const source=compile(readFileSync(new URL("../src/pages/MediaRemovalManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(jsx)).replace('"react"',JSON.stringify(react)).replace('"../api/client"',JSON.stringify(client)).replace('"../api/mediaRemoval"',JSON.stringify(apiURL));
  const {MediaRemovalConsentFields}=await import(url(source));const html=renderToStaticMarkup(createElement(MediaRemovalConsentFields,{value:{},onChange(){},review:preview,disabled:false}));
  for(const text of ["历史记录","同步去重行","永久删除","唯一副本","NFO","全部外部写入器","整个旧多库批次","非原子删除","不强制清版本"])assert.ok(html.includes(text),text);
});

test("media removal retains its known stable ID before a potentially lost save response",async()=>{
  const require=createRequire(import.meta.url),jsx=pathToFileURL(require.resolve("react/jsx-runtime")).href,react=pathToFileURL(require.resolve("react")).href;
  const raw=readFileSync(new URL("../src/pages/MediaRemovalManager.tsx",import.meta.url),"utf8");
  const save=raw.match(/async function save\(\)\{([^\n]+)\}/)?.[1];assert.ok(save);assert.ok(save.indexOf("setLookupId(preview.id)")<save.indexOf("await createMediaRemoval"));
  assert.ok(!raw.match(/function invalidate\(\)\{([^\n]+)\}/)?.[1].includes("setLookupId"));
  const source=compile(raw).replace('"react/jsx-runtime"',JSON.stringify(jsx)).replace('"react"',JSON.stringify(react)).replace('"../api/client"',JSON.stringify(client)).replace('"../api/mediaRemoval"',JSON.stringify(apiURL));
  const {MediaRemovalManager}=await import(url(source));const html=renderToStaticMarkup(createElement(MediaRemovalManager,{token:"token",enabled:true,onSessionExpired(){},onChanged(){}}));
  assert.ok(html.includes("只读核验这个稳定编号"));assert.ok(html.includes("保存响应丢失"));
});
