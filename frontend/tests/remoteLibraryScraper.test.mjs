import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import test from "node:test";
import ts from "typescript";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";

const compile=(s,jsx=ts.JsxEmit.ReactJSX)=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx}}).outputText;
const moduleURL=(s)=>`data:text/javascript;base64,${Buffer.from(s).toString("base64")}`;
const apiURL=moduleURL(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const {previewLibraryScrape,createLibraryScrape,reviewLibraryScrapeOutput,recoverLibraryScrapeOutput,getLibraryScraper}=await import(apiURL);
const remote={driver:"minio",atomic:false,recoveryCopy:true,directories:true,evidence:"exclusive-namespace/full-content"};
const input={driver:"minio",rootId:"movies",path:"Movie",mode:"force_all",excludes:[]};
const preview={input:{...input,requestId:"a".repeat(32)},fingerprint:"b".repeat(64),total:1,blocked:0,skipped:0,sample:[{path:"Movie/Movie.mkv"}],remote};
const job={id:"c".repeat(64),rootId:input.rootId,path:input.path,state:"ready",mode:"force_all",created:"now",updated:"now",total:1,done:0,blocked:0,reason:"",remote};
const output={id:"d".repeat(64),jobId:job.id,target:"Movie/Movie.nfo",state:"replaced",reason:"",remote};
const review={output,evidence:"replaced",fingerprint:"e".repeat(64),canRestore:true,canReconcile:false};
const consent={confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));

test("remote library review is readonly and binds provider without a local namesake fallback",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/library-scraper/preview");assert.deepEqual(JSON.parse(options.body),input);return response(preview);});
  assert.equal((await previewLibraryScrape("token",input)).remote.driver,"minio");assert.equal(calls,1);
  for(const bad of [{...preview,remote:undefined},{...preview,remote:{...remote,driver:"rclone"}},{...preview,input:{...preview.input,driver:""}},{...preview,remote:{...remote,atomic:true}}]){t.mock.method(globalThis,"fetch",async()=>response(bad));await assert.rejects(previewLibraryScrape("token",input));}
});
test("remote force creation requires independent overwrite and cloud consents and sends only one write",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/library-scraper/jobs");assert.deepEqual(JSON.parse(options.body),{...preview.input,...consent,fingerprint:preview.fingerprint,confirm:true,confirmOldExecutorsStopped:true,confirmOverwrite:true});return response(job);});
  await assert.rejects(createLibraryScrape("token",preview,true,true,true));await assert.rejects(createLibraryScrape("token",preview,true,true,true,{...consent,confirmRemoteExclusive:false}));await assert.rejects(createLibraryScrape("token",preview,true,true,false,consent));assert.equal(calls,0);
  assert.equal((await createLibraryScrape("token",preview,true,true,true,consent)).remote.driver,"minio");assert.equal(calls,1);
});
test("lost remote library creation never retries or silently switches provider",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost response");});await assert.rejects(createLibraryScrape("token",preview,true,true,true,consent));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>response({...job,remote:{...remote,driver:"rclone"}}));await assert.rejects(createLibraryScrape("token",preview,true,true,true,consent));
});
test("remote recovery needs a fresh independent cloud confirmation and never retries a lost restore",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;if(calls===1){assert.equal(options.method,undefined);return response(review);}assert.equal(path,`/api/v1/organization/library-scraper/outputs/${output.id}/restore`);assert.deepEqual(JSON.parse(options.body),{...consent,fingerprint:review.fingerprint,confirm:true,confirmOldExecutorsStopped:true});throw new Error("lost restore");});
  const r=await reviewLibraryScrapeOutput("token",output.id);await assert.rejects(recoverLibraryScrapeOutput("token",r,"restore",true,true));assert.equal(calls,1);await assert.rejects(recoverLibraryScrapeOutput("token",r,"restore",true,true,consent));assert.equal(calls,2);
});
test("library provider capabilities and partial receipts are never presented as automatic retry authority",async t=>{
  t.mock.method(globalThis,"fetch",async()=>response({roots:[],drivers:["","rclone","minio"],jobs:[{...job,state:"partial"}]}));assert.equal((await getLibraryScraper("token")).jobs[0].state,"partial");
  t.mock.method(globalThis,"fetch",async()=>response({roots:[],drivers:["unknown-provider"],jobs:[]}));await assert.rejects(getLibraryScraper("token"));
});
test("remote library consent explains complete reads, immutable copies and non-atomic visibility",async()=>{
  const require=createRequire(import.meta.url),jsx=pathToFileURL(require.resolve("react/jsx-runtime")).href;
  const source=compile(readFileSync(new URL("../src/components/LibraryRemoteConsent.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(jsx));
  const {LibraryRemoteConsent}=await import(moduleURL(source));const html=renderToStaticMarkup(createElement(LibraryRemoteConsent,{busy:false,exclusive:false,recovery:false,onExclusive(){},onRecovery(){}}));
  for(const text of ["远程逻辑路径","完整读取媒体","原件、新件","非原子","不自动清理","不自动重放覆盖"])assert.ok(html.includes(text),text);
});
