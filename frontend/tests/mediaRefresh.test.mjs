import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {createRequire} from "node:module";
import {pathToFileURL} from "node:url";
import test from "node:test";
import ts from "typescript";
import {createElement} from "react";
import {renderToStaticMarkup} from "react-dom/server";

const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const clientURL=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=url(compile(readFileSync(new URL("../src/api/mediaRefresh.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const client=await import(clientURL);
const plan={server:"emby",serverId:"server-1",targets:[{kind:"root",id:"root",path:"",label:"Full scan"}]};
const review={id:"a".repeat(64),requestId:"b".repeat(32),fingerprint:"c".repeat(64),jobId:"",whole:true,actor:"admin",itemCount:0,plan};
const job={id:review.id,state:"pending",reason:"",created:"2026-10-26T01:00:00Z",due:"2026-10-26T01:00:00Z",actor:"admin",source:"refresh_media_server",reference:"",server:"emby",fingerprint:review.fingerprint,plan:null,actions:[]};
const grants={confirm:true,confirmScan:true,confirmOldExecutorsStopped:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));

test("media refresh preview is readonly and never accepts client-uploaded server paths",async t=>{
  let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/library/refresh/preview");assert.deepEqual(JSON.parse(options.body),{jobId:"",whole:true,confirmRead:true});return response(review)});
  assert.equal((await api.previewMediaRefresh("token","",true,true)).plan.serverId,"server-1");
  await assert.rejects(api.previewMediaRefresh("token","",true,false));
  await assert.rejects(api.previewMediaRefresh("token","/arbitrary/path",false,true));
  await assert.rejects(api.previewMediaRefresh("token","1".repeat(32),true,true));
  assert.equal(calls,1);
});
test("media refresh submission requires every independent approval and sends one exact reviewed scope",async t=>{
  let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,"/api/v1/library/refresh/submit");
    assert.deepEqual(JSON.parse(options.body),{jobId:"",whole:true,requestId:review.requestId,fingerprint:review.fingerprint,confirmRead:true,...grants});
    assert.equal(JSON.parse(options.body).host,undefined);return response(job);
  });
  for(const key of Object.keys(grants))await assert.rejects(api.submitMediaRefresh("token",review,{...grants,[key]:false}));
  assert.equal(calls,0);
  assert.equal((await api.submitMediaRefresh("token",review,grants)).id,review.id);
  assert.equal(calls,1);
});
test("wrong media refresh receipts never imply that scanning completed or cause a retry",async t=>{
  for(const wrong of [{...job,id:"d".repeat(64)},{...job,fingerprint:"d".repeat(64)},{...job,reference:"d".repeat(32)},{...job,actor:"other"},{...job,plan:{...plan,serverId:"other"},actions:[{index:0,target:plan.targets[0],state:"accepted",reason:""}]}]){
    let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(wrong)});
    await assert.rejects(api.submitMediaRefresh("token",review,grants));assert.equal(calls,1);
  }
  assert.equal(api.validMediaRefreshReview({...review,plan:{...plan,targets:[plan.targets[0],plan.targets[0]]}}),false);
  assert.equal(api.validMediaRefreshJob({...job,state:"scan-completed"}),false);
});
test("lost media refresh response has no resend and stable receipt lookup is a separate GET",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost reply")});
  await assert.rejects(api.submitMediaRefresh("token",review,grants));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/library/refresh/jobs/"+review.id);assert.equal(options.method,undefined);return response(job)});
  assert.equal((await api.getMediaRefreshJob("token",review.id)).state,"pending");assert.equal(calls,2);
});
test("media refresh queue reads do not execute and stop is one explicitly confirmed request",async t=>{
  const status={installed:true,enabled:true,active:true,server:"emby",serverId:"server-1",actor:"admin",delay:3,workerRunning:false,jobs:[job]};
  let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/library/refresh");assert.equal(options.method,undefined);return response(status)});
  assert.equal((await api.getMediaRefreshStatus("token")).workerRunning,false);
  await assert.rejects(api.stopMediaRefreshJob("token",review.id,false));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/library/refresh/jobs/"+review.id+"/stop");assert.deepEqual(JSON.parse(options.body),{confirm:true});return response({...job,state:"stopped"})});
  assert.equal((await api.stopMediaRefreshJob("token",review.id,true)).state,"stopped");assert.equal(calls,2);
});
test("media refresh plugin activation is fingerprint-bound and cannot reuse an unchecked approval",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:0,success:true}))});
  const input={values:{enable:true,delay:"3",go_confirm:true,go_old_stopped:true,go_scan:true},clearConfig:[],fingerprint:"e".repeat(64)};
  await assert.rejects(client.savePluginConfig("token","LibraryRefresh",{...input,fingerprint:undefined}));
  for(const key of ["go_confirm","go_old_stopped","go_scan"])await assert.rejects(client.savePluginConfig("token","LibraryRefresh",{...input,values:{...input.values,[key]:false}}));
  assert.equal(calls,0);await client.savePluginConfig("token","LibraryRefresh",input);assert.equal(calls,1);
  await client.savePluginConfig("token","LibraryRefresh",{...input,values:{enable:false}});assert.equal(calls,2);
  await assert.rejects(client.savePluginConfig("token","LibraryRefresh",{...input,values:{...input.values,go_plex_login:false}}));assert.equal(calls,2);
});
test("media refresh UI separates scan acceptance, full fallback scope and stopping future requests",async()=>{
  const require=createRequire(import.meta.url);
  const raw=readFileSync(new URL("../src/components/MediaRefreshManager.tsx",import.meta.url),"utf8");
  const source=compile(raw).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/mediaRefresh"',JSON.stringify(apiURL));
  const {MediaRefreshConsentFields,MediaRefreshManager}=await import(url(source));
  const consent=renderToStaticMarkup(createElement(MediaRefreshConsentFields,{value:{},onChange(){},disabled:false}));
  assert.equal((consent.match(/type="checkbox"/g)||[]).length,3);assert.ok(!consent.includes('checked=""'));
  for(const text of ["全部目标","回退整库扫描","不等于扫描完成","实际停止"])assert.ok(consent.includes(text),text);
  const html=renderToStaticMarkup(createElement(MediaRefreshManager,{token:"token",onSessionExpired(){}}));
  for(const text of ["按稳定编号只读查回","不是把媒体服务器清单同步到本地","不强制重写全部元数据","不自动重发刷新"])assert.ok(html.includes(text),text);
  const apply=raw.match(/async function apply\(\)\{([^\n]+)\}/)?.[1];
  assert.ok(apply.indexOf("setLookupId(review.id)")<apply.indexOf("await submitMediaRefresh"));
  assert.ok(!raw.match(/function invalidate\(\)\{([^\n]+)\}/)?.[1].includes("setLookupId"));
});
test("media refresh expiry or broken JSON never retries a mutation",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:401,message:"expired"}),{status:401})});
  await assert.rejects(api.submitMediaRefresh("token",review,grants),e=>e.code===401);assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("incomplete")});
  await assert.rejects(api.submitMediaRefresh("token",review,grants));assert.equal(calls,2);
});
