import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import ts from "typescript";
const {outputText}=ts.transpileModule(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8"),{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
const {previewRecordRename,executeRecordRename,previewCustomIdentification,executeCustomIdentification,previewRecordRedo,executeRecordRedo}=await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const hash="a".repeat(64),job="b".repeat(32),max="9223372036854775807";
const consent={confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true,confirmOldExecutorsStopped:true};
const response=(data,code=0,success=true)=>new Response(JSON.stringify({code,success,data}));
const input={flag:"unidentification",ids:[max]};
const review={flag:input.flag,fingerprint:hash,items:[{id:max,code:0,sourceRoot:"/source",targetRoot:"/cloud",plan:{mode:"rclone",fingerprint:hash,remote:{driver:"rclone"}}}]};
test("legacy remote rename requires all three independent confirmations before sending",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/unknown/rename");assert.deepEqual(JSON.parse(options.body),{unknown_id:max,syncmod:"rclone",reviewFingerprint:hash,confirmSourceRemoval:true,...consent});return response({jobId:job,state:"completed"});});
  const request={unknown_id:max,syncmod:"rclone"};
  await assert.rejects(executeRecordRename("token",request,hash,true,true));
  for(const key of Object.keys(consent))await assert.rejects(executeRecordRename("token",request,hash,true,true,{...consent,[key]:false}));
  await assert.rejects(executeRecordRename("token",request,hash,true,false,consent));assert.equal(calls,0);
  await executeRecordRename("token",request,hash,true,true,consent);assert.equal(calls,1);
});
test("custom identification previews only its separate read-only route",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{assert.equal(path,"/api/v1/organization/unknown/renameudf/plan");const body=JSON.parse(options.body);assert.equal(body.confirmRemoteExclusive,undefined);assert.equal(body.confirmSourceRemoval,undefined);return response({plan:{fingerprint:hash,mode:"rclonecopy"}});});
  await previewCustomIdentification("token",{inpath:"/source",outpath:"",syncmod:"rclonecopy",type:"MOV",tmdb:"100"});
});
test("custom remote execution binds the reviewed hash and never grants record provenance",async(t)=>{
  let calls=0;const request={inpath:"/source/Movie.mkv",outpath:"/cloud",syncmod:"rclonecopy"};
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/unknown/renameudf");assert.deepEqual(JSON.parse(options.body),{...request,reviewFingerprint:hash,confirmSourceRemoval:false,...consent});return response({jobId:job,state:"completed"});});
  await assert.rejects(executeCustomIdentification("token",request,hash,true,false));assert.equal(calls,0);
  await executeCustomIdentification("token",request,hash,true,false,consent);assert.equal(calls,1);
});
test("re-identification keeps the full signed 64-bit ID as a string",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{assert.equal(path,"/api/v1/organization/unknown/redo/plan");assert.deepEqual(JSON.parse(options.body),input);return response(review);});
  assert.equal((await previewRecordRedo("token",input)).items[0].id,max);
});
test("invalid or duplicate legacy IDs cannot be rounded into another record",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("unexpected");});
  for(const ids of [["07"],["9223372036854775808"],["7","7"],[],["1e3"]])await assert.rejects(previewRecordRedo("token",{flag:"history",ids}));assert.equal(calls,0);
});
test("redo cannot replace source, account or scope approval with a stale local review",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("unexpected");});
  await assert.rejects(executeRecordRedo("token",input,review,true,true));
  await assert.rejects(executeRecordRedo("token",input,review,true,false,consent));
  await assert.rejects(executeRecordRedo("token",{...input,ids:["7"]},review,true,true,consent));
  await assert.rejects(executeRecordRedo("token",{...input,flag:"history"},review,true,true,consent));assert.equal(calls,0);
});
test("remote redo returns per-record partial receipts without replaying completed work",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/organization/unknown/redo");assert.deepEqual(JSON.parse(options.body),{...input,reviewFingerprint:hash,confirmSourceRemoval:true,...consent});return response({results:[{id:max,jobId:job,state:"needs_review",code:409}]},2,false);});
  const result=await executeRecordRedo("token",input,review,true,true,consent);assert.equal(result.success,false);assert.equal(result.results[0].jobId,job);assert.equal(calls,1);
});
test("lost legacy remote execution returns its error without retrying or dropping a saved job ID",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:409,success:false,data:{jobId:job,state:"needs_review"}}),{status:409});});
  await assert.rejects(executeRecordRename("token",{unknown_id:max,syncmod:"rclonecopy"},hash,true,false,consent),err=>err.jobId===job);assert.equal(calls,1);
});
test("local legacy execution is not upgraded to cloud authority",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{const body=JSON.parse(options.body);assert.equal(body.confirmRemoteExclusive,undefined);assert.equal(body.confirmRemoteRecoveryCopy,undefined);return response({jobId:job,state:"completed"});});
  await executeRecordRename("token",{unknown_id:"7",syncmod:"copy"},hash,true,false);
});
test("ordinary linked preview never invokes execution or carries cloud consent",async(t)=>{
  t.mock.method(globalThis,"fetch",async(path,options)=>{assert.equal(path,"/api/v1/organization/unknown/rename/plan");assert.deepEqual(JSON.parse(options.body),{unknown_id:max,syncmod:"rclonecopy"});return response({plan:{fingerprint:hash}});});
  await previewRecordRename("token",{unknown_id:max,syncmod:"rclonecopy"});
});
