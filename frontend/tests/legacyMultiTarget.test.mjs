import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import ts from "typescript";
const {outputText}=ts.transpileModule(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8"),{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
const {executeRecordRedo,executeRecordRename,getLegacyOrganizationBatch,controlLegacyOrganizationBatch}=await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const id="a".repeat(32),hash="b".repeat(64),job="c".repeat(32);
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
const input={flag:"unidentification",ids:["7"]},review={flag:input.flag,fingerprint:hash,items:[{id:"7",code:0,plan:{mode:"copy",partitions:[{targetId:"movie",targetRoot:"/movie"},{targetId:"tv",targetRoot:"/tv"}]}}]};
const batch={id,fingerprint:hash,reviewFingerprint:hash,state:"ready",jobs:[job],mode:"copy",targets:["/movie","/tv"]};
test("native multi-target redo requires full-scope stopped-writer acknowledgement",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/unknown/redo");const body=JSON.parse(options.body);assert.equal(body.confirmOldExecutorsStopped,true);assert.equal(body.confirmRemoteExclusive,undefined);return response({results:[{id:"7",jobId:job,batchId:id,jobs:[job],state:"completed",code:0}]});});
  await assert.rejects(executeRecordRedo("token",input,review,true,false));assert.equal(calls,0);
  const result=await executeRecordRedo("token",input,review,true,false,{confirmOldExecutorsStopped:true,confirmRemoteExclusive:false,confirmRemoteRecoveryCopy:false});assert.equal(result.results[0].batchId,id);assert.equal(calls,1);
});
test("native linked rename carries stopped-writer consent without granting cloud access",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{const body=JSON.parse(options.body);assert.equal(body.confirmOldExecutorsStopped,true);assert.equal(body.confirmRemoteExclusive,undefined);assert.equal(body.confirmRemoteRecoveryCopy,undefined);return response({jobId:job,batchId:id,jobs:[job],state:"completed"});});
  const result=await executeRecordRename("token",{unknown_id:"7",syncmod:"copy"},hash,true,false,{confirmOldExecutorsStopped:true,confirmRemoteExclusive:false,confirmRemoteRecoveryCopy:false});assert.equal(result.batchId,id);
});
test("reading a batch does not execute, resume, close or confer authority",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{assert.equal(path,`/api/v1/organization/legacy/batches/${id}`);assert.equal(options.method,undefined);assert.equal(options.body,undefined);return response(batch);});await getLegacyOrganizationBatch("token",id);
});
test("readonly full-batch reconciliation carries no remote publication authority",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{assert.equal(path,`/api/v1/organization/legacy/batches/${id}/reconcile`);assert.deepEqual(JSON.parse(options.body),{confirm:true,reviewFingerprint:hash});return response({...batch,state:"completed"});});await controlLegacyOrganizationBatch("token",batch,"reconcile",{confirm:true});
});
test("lost batch execution is sent once and never converted into a retry or reconcile",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,`/api/v1/organization/legacy/batches/${id}/execute`);throw new Error("lost response");});await assert.rejects(controlLegacyOrganizationBatch("token",batch,"execute",{confirm:true,confirmOldExecutorsStopped:true}));assert.equal(calls,1);
});
test("remote reopening cannot borrow source removal or retained-copy approval",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("unexpected");});
  const remote={...batch,mode:"rclone",state:"closing"};
  await assert.rejects(controlLegacyOrganizationBatch("token",remote,"resume",{confirm:true,confirmOldExecutorsStopped:true,reason:"review"}));
  await assert.rejects(controlLegacyOrganizationBatch("token",remote,"execute",{confirm:true,confirmOldExecutorsStopped:true,confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true}));assert.equal(calls,0);
});
test("batch closing is an explicit fresh-scope action, not a successful transfer",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,`/api/v1/organization/legacy/batches/${id}/close`);assert.deepEqual(JSON.parse(options.body),{confirm:true,confirmOldExecutorsStopped:true,reason:"all children inspected",reviewFingerprint:hash});return response({...batch,state:"closed",unknownState:"cancelled"});});
  await assert.rejects(controlLegacyOrganizationBatch("token",batch,"close",{confirm:true,reason:"review"}));assert.equal(calls,0);
  const result=await controlLegacyOrganizationBatch("token",{...batch,state:"closing"},"close",{confirm:true,confirmOldExecutorsStopped:true,reason:"all children inspected"});assert.equal(result.unknownState,"cancelled");assert.equal(calls,1);
});
