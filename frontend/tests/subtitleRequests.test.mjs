import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {createRequire} from "node:module";
import {pathToFileURL} from "node:url";
import test from "node:test";
import ts from "typescript";
import {createElement} from "react";
import {renderToStaticMarkup} from "react-dom/server";
const require=createRequire(import.meta.url);
const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const clientURL=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=url(compile(readFileSync(new URL("../src/api/subtitleRequests.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL),now=()=>new Date().toISOString();
const status={ready:true,reason:"",fingerprint:"a".repeat(64),service:"ChineseSubFinder",origin:"https://subtitle.test",http:false,roots:[{id:"movie-root",path:"/tmp/library",label:"Original library",type:"MOV"}],drivers:["","minio","rclone"]};
const review=()=>({id:"b".repeat(64),state:"review",queued:false,rootId:"movie-root",driver:"",path:"Movie.2026.mkv",name:"Movie.2026.mkv",title:"Movie",year:"2026",mediaType:"电影",tmdbId:"100",service:"ChineseSubFinder",serviceOrigin:status.origin,mappedPath:"/subtitles/Movie.2026.mkv",http:false,bytes:57,created:now(),updated:now(),fingerprint:"c".repeat(64),reason:"",serviceState:"not_queued",serviceUpdated:"",externalId:"",stoppedBy:""});
const selection={rootId:"movie-root",driver:"",path:"Movie.2026.mkv",name:"",confirmRead:true,confirmRemoteRead:false};
const consent={confirm:true,confirmPath:true,confirmExternalWrite:true,confirmOldExecutorsStopped:true,http:false,confirmRemoteRead:false};
const queued=p=>({...p,state:"queued",queued:true,serviceState:"pending",serviceUpdated:now()});
function mock(t,fn){
 const before=globalThis.fetch,calls=[];t.after(()=>{globalThis.fetch=before});
 globalThis.fetch=async(path,options)=>{calls.push({path,options});return fn(path,options,calls.length)};
 return calls;
}
const response=data=>new Response(JSON.stringify({code:0,success:true,data}),{status:200,headers:{"Content-Type":"application/json"}});

test("subtitle facts distinguish review, queued, accepted, uncertain and explicit stop",()=>{
 assert.equal(api.validSubtitleRequestStatus(status),true);const p=review();assert.equal(api.validSubtitleRequest(p),true);
 assert.equal(api.validSubtitleRequest({...queued(p),serviceState:"submitted",externalId:"service-job"}),true);
 assert.equal(api.validSubtitleRequest({...queued(p),serviceState:"uncertain"}),true);
 assert.equal(api.validSubtitleRequest({...queued(p),state:"cancelled",serviceState:"blocked",reason:"stop original",stoppedBy:"root"}),true);
 for(const bad of [
  {...p,queued:true},{...p,state:"queued"},{...queued(p),serviceState:"not_queued"},
  {...queued(p),serviceState:"submitted",externalId:""},{...queued(p),serviceState:"uncertain",externalId:"invented-success"},
  {...p,path:"../foreign.mkv"},{...p,mappedPath:"relative.mkv"},{...p,bytes:2**53},{...p,tmdbId:"9223372036854775808"},
  {...p,state:"cancelled",stoppedBy:""},{...p,serviceOrigin:"https://private-key@subtitle.test"}
 ])assert.equal(api.validSubtitleRequest(bad),false,JSON.stringify(bad));
 assert.equal(api.validSubtitleRequestStatus({...status,roots:[status.roots[0],status.roots[0]]}),false);
 assert.equal(api.validSubtitleRequestStatus({...status,http:true}),false);
});
test("subtitle review requires explicit original root and remote full-read consent before any request",async t=>{
 const p={...review(),driver:"minio"},calls=mock(t,()=>response(p));
 await assert.rejects(api.reviewSubtitleRequest("token",status,{...selection,confirmRead:false}));
 await assert.rejects(api.reviewSubtitleRequest("token",status,{...selection,rootId:"unknown"}));
 await assert.rejects(api.reviewSubtitleRequest("token",status,{...selection,driver:"minio",confirmRemoteRead:false}));
 assert.equal(calls.length,0);
 const out=await api.reviewSubtitleRequest("token",status,{...selection,driver:"minio",confirmRemoteRead:true});
 assert.equal(out.id,p.id);assert.equal(calls.length,1);assert.equal(calls[0].path,"/api/v1/organization/subtitles/review");
 const body=JSON.parse(calls[0].options.body);assert.equal(body.fingerprint,status.fingerprint);assert.equal(body.confirmRead,true);assert.equal(body.confirmRemoteRead,true);
});
test("subtitle submission needs separate file, service-write, stopped-writer, HTTP and remote grants",async t=>{
 const p={...review(),driver:"rclone",serviceOrigin:"http://subtitle.test",http:true},calls=mock(t,()=>response(queued(p))),all={...consent,http:true,confirmRemoteRead:true};
 for(const field of ["confirm","confirmPath","confirmExternalWrite","confirmOldExecutorsStopped","http","confirmRemoteRead"]){
  await assert.rejects(api.queueSubtitleRequest("token",p,{...all,[field]:false}),field);
 }
 assert.equal(calls.length,0);
 const out=await api.queueSubtitleRequest("token",p,all);assert.equal(out.state,"queued");assert.equal(calls.length,1);
 const body=JSON.parse(calls[0].options.body);
 assert.equal(body.reviewId,p.id);assert.equal(body.fingerprint,p.fingerprint);
 for(const key of ["path","rootId","mappedPath","name","identity"])assert.equal(Object.hasOwn(body,key),false);
});
test("subtitle submission never retries an uncertain response and rejects mismatched original facts",async t=>{
 const p=review(),calls=mock(t,()=>{throw Error("lost acknowledgement")});
 await assert.rejects(api.queueSubtitleRequest("token",p,consent),/lost acknowledgement/);assert.equal(calls.length,1);
 for(const field of ["id","fingerprint","path","rootId","mappedPath","serviceOrigin"]){
  globalThis.fetch=async()=>response({...queued(p),[field]:field==="serviceOrigin"?"https://other.test":field==="mappedPath"?"/other/Movie.mkv":field==="id"||field==="fingerprint"?"d".repeat(64):"Other.mkv"});
  await assert.rejects(api.queueSubtitleRequest("token",p,consent),field);
 }
});
test("expired subtitle review and a different selected media identity cannot be submitted",async t=>{
 const p=review(),calls=mock(t,()=>response({...p,tmdbId:"101"}));
 await assert.rejects(api.queueSubtitleRequest("token",{...p,created:new Date(Date.now()-11*60000).toISOString()},consent));
 await assert.rejects(api.queueSubtitleRequest("token",{...p,created:new Date(Date.now()+60000).toISOString()},consent));
 assert.equal(calls.length,0);
 await assert.rejects(api.reviewSubtitleRequest("token",status,{...selection,identity:{kind:"movie",tmdbId:"100"}}),/媒体身份/);
 assert.equal(calls.length,1);
});
test("older subtitle originals and paged lists use only bounded readonly requests",async t=>{
 const p={...queued(review()),serviceState:"uncertain"},calls=mock(t,path=>response(path.includes("?")?[p]:p));
 assert.equal((await api.getSubtitleRequest("token",p.id)).id,p.id);
 assert.equal((await api.listSubtitleRequests("token",2)).length,1);
 assert.equal(calls.every(c=>c.options.method==="GET"),true);assert.equal(calls[1].path.endsWith("?page=2"),true);
 await assert.rejects(api.getSubtitleRequest("token","short"));await assert.rejects(api.listSubtitleRequests("token",0));assert.equal(calls.length,2);
 globalThis.fetch=async()=>response(Array.from({length:31},()=>p));await assert.rejects(api.listSubtitleRequests("token",1));
 globalThis.fetch=async()=>response({...p,id:"e".repeat(64)});await assert.rejects(api.getSubtitleRequest("token",p.id));
});
test("subtitle stop applies only to the original not-sent work and does not fabricate cancellation of a service job",async t=>{
 const p=queued(review()),calls=mock(t,()=>response({...p,state:"cancelled",serviceState:"blocked",reason:"stop original",stoppedBy:"root"}));
 for(const serviceState of ["sending","uncertain","submitted"]){
  const item={...p,serviceState,...(serviceState==="submitted"?{externalId:"service-job"}:{})};
  await assert.rejects(api.cancelSubtitleRequest("token",item,true,"stop original"));
 }
 await assert.rejects(api.cancelSubtitleRequest("token",p,false,"stop original"));await assert.rejects(api.cancelSubtitleRequest("token",p,true," "));
 assert.equal(calls.length,0);
 assert.equal((await api.cancelSubtitleRequest("token",p,true,"stop original")).state,"cancelled");assert.equal(calls.length,1);
 assert.equal(calls[0].path.endsWith("/"+p.id+"/cancel"),true);assert.deepEqual(JSON.parse(calls[0].options.body),{confirm:true,reason:"stop original"});
});
test("subtitle manager starts with no scope, no consent and no automatic submission",async t=>{
 const calls=mock(t,()=>{throw Error("no implicit request")});
 let source=compile(readFileSync(new URL("../src/components/SubtitleRequestManager.tsx",import.meta.url),"utf8"));
 source=source.replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href))
  .replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href))
  .replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/subtitleRequests"',JSON.stringify(apiURL));
 const {SubtitleRequestManager}=await import(url(source));
 const html=renderToStaticMarkup(createElement(SubtitleRequestManager,{token:"fixture-token",onSessionExpired(){}}));
 assert.match(html,/为一个现有视频请求字幕/);assert.match(html,/任务被接收不代表字幕已经下载/);
 assert.doesNotMatch(html,/\schecked(?:=""|>|\s)/);assert.doesNotMatch(html,/仅提交这份原字幕请求/);assert.equal(calls.length,0);
});
