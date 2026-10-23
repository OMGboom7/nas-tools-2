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
const apiURL=url(compile(readFileSync(new URL("../src/api/reservationRelease.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(client)));
const api=await import(apiURL);
const seed={kind:"organization",id:"a".repeat(32)};
const owner={...seed,state:"active",related:false,pathCount:2,paths:["/offline/source/movie.mkv","/offline/library/Movie/movie.mkv"],claims:[{kind:"target",key:"b".repeat(64),path:""},{kind:"unknown-association",key:"9007199254740993",path:""}]};
const preview={id:"c".repeat(64),state:"preview",seed,actor:"admin",requestId:"d".repeat(32),decision:"independent full reservation release",fingerprint:"e".repeat(64),created:"",owners:[owner],claimCount:2,rowCount:6};
const saved={...preview,state:"released",created:"2026-10-23T06:00:00Z"};
const grants={confirm:true,confirmReleaseReservations:true,confirmCloseRelated:true,confirmNoContentProof:true,confirmFutureOperations:true,confirmOldExecutorsStopped:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));

test("reservation release preview is ledger-only and preserves exact identifiers",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/reservation-releases/preview");assert.deepEqual(JSON.parse(options.body),{seed,decision:preview.decision,confirmRead:true});return response(preview)});
  const v=await api.previewReservationRelease("token",seed,preview.decision,true);assert.equal(v.owners[0].claims[1].key,"9007199254740993");assert.equal(calls,1);
  for(const ref of [{...seed,id:1},{...seed,id:"a".repeat(64)},{kind:"arbitrary-table",id:seed.id}])await assert.rejects(api.previewReservationRelease("token",ref,preview.decision,true));
  await assert.rejects(api.previewReservationRelease("token",seed,preview.decision));assert.equal(calls,1);
});
test("every reservation release grant is independent and no deletion authority is sent",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/reservation-releases");const b=JSON.parse(options.body);assert.deepEqual(b,{seed,requestId:preview.requestId,decision:preview.decision,fingerprint:preview.fingerprint,confirmRead:true,...grants});assert.equal(b.confirmPermanentDeletion,undefined);return response(saved)});
  for(const key of Object.keys(grants))await assert.rejects(api.releaseReservations("token",preview,{...grants,[key]:false}));assert.equal(calls,0);
  assert.equal((await api.releaseReservations("token",preview,grants)).state,"released");assert.equal(calls,1);
  await assert.rejects(api.releaseReservations("token",saved,grants));assert.equal(calls,1);
});
test("wrong stable receipt, seed, claim scope or full owner list is never accepted or retried",async t=>{
  const other={...owner,id:"f".repeat(32),related:true};
  for(const wrong of [{...saved,id:"f".repeat(64)},{...saved,fingerprint:"f".repeat(64)},{...saved,seed:{...seed,id:other.id}},{...saved,owners:[{...owner,claims:[{...owner.claims[0],key:"f".repeat(64)},owner.claims[1]]}]},{...saved,owners:[owner,other],claimCount:4}]){
    let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(wrong)});await assert.rejects(api.releaseReservations("token",preview,grants));assert.equal(calls,1);
  }
});
test("lost reservation release response uses one request and can be found separately by stable ID",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost receipt")});await assert.rejects(api.releaseReservations("token",preview,grants));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/reservation-releases/"+preview.id);assert.equal(options.method,undefined);return response(saved)});
  assert.equal((await api.getReservationRelease("token",preview.id)).id,preview.id);assert.equal(calls,2);
});
test("reservation list pagination does not turn a page into the complete release scope",async t=>{
  const next="organization:"+seed.id;let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,"/api/v1/organization/reservation-releases?after="+encodeURIComponent(next));return response({owners:[owner],next:"",releases:[saved]})});
  assert.equal((await api.getReservationStatus("token",next)).releases[0].state,"released");assert.equal(calls,1);
  await assert.rejects(api.getReservationStatus("token","outside:anything"));assert.equal(calls,1);
  assert.equal(api.validReservationRelease({...preview,claimCount:1}),false);
  assert.equal(api.validReservationRelease({...preview,owners:[owner,owner],claimCount:4}),false);
  assert.equal(api.validReservationRelease({...preview,owners:[{...owner,claims:[{...owner.claims[0],key:9007199254740993}]}],claimCount:1}),false);
});
test("expired authentication and incomplete reservation receipts never cause a mutation retry",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:401,message:"expired"}),{status:401})});await assert.rejects(api.releaseReservations("token",preview,grants),e=>e.code===401);assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("incomplete")});await assert.rejects(api.releaseReservations("token",preview,grants));assert.equal(calls,2);
});
test("reservation release UI states negative disposition, complete scope and future writer risk separately",async()=>{
  const require=createRequire(import.meta.url),jsx=pathToFileURL(require.resolve("react/jsx-runtime")).href,react=pathToFileURL(require.resolve("react")).href;
  const raw=readFileSync(new URL("../src/pages/ReservationReleaseManager.tsx",import.meta.url),"utf8");
  const source=compile(raw).replace('"react/jsx-runtime"',JSON.stringify(jsx)).replace('"react"',JSON.stringify(react)).replace('"../api/client"',JSON.stringify(client)).replace('"../api/reservationRelease"',JSON.stringify(apiURL));
  const {ReservationConsentFields,ReservationReleaseManager}=await import(url(source));
  const consent=renderToStaticMarkup(createElement(ReservationConsentFields,{value:{},onChange(){},disabled:false}));
  for(const text of ["完整关联范围","独立同意释放","整个旧多库","不读取媒体内容","未来已授权","全部外部写入器"])assert.ok(consent.includes(text),text);
  assert.ok(!consent.includes("checked=\"\""));
  const html=renderToStaticMarkup(createElement(ReservationReleaseManager,{token:"token",enabled:true,onSessionExpired(){},onChanged(){}}));assert.ok(html.includes("按稳定编号只读查回释放结果"));assert.ok(html.includes("不要求旧根目录或云账户在线"));
  const apply=raw.match(/async function apply\(\)\{([^\n]+)\}/)?.[1];assert.ok(apply);assert.ok(apply.indexOf("setLookupId(preview.id)")<apply.indexOf("await releaseReservations"));
  assert.ok(!raw.match(/function invalidate\(\)\{([^\n]+)\}/)?.[1].includes("setLookupId"));
});
