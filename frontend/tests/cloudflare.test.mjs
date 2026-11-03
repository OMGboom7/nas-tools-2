import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}}).outputText;
const url=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const client=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const api=await import(url(compile(readFileSync(new URL("../src/api/cloudflare.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(client))));
const job={id:"a".repeat(64),origin:"fixture-original",state:"unknown",reason:"original pending",created:"fixture",updated:"fixture",canReconcile:true,canClose:true};
const status={fingerprint:"b".repeat(64),ready:true,enabled:true,current:true,fromSignin:false,nextRun:"",nextOnce:"",values:{cf_ip:"192.0.2.1",cron:"",version:"v2.3.5",additional_args:"-dd",ipv4:true,ipv6:false,check:false,onlyonce:false,re_install:false,notify:false},jobs:[job]};
const response=data=>({ok:true,status:200,json:async()=>({code:0,success:true,data})});
test("CF status preserves unknown outcomes and rejects malformed success",async t=>{
 t.mock.method(globalThis,"fetch",async()=>response(status));assert.equal((await api.getCF("fixture")).jobs[0].state,"unknown");
 assert.equal(api.cfStatusValid({...status,jobs:[{...job,state:"invented_success"}]}),false);
 assert.equal(api.cfStatusValid({...status,values:{...status.values,ipv4:"true"}}),false);
});
test("CF original receipt query checks identity and never dispatches a POST",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(options.method,"GET");assert.equal(path,"/api/v1/cloudflare/jobs/"+job.id);return response(job);});
 assert.equal((await api.getCFJob("fixture",job.id)).id,job.id);for(const invalid of ["../outside","short","A".repeat(64)])assert.throws(()=>api.getCFJob("fixture",invalid));assert.equal(calls,1);
 t.mock.method(globalThis,"fetch",async()=>response({...job,id:"c".repeat(64)}));await assert.rejects(()=>api.getCFJob("fixture",job.id));
});
test("CF lost mutation response is surfaced once and never automatically retried",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return {ok:true,status:200,json:async()=>{throw new Error("lost fixture reply")}};});
 await assert.rejects(()=>api.runCF("fixture",status.fingerprint,"same-fixture-nonce"));assert.equal(calls,1);
});
