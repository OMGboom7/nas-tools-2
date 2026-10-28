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
const apiURL=url(compile(readFileSync(new URL("../src/api/mediaSyncDelete.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL),now="2026-10-28T13:00:00Z";
const spec={enabled:true,deleteSource:false,notify:false,exclude:""};
const root={id:"source-root",path:"/tmp/source",label:"fixture source",type:"source"};
const status={installed:true,active:false,current:true,ready:true,workerRunning:true,fingerprint:"a".repeat(64),reason:"",spec,roots:[root]};
const review={id:"b".repeat(32),fingerprint:"c".repeat(64),created:now,mediaId:"original-media",http:false,spec,roots:[]};
const pending={id:"d".repeat(64),callbackId:"e".repeat(32),event:{type:"Movie",name:"Original",path:"/emby/movie.mkv",tmdbId:"100",season:0,episode:0,virtual:false,missingVirtual:false},state:"pending",parentId:"",parentState:"",parentFingerprint:"",count:0,reason:"",actor:"",created:now,updated:now,policy:spec};
const original={...pending,state:"needs_review",parentId:"f".repeat(64),parentState:"needs_review",parentFingerprint:"1".repeat(64),count:1};
const execution={receipt:original,parent:{id:original.parentId,state:"needs_review",fingerprint:"2".repeat(64),creationFingerprint:original.parentFingerprint,canContinue:true,historyIds:["9223372036854775807"],units:[],dedupeRows:2,reason:"original metadata retained"}};
const consent={confirm:true,history:true,dedupe:true,closeRelated:true,source:false,onlyCopy:false,expanded:false,legacyStopped:true,notify:false};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("media deletion readonly status and original lookup preserve stable identity",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(o.method,"GET");assert.equal(o.headers.Authorization,"token");return response(p.endsWith("?page=2")?[pending]:p.endsWith(pending.id)?pending:status)});
 assert.equal((await api.getMediaDeleteStatus("token")).active,false);assert.equal((await api.getMediaDeleteReceipt("token",pending.id)).id,pending.id);assert.equal((await api.listMediaDeleteReceipts("token",2)).length,1);
 for(const id of ["../x","A".repeat(64),"short"])await assert.rejects(api.getMediaDeleteReceipt("token",id));for(const page of [0,1.5,10001])await assert.rejects(api.listMediaDeleteReceipts("token",page));assert.equal(calls,3);
});
test("future deletion review is read-only and source roots are explicit",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(p,"/api/v1/media-sync-delete/review");assert.deepEqual(JSON.parse(o.body),{fingerprint:status.fingerprint,rootIds:[],http:false,confirmRead:true});return response(review)});
 await assert.rejects(api.reviewMediaDelete("token",status,[],false,false));await assert.rejects(api.reviewMediaDelete("token",status,[root.id],false,true));assert.equal(calls,0);
 assert.equal((await api.reviewMediaDelete("token",status,[],false,true)).id,review.id);assert.equal(calls,1);
 await assert.rejects(api.reviewMediaDelete("token",{...status,spec:{...spec,deleteSource:true}},[],false,true));await assert.rejects(api.reviewMediaDelete("token",{...status,spec:{...spec,deleteSource:true}},["unknown"],false,true));assert.equal(calls,1);
});
test("future destructive scope and selected notification need every independent consent",async t=>{
 let calls=0;const sourceReview={...review,spec:{...spec,deleteSource:true,notify:true},roots:[{id:root.id,path:root.path,identity:"inode-fixture"}]},all={...consent,source:true,onlyCopy:true,expanded:true,notify:true};
 t.mock.method(globalThis,"fetch",async()=>{calls++;return response({...status,active:true,spec:sourceReview.spec})});
 for(const field of Object.keys(all))await assert.rejects(api.authorizeMediaDelete("token",sourceReview,{...all,[field]:false}));
 assert.equal(calls,0);assert.equal((await api.authorizeMediaDelete("token",sourceReview,all)).active,true);assert.equal(calls,1);
});
test("stopping or ignoring never implies restoring deleted contents",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;if(p.endsWith("/stop")){assert.deepEqual(JSON.parse(o.body),{fingerprint:status.fingerprint,confirm:true});return response(status)}return response({...pending,state:"ignored",reason:"fixture reviewed",actor:"admin"})});
 await assert.rejects(api.stopMediaDelete("token",status,false));await assert.rejects(api.ignoreMediaDelete("token",pending,"",true));await assert.rejects(api.ignoreMediaDelete("token",pending,"fixture reviewed",false));assert.equal(calls,0);
 assert.equal((await api.stopMediaDelete("token",status,true)).active,false);assert.equal((await api.ignoreMediaDelete("token",pending,"fixture reviewed",true)).state,"ignored");assert.equal(calls,2);
});
test("original independent recovery links the exact coordinator and complete history selection",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;if(p.endsWith("/review")){assert.deepEqual(JSON.parse(o.body),{confirmRead:true});return response(execution)}assert.equal(p,"/api/v1/media-sync-delete/receipts/"+original.id+"/continue");assert.deepEqual(JSON.parse(o.body),{confirmRead:true,fingerprint:execution.parent.fingerprint,...consent});return response({...original,state:"completed",parentState:"completed"})});
 await assert.rejects(api.reviewMediaDeleteExecution("token",original,false));await assert.rejects(api.reviewMediaDeleteExecution("token",pending,true));assert.equal(calls,0);
 const v=await api.reviewMediaDeleteExecution("token",original,true);assert.equal(v.parent.historyIds[0],"9223372036854775807");
 for(const flag of ["confirm","history","dedupe","closeRelated","legacyStopped"])await assert.rejects(api.continueMediaDelete("token",v,{...consent,[flag]:false}));assert.equal(calls,1);
 assert.equal((await api.continueMediaDelete("token",v,consent)).state,"completed");assert.equal(calls,2);
});
test("lost and mismatched irreversible responses never automatically replay requests",async t=>{
 for(const out of [{...original,id:"3".repeat(64),state:"completed",parentState:"completed"},{...original,state:"running"},null]){
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(out)});await assert.rejects(api.continueMediaDelete("token",execution,consent));assert.equal(calls,1);
 }
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw Error("lost acknowledgement")});await assert.rejects(api.authorizeMediaDelete("token",review,consent));assert.equal(calls,1);
 t.mock.method(globalThis,"fetch",async()=>response({...execution,parent:{...execution.parent,creationFingerprint:"4".repeat(64)}}));await assert.rejects(api.reviewMediaDeleteExecution("token",original,true));
});
test("public deletion facts reject false completion and allow complete series above one hundred",()=>{
 assert.equal(api.validMediaDeleteStatus(status),true);assert.equal(api.validMediaDeleteReceipt(pending),true);assert.equal(api.validMediaDeleteReceipt(original),true);assert.equal(api.validMediaDeleteExecution(execution),true);
 for(const changed of [{state:"completed"},{parentId:"bad"},{count:20001},{event:{...pending.event,tmdbId:"not-id"}},{policy:undefined}])assert.equal(api.validMediaDeleteReceipt({...pending,...changed}),false);
 const many={...execution,receipt:{...original,count:151},parent:{...execution.parent,historyIds:Array.from({length:151},(_,i)=>String(i+1))}};assert.equal(api.validMediaDeleteExecution(many),true);
 assert.equal(api.validMediaDeleteExecution({...many,parent:{...many.parent,historyIds:many.parent.historyIds.slice(0,100)}}),false);
 assert.equal(api.validMediaDeleteStatus({...status,spec:undefined,active:true}),false);assert.equal(api.validMediaDeleteStatus({...status,spec:{...spec,enabled:false},active:true}),false);
});
test("authentication, malformed pages and wrong original read identities remain errors",async t=>{
 t.mock.method(globalThis,"fetch",async()=>new Response(JSON.stringify({code:401,success:false}),{status:401}));await assert.rejects(api.getMediaDeleteStatus("token"),e=>e.code===401);
 t.mock.method(globalThis,"fetch",async()=>new Response("invalid"));await assert.rejects(api.getMediaDeleteReceipt("token",pending.id));
 t.mock.method(globalThis,"fetch",async()=>response({...pending,id:"4".repeat(64)}));await assert.rejects(api.getMediaDeleteReceipt("token",pending.id));
 t.mock.method(globalThis,"fetch",async()=>response(Array(31).fill(pending)));await assert.rejects(api.listMediaDeleteReceipts("token"));
});
test("media deletion UI starts idle without selected permissions or an execution action",async()=>{
 const require=createRequire(import.meta.url);const source=compile(readFileSync(new URL("../src/components/MediaSyncDeleteManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/mediaSyncDelete"',JSON.stringify(apiURL));
 const {MediaSyncDeleteManager}=await import(url(source));const html=renderToStaticMarkup(createElement(MediaSyncDeleteManager,{token:"token",onSessionExpired(){}}));
 for(const phrase of ["Emby 同步删除管理","只读核对同步删除设置","原同步删除稳定编号","按原同步删除编号只读查回","不直接作为删除路径","缺少明确虚拟媒体标志会停用插件","不自动续跑旧计划"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!html.includes("checked="));assert.ok(!html.includes("启用这份未来同步删除授权"));assert.ok(!html.includes("立即删除"));
});
