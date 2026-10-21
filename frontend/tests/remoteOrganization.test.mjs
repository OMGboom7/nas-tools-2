import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import ts from "typescript";
const load=async(path)=>{
  const {outputText}=ts.transpileModule(readFileSync(new URL(path,import.meta.url),"utf8"),{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
};
const {controlOrganizationJob,closeRemoteOrganizationJob,restoreOrganizationSource}=await load("../src/api/client.ts");
const {canAbandonOrganizationJob}=await load("../src/pages/organizationState.ts");
const id="a".repeat(32),remote={driver:"rclone",atomic:false,recoveryCopy:true,directories:false,evidence:"exclusive-namespace/full-content"};
const consent={confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true,confirmOldExecutorsStopped:true};
const response=(data)=>new Response(JSON.stringify({code:0,success:true,data}));
test("remote execution needs all independent remote acknowledgements, not source removal alone",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/jobs/${id}/execute`);assert.deepEqual(JSON.parse(options.body),{confirm:true,confirmSourceRemoval:true,...consent});return response({id,state:"completed",remote});});
  for(const missing of Object.keys(consent))await assert.rejects(controlOrganizationJob("token",id,"execute",true,{...consent,[missing]:false}));
  assert.equal(calls,0);await controlOrganizationJob("token",id,"execute",true,consent);assert.equal(calls,1);
});
test("ordinary actions never inherit remote permission and lost remote results do not replay",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;if(calls===1){assert.deepEqual(JSON.parse(options.body),{confirm:true});return response({id,state:"ready"});}assert.equal(path,`/api/v1/organization/jobs/${id}/resume-publication`);throw new Error("lost response");});
  await controlOrganizationJob("token",id,"cancel");await assert.rejects(controlOrganizationJob("token",id,"resume-publication",false,consent));assert.equal(calls,2);
});
test("remote unfinished work cannot select local destructive staging disposal",()=>{
  for(const mode of ["rclonecopy","rclone"])assert.equal(canAbandonOrganizationJob({mode,state:"needs_review",items:[{state:"prepared"}]}),false);
  assert.equal(canAbandonOrganizationJob({remote,state:"abandoning",items:[]}),false);
});
const closed={id,remote,state:"remote_closed",remoteClosure:{actor:"admin",reason:"retain all objects",created:"now"},items:[{state:"remote_stopped"}]};
test("remote close is explicit metadata-only permission, with independent stopped-executor confirmation",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/jobs/${id}/close-remote`);assert.deepEqual(JSON.parse(options.body),{confirm:true,confirmOldExecutorsStopped:true,reason:"retain all objects"});return response(closed);});
  await assert.rejects(closeRemoteOrganizationJob("token",id,"retain all objects"));await assert.rejects(closeRemoteOrganizationJob("token",id,"retain all objects",true));assert.equal(calls,0);
  assert.equal((await closeRemoteOrganizationJob("token",id,"retain all objects",true,true)).state,"remote_closed");assert.equal(calls,1);
});
test("remote close does not accept a success, wrong-job, missing-audit or still-isolated receipt",async(t)=>{
  for(const bad of [{...closed,state:"completed"},{...closed,id:"b".repeat(32)},{...closed,remote:undefined},{...closed,remoteClosure:undefined},{...closed,remoteClosure:{...closed.remoteClosure,reason:"other"}},{...closed,items:[{state:"moving"}]},{...closed,items:[{state:"completed",reason:"pending cleanup"}]}]){
    t.mock.method(globalThis,"fetch",async()=>response(bad));await assert.rejects(closeRemoteOrganizationJob("token",id,"retain all objects",true,true));
  }
});
test("restoring a remote move uses only original-source permission, not remote-write or deletion permission",async(t)=>{
  const item={index:0,source:"Movie.mkv",outcome:"isolated_source"};
  const review={jobId:id,state:"needs_review",fingerprint:"b".repeat(64),canRestore:true,reason:"",items:[item]};
  t.mock.method(globalThis,"fetch",async(path,options)=>{assert.equal(path,`/api/v1/organization/jobs/${id}/restore-source`);const body=JSON.parse(options.body);assert.equal(body.confirmRemoteExclusive,undefined);assert.equal(body.confirmSourceRemoval,undefined);return response({id,remote,mode:"rclone",state:"source_restored",items:[{index:0,source:item.source,state:"source_restored"}],sourceRestore:{jobId:id,state:"source_restored",actor:"admin",reason:"restore original",created:"now",updated:"now",targetsRetained:true,items:[{...item,outcome:"source_in_place"}]}});});
  assert.equal((await restoreOrganizationSource("token",review,"restore original",true,true)).mode,"rclone");
});
