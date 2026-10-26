import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {webcrypto} from "node:crypto";
import {createRequire} from "node:module";
import {pathToFileURL} from "node:url";
import test from "node:test";
import ts from "typescript";
import {createElement} from "react";
import {renderToStaticMarkup} from "react-dom/server";

const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const clientURL=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=url(compile(readFileSync(new URL("../src/api/fuzzySubscriptions.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL),client=await import(clientURL);
const base="/api/v1/fuzzy-subscriptions";
const candidate={title:"Movie.2026.WEB",name:"Movie",year:"2026",kind:"MOV",season:"",episodes:"",site:"Tracker",priority:0,topOrder:0,groupId:"0"};
const other={...candidate,title:"Other.2026.WEB",name:"Other"};
const review={id:"a".repeat(64),requestId:"b".repeat(32),fingerprint:"c".repeat(64),kind:"MOV",subscriptionId:"9223372036854775806",name:"Movie|Other",actor:"admin",candidates:[candidate,other],overEdition:false};
const job={id:review.id,kind:review.kind,subscriptionId:review.subscriptionId,name:review.name,actor:review.actor,requestId:review.requestId,fingerprint:review.fingerprint,revision:"d".repeat(64),authorizedBy:"admin",state:"pending",reason:"",created:"2026-10-26T03:00:00Z",sourceResult:"none",items:review.candidates.map((candidate,index)=>({index,candidate,state:"pending",reason:""}))};
const grant={kind:review.kind,id:review.subscriptionId,name:review.name,year:"",season:"",overEdition:false,actor:"admin",active:false,fingerprint:"e".repeat(64),sites:["Tracker"],workerRunning:false,targets:[{site:"Tracker",downloaderId:"1",downloader:"Original",type:"qbittorrent",directory:"/reviewed-only",paused:true}]};
const approvals={confirm:true,confirmWhole:true,confirmOldExecutorsStopped:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
const withState=(state,items=job.items)=>({...job,state,items});

test("fuzzy readonly source review preserves full integer IDs and reads no external RSS or downloader",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/MOV/"+review.subscriptionId);assert.equal(options.method,undefined);return response({review:grant,jobs:[]});
  });
  assert.equal((await api.getFuzzySource("token","MOV",review.subscriptionId)).review.targets[0].directory,"/reviewed-only");
  for(const id of ["1e3","9223372036854775808","../1","0"])await assert.rejects(api.getFuzzySource("token","MOV",id));
  assert.equal(calls,1);
});
test("future fuzzy authority needs every independent confirmation and never accepts client-uploaded endpoints",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/MOV/"+review.subscriptionId+"/authorize");
    assert.deepEqual(JSON.parse(options.body),{fingerprint:grant.fingerprint,...approvals});return response({active:true});
  });
  for(const key of Object.keys(approvals))await assert.rejects(api.setFuzzyGrant("token",grant,true,{...approvals,[key]:false}));
  assert.equal(calls,0);await api.setFuzzyGrant("token",grant,true,approvals);assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/MOV/"+review.subscriptionId+"/revoke");assert.equal(JSON.parse(options.body).confirm,true);return response({active:false});
  });
  await api.setFuzzyGrant("token",{...grant,active:true},false,{confirm:true,confirmWhole:false,confirmOldExecutorsStopped:false});assert.equal(calls,2);
});
test("fuzzy RSS preview is separate read consent and saving binds all candidates",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/MOV/"+review.subscriptionId+"/preview");assert.deepEqual(JSON.parse(options.body),{confirmRead:true});return response(review);
  });
  await assert.rejects(api.previewFuzzyBatch("token","MOV",review.subscriptionId,false));assert.equal(calls,0);
  assert.equal((await api.previewFuzzyBatch("token","MOV",review.subscriptionId,true)).candidates.length,2);
  for(const key of Object.keys(approvals))await assert.rejects(api.saveFuzzyBatch("token",review,{...approvals,[key]:false}));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/MOV/"+review.subscriptionId+"/save");
    assert.deepEqual(JSON.parse(options.body),{requestId:review.requestId,fingerprint:review.fingerprint,confirmRead:true,...approvals});return response(job);
  });
  assert.equal((await api.saveFuzzyBatch("token",review,approvals)).items.length,2);assert.equal(calls,2);
});
test("wrong or partial fuzzy receipts cannot be treated as a saved whole batch and never retry",async t=>{
  for(const bad of [
    {...job,id:"f".repeat(64)},{...job,requestId:"f".repeat(32)},{...job,actor:"other"},
    {...job,fingerprint:"f".repeat(64)},{...job,items:job.items.slice(0,1)},
    {...job,items:[job.items[0],{...job.items[1],candidate:{...other,site:"different"}}]},
  ]){
    let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(bad)});
    await assert.rejects(api.saveFuzzyBatch("token",review,approvals));assert.equal(calls,1);
  }
  assert.equal(api.validFuzzyReview({...review,candidates:[candidate,candidate]}),false);
  assert.equal(api.validFuzzyReview({...review,candidates:Array(201).fill(candidate)}),false);
  assert.equal(api.validFuzzyCandidate({...candidate,priority:Number.MAX_SAFE_INTEGER+1}),false);
  assert.equal(api.validFuzzyBatch({...job,state:"completed",sourceResult:"finished"}),false);
});
test("lost fuzzy save reply has no resend and stable batch lookup is one separate GET",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost")});
  await assert.rejects(api.saveFuzzyBatch("token",review,approvals));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,base+"/jobs/"+job.id);assert.equal(options.method,undefined);return response(job)});
  assert.equal((await api.getFuzzyBatch("token",job.id)).state,"pending");assert.equal(calls,2);
});
test("fuzzy uncertain items need exact positive receipt adoption and do not authorize continuation",async t=>{
  const unknown=withState("uncertain",[{...job.items[0],state:"uncertain"},job.items[1]]);
  const accepted=withState("uncertain",[{...job.items[0],state:"accepted"},job.items[1]]);
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/jobs/"+job.id+"/verify");assert.deepEqual(JSON.parse(options.body),{revision:job.revision,index:0,confirmRead:true,confirm:true});return response(accepted);
  });
  await assert.rejects(api.verifyFuzzyItem("token",unknown,0,false,true));await assert.rejects(api.verifyFuzzyItem("token",unknown,1,true,true));assert.equal(calls,0);
  assert.equal((await api.verifyFuzzyItem("token",unknown,0,true,true)).state,"uncertain");assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>{calls++;return response(unknown)});
  await assert.rejects(api.verifyFuzzyItem("token",unknown,0,true,true));assert.equal(calls,2);
});
test("fuzzy continuation requires a new full review and cannot replay unknown intents",async t=>{
  const blocked=withState("blocked",[{...job.items[0],state:"accepted"},job.items[1]]);
  const continuation={batch:blocked,actor:"admin",fingerprint:"f".repeat(64),canResume:true};
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/jobs/"+job.id+"/review");assert.deepEqual(JSON.parse(options.body),{confirmRead:true});return response(continuation);
  });
  await assert.rejects(api.reviewFuzzyContinuation("token",blocked,false));
  assert.equal((await api.reviewFuzzyContinuation("token",blocked,true)).canResume,true);assert.equal(calls,1);
  for(const key of Object.keys(approvals))await assert.rejects(api.resumeFuzzyBatch("token",continuation,{...approvals,[key]:false}));assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/jobs/"+job.id+"/resume");assert.deepEqual(JSON.parse(options.body),{revision:job.revision,fingerprint:continuation.fingerprint,confirmRead:true,...approvals});return response({...blocked,state:"pending"});
  });
  assert.equal((await api.resumeFuzzyBatch("token",continuation,approvals)).items[0].state,"accepted");assert.equal(calls,2);
  await assert.rejects(api.resumeFuzzyBatch("token",{...continuation,canResume:false},approvals));assert.equal(calls,2);
});
test("fuzzy stop and handler-only dispatch need separate approvals and matching receipts",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/jobs/"+job.id+"/stop");assert.deepEqual(JSON.parse(options.body),{confirm:true});return response({...job,state:"stopped"});
  });
  await assert.rejects(api.stopFuzzyBatch("token",job,false));assert.equal(calls,0);
  assert.equal((await api.stopFuzzyBatch("token",job,true)).state,"stopped");
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/jobs/"+job.id+"/execute");assert.deepEqual(JSON.parse(options.body),{revision:job.revision,...approvals});return response({...job,state:"running"});
  });
  for(const key of Object.keys(approvals))await assert.rejects(api.executeFuzzyBatch("token",job,{...approvals,[key]:false}));
  await api.executeFuzzyBatch("token",job,approvals);assert.equal(calls,2);
  await assert.rejects(api.executeFuzzyBatch("token",withState("uncertain"),approvals));assert.equal(calls,2);
});
test("fuzzy restore preserves its request identity and never restores old execution authority",async t=>{
  const completed={...job,state:"completed",sourceResult:"finished",items:job.items.map(i=>({...i,state:"accepted"}))},requestId="1".repeat(32);
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;assert.equal(path,base+"/jobs/"+job.id+"/restore");assert.deepEqual(JSON.parse(options.body),{revision:job.revision,requestId,confirm:true});
    return response({id:"5",batchId:job.id,requestId,executionAuthorized:false});
  });
  await assert.rejects(api.restoreFuzzyRule("token",completed,requestId,false));assert.equal(calls,0);
  assert.equal((await api.restoreFuzzyRule("token",completed,requestId,true)).executionAuthorized,false);
  t.mock.method(globalThis,"fetch",async()=>{calls++;return response({id:"5",batchId:job.id,requestId,executionAuthorized:true})});
  await assert.rejects(api.restoreFuzzyRule("token",completed,requestId,true));assert.equal(calls,2);
});
test("fuzzy session expiry and incomplete JSON never retry a mutation",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:401,message:"expired"}),{status:401})});
  await assert.rejects(api.saveFuzzyBatch("token",review,approvals),e=>e.code===401);assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("broken")});
  await assert.rejects(api.saveFuzzyBatch("token",review,approvals));assert.equal(calls,2);
});
test("fuzzy UI starts three independent approvals unchecked and retains IDs before awaiting writes",async()=>{
  const require=createRequire(import.meta.url),raw=readFileSync(new URL("../src/components/FuzzySubscriptionManager.tsx",import.meta.url),"utf8");
  const source=compile(raw).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/fuzzySubscriptions"',JSON.stringify(apiURL));
  const {FuzzyConsentFields,FuzzySubscriptionManager}=await import(url(source));
  const consent=renderToStaticMarkup(createElement(FuzzyConsentFields,{value:{confirm:false,confirmWhole:false,confirmOldExecutorsStopped:false},onChange(){},disabled:false}));
  assert.equal((consent.match(/type="checkbox"/g)||[]).length,3);assert.ok(!consent.includes('checked=""'));
  for(const text of ["完整批次","整个匹配种子","旧 Python","不会代为停止"])assert.ok(consent.includes(text),text);
  const html=renderToStaticMarkup(createElement(FuzzySubscriptionManager,{token:"token",selected:null,onClose(){},onSessionExpired(){},onChanged(){}}));
  for(const text of ["按稳定编号只读查回","不查库存","不是永久关键词监控","没有有效优先级","不自动重发","新授权不接管旧批次"])assert.ok(html.includes(text),text);
  const save=raw.match(/async function save\(\)\{([^\n]+)\}/)?.[1],restore=raw.match(/async function restoreRule\(\)\{([^\n]+)\}/)?.[1];
  assert.ok(save.indexOf("setLookup(review.id)")<save.indexOf("await saveFuzzyBatch"));
  assert.ok(restore.indexOf("setRestoreID(requestId)")<restore.indexOf("await restoreFuzzyRule"));
  assert.ok(!raw.match(/function invalidate\(\)\{([^\n]+)\}/)?.[1].includes("setLookup"));
  assert.ok(raw.includes("restoreTarget.current?.batch===batch.id"));
  assert.ok(raw.includes("value={futureConsent} onChange={setFutureConsent}"));
  assert.ok(raw.includes("enable?futureConsent:"));
  const find=raw.match(/async function find\(id=lookup\)\{([^\n]+)\}/)?.[1];
  assert.ok(find.includes('v.state==="completed"||v.state==="stopped"'));assert.ok(find.includes("setSource(null);onChanged()"));
  const page=readFileSync(new URL("../src/pages/SubscriptionsPage.tsx",import.meta.url),"utf8");assert.ok(page.includes("FuzzySubscriptionManager"));
});
test("ordinary history restore supplies a stable fresh request ID without changing route",async t=>{
  const previous=globalThis.crypto;Object.defineProperty(globalThis,"crypto",{value:webcrypto,configurable:true});t.after(()=>Object.defineProperty(globalThis,"crypto",{value:previous,configurable:true}));
  let calls=0,body;
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/subscriptions/history/MOV/1/redo");body=new URLSearchParams(options.body);return response({id:"3"})});
  await client.controlSubscriptionHistory("token","MOV","1","redo");assert.equal(calls,1);assert.match(body.get("request_id"),/^[a-f0-9]{32}$/);
});
