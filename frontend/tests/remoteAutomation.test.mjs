import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import ts from "typescript";
const {outputText}=ts.transpileModule(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8"),{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
const {activateSyncMonitor,activateDownloadMonitor,controlDownloadOrganization,organizationMovesSource}=await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const hash="a".repeat(64),id="b".repeat(32);
const remote={confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true,confirmOldExecutorsStopped:true};
const download={...remote,confirmSourceRemoval:true,confirmPauseTasks:true,confirmResultWrites:true,confirmTaskRemoval:true,confirmExclusiveLabels:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("remote sync activation needs independent non-atomic/exclusive and retained-copy consent",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/sync/monitor/activate");assert.deepEqual(JSON.parse(options.body),{ids:["7"],includeExisting:false,fingerprint:hash,confirm:true,confirmOldExecutorsStopped:true,confirmSourceRemoval:true,confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true});return response({});});
  for(const key of Object.keys(remote))await assert.rejects(activateSyncMonitor("token",{ids:["7"],includeExisting:false},hash,true,true,true,{...remote,[key]:false}));
  assert.equal(calls,0);await activateSyncMonitor("token",{ids:["7"],includeExisting:false},hash,true,true,true,remote);assert.equal(calls,1);
});
test("local sync activation is not silently promoted to remote authorization",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{const body=JSON.parse(options.body);assert.equal(body.confirmRemoteExclusive,undefined);assert.equal(body.confirmRemoteRecoveryCopy,undefined);return response({});});
  await activateSyncMonitor("token",{ids:["7"],includeExisting:true},hash,true,true,false);
});
test("download monitor remote authority does not replace pause, result or removal authority",async(t)=>{
  let calls=0;const input={ids:["1"],includeExisting:true,allowOmitted:false};
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/downloads/monitor/activate");assert.deepEqual(JSON.parse(options.body),{...input,fingerprint:hash,confirm:true,...download});return response({});});
  await assert.rejects(activateDownloadMonitor("token",input,hash,true,{...download,confirmRemoteExclusive:false}));await assert.rejects(activateDownloadMonitor("token",input,hash,true,{...download,confirmRemoteRecoveryCopy:false}));await assert.rejects(activateDownloadMonitor("token",input,hash,true,{...download,confirmResultWrites:false}));assert.equal(calls,0);
  await activateDownloadMonitor("token",input,hash,true,download);assert.equal(calls,1);
});
test("remote batch execution carries separate source-removal and stopped-task consent",async(t)=>{
  let calls=0;const consent={...remote,confirmSourceRemoval:true,confirmTaskStopped:true};
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/downloads/batches/${id}/execute`);assert.deepEqual(JSON.parse(options.body),{confirm:true,...consent});return response({id,state:"files_done"});});
  for(const key of Object.keys(remote))await assert.rejects(controlDownloadOrganization("token",id,"execute",true,{...consent,[key]:false}));assert.equal(calls,0);
  await controlDownloadOrganization("token",id,"execute",true,consent);assert.equal(calls,1);
});
test("remote result reconciliation never grants remote publication or repeats an operation",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/downloads/batches/${id}/reconcile-result`);assert.deepEqual(JSON.parse(options.body),{confirm:true});throw new Error("lost response");});
  await assert.rejects(controlDownloadOrganization("token",id,"reconcile-result",true));assert.equal(calls,1);
});
test("lost remote monitor activation is not retried or upgraded into an immediate execution",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,"/api/v1/organization/downloads/monitor/activate");throw new Error("lost response");});
  await assert.rejects(activateDownloadMonitor("token",{ids:["1"],includeExisting:false,allowOmitted:false},hash,true,download));assert.equal(calls,1);
});
test("remote moving is source-destructive while remote copying is not",()=>{
  for(const mode of ["move","rclone"])assert.equal(organizationMovesSource(mode),true);
  for(const mode of ["copy","rclonecopy","link","softlink",undefined])assert.equal(organizationMovesSource(mode),false);
});
