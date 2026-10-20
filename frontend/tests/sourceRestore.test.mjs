import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import ts from "typescript";
const {outputText}=ts.transpileModule(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8"),{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
const {reviewOrganizationSourceRestore,restoreOrganizationSource}=await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const id="1".repeat(32),hash="a".repeat(64);
const items=[{index:0,source:"Movie.mkv",outcome:"isolated_source"},{index:1,source:"Movie.srt",outcome:"not_started"}];
const review={jobId:id,state:"needs_review",fingerprint:hash,canRestore:true,reason:"",items};
const receipt={jobId:id,state:"source_restored",actor:"admin",reason:"restore original",created:"now",updated:"later",targetsRetained:true,items:[{...items[0],outcome:"source_in_place"},items[1]]};
const job={id,mode:"move",state:"source_restored",sourceRestore:receipt,items:[{index:0,source:"Movie.mkv",state:"source_restored"},{index:1,source:"Movie.srt",state:"source_restore_stopped"}]};
const response=(data)=>new Response(JSON.stringify({code:0,success:true,data}));
test("source recovery review only reads source evidence and never starts file actions",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/jobs/${id}/source-restore`);assert.equal(options.method,undefined);assert.equal(options.headers.Authorization,"token");return response(review);});
  assert.equal((await reviewOrganizationSourceRestore("token",id)).items[0].outcome,"isolated_source");await assert.rejects(reviewOrganizationSourceRestore("token","bad"));assert.equal(calls,1);
});
test("source restoration requires separate restored-source and stopped-writer consent before sending",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/jobs/${id}/restore-source`);assert.deepEqual(JSON.parse(options.body),{confirm:true,confirmRestoreSource:true,confirmOldExecutorsStopped:true,reviewFingerprint:hash,reason:"restore original"});return response(job);});
  await assert.rejects(restoreOrganizationSource("token",review,"restore original"));await assert.rejects(restoreOrganizationSource("token",review,"restore original",true));await assert.rejects(restoreOrganizationSource("token",review," ",true,true));await assert.rejects(restoreOrganizationSource("token",{...review,canRestore:false},"restore original",true,true));assert.equal(calls,0);
  assert.equal((await restoreOrganizationSource("token",review,"restore original",true,true)).state,"source_restored");assert.equal(calls,1);
});
test("an interrupted restore needs a fresh explicit review and never implies continuing a move or deleting staging",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/jobs/${id}/restore-source`);const body=JSON.parse(options.body);assert.equal(body.reviewFingerprint,"b".repeat(64));assert.equal(body.confirmSourceRemoval,undefined);assert.equal(body.confirmDiscardStaging,undefined);return response(job);});
  await restoreOrganizationSource("token",{...review,state:"source_restoring",fingerprint:"b".repeat(64)},"restore original",true,true);assert.equal(calls,1);
});
test("lost restoration response never retries, continues a move, reconciles history or releases targets",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,`/api/v1/organization/jobs/${id}/restore-source`);throw new Error("lost response");});await assert.rejects(restoreOrganizationSource("token",review,"restore original",true,true));assert.equal(calls,1);
});
test("stale cross-job scopes and malformed positive source evidence cannot authorize restoration",async(t)=>{
  for(const bad of [{...review,jobId:"2".repeat(32)},{...review,fingerprint:"bad"},{...review,canRestore:"yes"},{...review,items:[items[0],items[0]]},{...review,items:[{...items[0],outcome:"blocked"}]},{...review,state:"completed"}]){t.mock.method(globalThis,"fetch",async()=>response(bad));await assert.rejects(reviewOrganizationSourceRestore("token",id));}
});
test("source receipt never claims a transfer completed or accepts omitted retained-target guarantees",async(t)=>{
  for(const bad of [{...job,id:"2".repeat(32)},{...job,state:"completed"},{...job,mode:"copy"},{...job,sourceRestore:{...receipt,targetsRetained:false}},{...job,sourceRestore:{...receipt,reason:"another reason"}},{...job,sourceRestore:{...receipt,items:[{...receipt.items[0],source:"another.mkv"},receipt.items[1]]}},{...job,items:[job.items[1],job.items[0]]}]){t.mock.method(globalThis,"fetch",async()=>response(bad));await assert.rejects(restoreOrganizationSource("token",review,"restore original",true,true));}
});
