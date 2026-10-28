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
const apiURL=url(compile(readFileSync(new URL("../src/api/speedLimiter.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const spec={download:128,upload:64,idleDownload:0,idleUpload:0,bandwidth:0,interval:300,ipv4:"192.168.0.0/16",ipv6:"fd00::/8",downloaders:["9223372036854775807"],ratios:[],notify:false};
const target={id:spec.downloaders[0],name:"fixture qB",kind:"qbittorrent",weight:1};
const status={installed:true,active:false,current:true,server:"emby",cloud:false,workerRunning:true,fingerprint:"a".repeat(64),spec,reason:"",targets:[target]};
const before={upload:0,download:0,otherUpload:0,otherDownload:0,uploadLimited:false,downloadLimited:false,alternative:false,scheduled:false,unit:"B/s"};
const after={...before,upload:65536,uploadLimited:true};
const observation={playing:1,exempt:0,unknownBitrate:0,bitrate:8000000};
const now="2026-10-28T04:00:00Z";
const review={id:"b".repeat(32),fingerprint:"c".repeat(64),created:now,server:"emby",mediaId:"fixture-media",cloud:false,http:false,spec,observation,targets:[{...target,before,desired:after}]};
const job={id:"d".repeat(64),source:"webhook",reference:"media-webhook:original",state:"partial",created:now,updated:now,observation,operations:[{ordinal:0,targetId:target.id,name:target.name,kind:target.kind,dimension:"upload",before,after,state:"uncertain",reason:"response lost",actor:"",updated:now}]};
const grant={confirm:true,global:true,unlimited:true,legacyStopped:true,notify:false};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("limiter read APIs preserve stable IDs and never mutate",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(o.method,"GET");assert.equal(o.headers.Authorization,"token");return response(p.endsWith("?page=2")?[job]:p.endsWith(job.id)?job:p.endsWith("triggers")?[]:status)});
 assert.equal((await api.getLimiterStatus("token")).spec.downloaders[0],"9223372036854775807");
 assert.equal((await api.getLimiterJob("token",job.id)).id,job.id);assert.equal((await api.listLimiterJobs("token",2)).length,1);assert.equal((await api.getLimiterTriggers("token")).length,0);
 for(const id of ["../x","A".repeat(64),"short"])await assert.rejects(api.getLimiterJob("token",id));
 for(const page of [0,-1,1.5,10001])await assert.rejects(api.listLimiterJobs("token",page));assert.equal(calls,4);
});
test("limiter review compares named config values independent of JSON key order",async t=>{
 t.mock.method(globalThis,"fetch",async(p,o)=>{assert.equal(p,"/api/v1/speed-limiter/review");assert.deepEqual(JSON.parse(o.body),{fingerprint:status.fingerprint,confirm:true,http:false,plexAccount:false,plexHTTP:false});return response({...review,spec:Object.fromEntries(Object.entries(spec).reverse())})});
 assert.equal((await api.reviewLimiter("token",status,{http:false,plexAccount:false,plexHTTP:false})).id,review.id);
});
test("limiter grants require every selected independent permission before request",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(p,"/api/v1/speed-limiter/authorize");assert.deepEqual(JSON.parse(o.body),{id:review.id,fingerprint:review.fingerprint,...grant});return response({...status,active:true})});
 for(const key of ["confirm","global","unlimited","legacyStopped"])await assert.rejects(api.authorizeLimiter("token",review,{...grant,[key]:false}));
 await assert.rejects(api.authorizeLimiter("token",{...review,spec:{...spec,notify:true}},grant));assert.equal(calls,0);
 assert.equal((await api.authorizeLimiter("token",review,grant)).active,true);assert.equal(calls,1);
});
test("limiter cloud read consent and 0-unlimited acknowledgement cannot be inferred",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(review)});
 await assert.rejects(api.reviewLimiter("token",{...status,cloud:true},{http:false,plexAccount:false,plexHTTP:false}));
 await assert.rejects(api.reviewLimiter("token",status,{http:false,plexAccount:false,plexHTTP:true}));
 await assert.rejects(api.stopLimiter("token",status,false));assert.equal(calls,0);
});
test("limiter unknown operations may only verify exact original current snapshot",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(p,"/api/v1/speed-limiter/jobs/"+job.id+"/verify");assert.deepEqual(JSON.parse(o.body),{confirm:true,ordinal:0});return response({...job,operations:[{...job.operations[0],state:"verified_current",actor:"admin"}]})});
 for(const [j,n,c]of [[job,0,false],[job,1,true],[{...job,operations:[{...job.operations[0],state:"confirmed"}]},0,true]])await assert.rejects(api.verifyLimiterOperation("token",j,n,c));
 assert.equal(calls,0);assert.equal((await api.verifyLimiterOperation("token",job,0,true)).operations[0].state,"verified_current");assert.equal(calls,1);
});
test("limiter verification rejects changed target, dimension, before/after and lost response without replay",async t=>{
 for(const changed of [{targetId:"2"},{dimension:"download"},{after:{...after,upload:32768}},{before:{...before,download:1,downloadLimited:true}}]){
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response({...job,operations:[{...job.operations[0],state:"verified_current",actor:"admin",...changed}]})});await assert.rejects(api.verifyLimiterOperation("token",job,0,true));assert.equal(calls,1);
 }
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw Error("response lost")});await assert.rejects(api.authorizeLimiter("token",review,grant));assert.equal(calls,1);
});
test("limiter ignore preserves confirmed facts and requires reason plus stop confirmation",async t=>{
 let calls=0;const complete={...job.operations[0],ordinal:1,state:"confirmed",dimension:"download",before:after,after:{...after,download:131072,downloadLimited:true}};const partial={...job,operations:[job.operations[0],complete]};
 t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(p,"/api/v1/speed-limiter/jobs/"+job.id+"/ignore");assert.deepEqual(JSON.parse(o.body),{confirm:true,reason:"fixture checked"});return response({...partial,state:"ignored",operations:[{...partial.operations[0],state:"ignored",actor:"admin",reason:"fixture checked"},complete]})});
 await assert.rejects(api.ignoreLimiterJob("token",partial,true,""));await assert.rejects(api.ignoreLimiterJob("token",partial,false,"checked"));assert.equal(calls,0);
 const got=await api.ignoreLimiterJob("token",partial,true,"fixture checked");assert.equal(got.operations[1].state,"confirmed");assert.equal(calls,1);
 t.mock.method(globalThis,"fetch",async()=>response({...got,operations:got.operations.map(o=>({...o,state:"ignored",actor:"admin"}))}));await assert.rejects(api.ignoreLimiterJob("token",partial,true,"fixture checked"));
});
test("limiter public models validate units, exact large IDs, observations and receipt facts",()=>{
 assert.equal(api.validLimiterStatus(status),true);assert.equal(api.validLimiterReview(review),true);assert.equal(api.validLimiterJob(job),true);
 for(const specChange of [{download:-1},{downloaders:[9223372036854775807]},{downloaders:["9223372036854775808"]},{interval:0},{ratios:[0]}])assert.equal(api.validLimiterStatus({...status,spec:{...spec,...specChange}}),false);
 for(const change of [{upload:0.5},{upload:1},{unit:"RPC KB/s"},{download:Infinity}])assert.equal(api.validLimitRates({...before,...change},"qbittorrent"),false);
 assert.equal(api.validLimitRates({...before,unit:"RPC KB/s",upload:0.5},"transmission"),true);
 for(const o of [{...observation,unknownBitrate:2},{...observation,exempt:1000},{...observation,bitrate:-1}])assert.equal(api.validLimiterJob({...job,observation:o}),false);
 assert.equal(api.validLimiterJob({...job,operations:[{...job.operations[0],state:"verified_current"}]}),false);
});
test("limiter malformed/authentication responses and different original IDs fail without mutation",async t=>{
 t.mock.method(globalThis,"fetch",async()=>new Response(JSON.stringify({code:401,success:false}),{status:401}));await assert.rejects(api.getLimiterStatus("token"),e=>e.code===401);
 t.mock.method(globalThis,"fetch",async()=>new Response("invalid"));await assert.rejects(api.getLimiterJob("token",job.id));
 t.mock.method(globalThis,"fetch",async()=>response({...job,id:"e".repeat(64)}));await assert.rejects(api.getLimiterJob("token",job.id));
 t.mock.method(globalThis,"fetch",async()=>response(Array(31).fill(job)));await assert.rejects(api.listLimiterJobs("token"));
});
test("limiter UI is initially idle with separate read, stable lookup and no preselected permission",async()=>{
 const require=createRequire(import.meta.url);
 const source=compile(readFileSync(new URL("../src/components/SpeedLimiterManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/speedLimiter"',JSON.stringify(apiURL));
 const {SpeedLimiterManager}=await import(url(source));const html=renderToStaticMarkup(createElement(SpeedLimiterManager,{token:"token",onSessionExpired(){}}));
 for(const phrase of ["播放联动下载器限速","只读核对限速设置","原限速稳定编号","按原限速编号只读查回","读取失败不会当作无人播放","进程重启和刷新不自动重发"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!html.includes("checked="));assert.ok(!html.includes("启用这份原限速授权"));assert.ok(!html.includes("重新发送"));
});
