import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const {outputText}=ts.transpileModule(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8"),{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
const {getLibraryScraper,previewLibraryScrape,createLibraryScrape,getLibraryScrapeJob,cancelLibraryScrape,reviewLibraryScrapeOutput,recoverLibraryScrapeOutput}=await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const base="/api/v1/organization/library-scraper", hash="a".repeat(64);
const input={rootId:"movie",path:"Folder",mode:"force_all",excludes:["Folder/skip"]};
const preview={input:{...input,requestId:"1".repeat(32)},fingerprint:hash,total:1,blocked:0,skipped:0,sample:[{path:"Folder/Movie.mkv"}]};
const job={id:hash,state:"ready",mode:"force_all",created:"now",updated:"now",total:1,done:0,blocked:0,reason:""};
const output={id:"b".repeat(64),jobId:hash,target:"Folder/Movie.nfo",state:"replaced",reason:""};
const review={output,evidence:"replaced",fingerprint:"c".repeat(64),canRestore:true,canReconcile:false};
const response=(data)=>new Response(JSON.stringify({code:0,success:true,data}));
test("library refresh and scope review are readonly and do not start or restore tasks",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(options.headers.Authorization,"token");if(calls===1){assert.equal(path,base);assert.equal(options.method,undefined);return response({roots:[],jobs:[{...job,state:"running"}]});}assert.equal(path,base+"/preview");assert.deepEqual(JSON.parse(options.body),input);return response(preview);});
  assert.equal((await getLibraryScraper("token")).jobs[0].state,"running");assert.equal((await previewLibraryScrape("token",input)).input.requestId,preview.input.requestId);assert.equal(calls,2);
});
test("force scraping requires separate overwrite and stopped-writer consent",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,base+"/jobs");assert.deepEqual(JSON.parse(options.body),{...preview.input,fingerprint:hash,confirm:true,confirmOldExecutorsStopped:true,confirmOverwrite:true});return response(job);});
  await assert.rejects(createLibraryScrape("token",preview));await assert.rejects(createLibraryScrape("token",preview,true));await assert.rejects(createLibraryScrape("token",preview,true,true));assert.equal(calls,0);
  assert.equal((await createLibraryScrape("token",preview,true,true,true)).state,"ready");assert.equal(calls,1);
});
test("missing-only scraping still requires explicit one-time execution consent",async(t)=>{
  let calls=0;const p={...preview,input:{...preview.input,mode:"no_force"}};t.mock.method(globalThis,"fetch",async(_path,options)=>{calls++;assert.equal(JSON.parse(options.body).confirmOverwrite,false);return response({...job,mode:"no_force"});});
  await assert.rejects(createLibraryScrape("token",p,false,true));assert.equal(calls,0);await createLibraryScrape("token",p,true,true);assert.equal(calls,1);
});
test("lost library submission is never retried or converted into execution, cancel or recovery",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,base+"/jobs");throw new Error("lost response");});await assert.rejects(createLibraryScrape("token",preview,true,true,true));assert.equal(calls,1);
});
test("library detail pages only read and output pagination is independent of media",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,base+`/jobs/${hash}?page=2&outputPage=7`);assert.equal(options.method,undefined);return response({job,items:[],outputs:[output],page:2,outputPage:7});});
  await assert.rejects(getLibraryScrapeJob("token","bad"));assert.equal(calls,0);assert.equal((await getLibraryScrapeJob("token",hash,2,7)).outputs[0].state,"replaced");assert.equal(calls,1);
});
test("library stop is explicit and never deletes metadata or backups",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,base+`/jobs/${hash}/cancel`);assert.deepEqual(JSON.parse(options.body),{confirm:true});return response({job:{...job,state:"cancelled"},items:[],outputs:[output],page:1,outputPage:1});});await assert.rejects(cancelLibraryScrape("token",hash));assert.equal(calls,0);await cancelLibraryScrape("token",hash,true);assert.equal(calls,1);
});
test("recovery begins with readonly proof and separate consent for exactly one output",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;if(calls===1){assert.equal(path,base+`/outputs/${output.id}`);assert.equal(options.method,undefined);return response(review);}assert.equal(path,base+`/outputs/${output.id}/restore`);assert.deepEqual(JSON.parse(options.body),{fingerprint:review.fingerprint,confirm:true,confirmOldExecutorsStopped:true});return response({...output,state:"restored"});});
  const r=await reviewLibraryScrapeOutput("token",output.id);await assert.rejects(recoverLibraryScrapeOutput("token",r,"restore"));await assert.rejects(recoverLibraryScrapeOutput("token",r,"restore",true));await assert.rejects(recoverLibraryScrapeOutput("token",r,"reconcile",true,true));assert.equal(calls,1);assert.equal((await recoverLibraryScrapeOutput("token",r,"restore",true,true)).state,"restored");assert.equal(calls,2);
});
test("reconciliation never implicitly restores or replays an uncertain output",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,base+`/outputs/${output.id}/reconcile`);throw new Error("lost receipt");});const r={...review,canReconcile:true,canRestore:false};await assert.rejects(recoverLibraryScrapeOutput("token",r,"restore",true,true));assert.equal(calls,0);await assert.rejects(recoverLibraryScrapeOutput("token",r,"reconcile",true,true));assert.equal(calls,1);
});
test("incomplete library responses cannot be presented as verified success",async(t)=>{
  for(const data of [{roots:[],jobs:null},{roots:[],jobs:[{...job,done:2}]},{roots:[{}],jobs:[]}]){t.mock.method(globalThis,"fetch",async()=>response(data));await assert.rejects(getLibraryScraper("token"));}
  t.mock.method(globalThis,"fetch",async()=>response({...review,fingerprint:"invalid"}));await assert.rejects(reviewLibraryScrapeOutput("token",output.id));
  t.mock.method(globalThis,"fetch",async()=>response({...preview,input:{...preview.input,requestId:"invalid"}}));await assert.rejects(previewLibraryScrape("token",input));
});
test("cross-job and cross-output recovery responses are rejected",async(t)=>{
  t.mock.method(globalThis,"fetch",async()=>response({job:{...job,id:"d".repeat(64)},items:[],outputs:[],page:1,outputPage:1}));await assert.rejects(getLibraryScrapeJob("token",hash));
  t.mock.method(globalThis,"fetch",async()=>response({...review,output:{...output,id:"d".repeat(64)}}));await assert.rejects(reviewLibraryScrapeOutput("token",output.id));
  t.mock.method(globalThis,"fetch",async()=>response({...output,state:"restored",target:"another.nfo"}));await assert.rejects(recoverLibraryScrapeOutput("token",review,"restore",true,true));
});
