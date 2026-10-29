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
const apiURL=url(compile(readFileSync(new URL("../src/api/movieLike.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL),now="2026-10-29T00:00:00Z";
const root={id:"movie-root",path:"/tmp/library",label:"Fixture movie library",type:"MOV"};
const spec={enabled:true,directory:"Favorites",mappings:[{remote:"/emby",local:root.path}]};
const status={installed:true,active:false,current:true,ready:true,workerRunning:true,fingerprint:"a".repeat(64),reason:"",spec,roots:[root],categories:["Films","Animation"]};
const review={id:"b".repeat(32),fingerprint:"c".repeat(64),created:now,mediaId:"original-emby",http:false,spec,roots:[{id:root.id,path:root.path,configuredPath:root.path,identity:"original-inode"}],categories:status.categories};
const pending={id:"d".repeat(64),callbackId:"e".repeat(32),event:{name:"Original Movie",path:"/emby/Films/Movie/movie.mkv",itemId:"movie-one"},state:"pending",phase:"",reason:"",actor:"",created:now,updated:now};
const original={...pending,state:"needs_review",phase:"copying"};
const execution={receipt:original,move:{id:original.id,state:original.state,phase:original.phase,rootId:root.id,source:"Films/Movie",target:"Favorites/Movie",files:3,directories:3,links:1,bytes:57,sample:[".","movie.mkv","extras/readme.txt"],owners:[{kind:"library",id:"f".repeat(64),state:"recovery_closed",fingerprint:"1".repeat(64),related:false}],fingerprint:"2".repeat(64),canContinue:true,evidence:"complete original directory and content proof"}};
const grants={confirm:true,move:true,whole:true,sourceRemoval:true,closeRelated:true,legacyStopped:true,refresh:true};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("favorite readonly status, paged list and original identity never invoke a write",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(o.method,"GET");assert.equal(o.headers.Authorization,"token");return response(p.includes("?page=")?[pending]:p.endsWith(pending.id)?pending:status)});
 assert.equal((await api.getFavoriteStatus("token")).active,false);assert.equal((await api.listFavoriteReceipts("token",2)).length,1);assert.equal((await api.getFavoriteReceipt("token",pending.id)).id,pending.id);
 for(const id of ["../x","bad","A".repeat(64)])await assert.rejects(api.getFavoriteReceipt("token",id));for(const page of [0,1.5,10001])await assert.rejects(api.listFavoriteReceipts("token",page));assert.equal(calls,3);
});
test("future movie roots are explicit and an original read does not authorize moving",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(p,"/api/v1/movie-like/review");assert.deepEqual(JSON.parse(o.body),{fingerprint:status.fingerprint,rootIds:[root.id],http:false,confirmRead:true});return response(review)});
 await assert.rejects(api.reviewFavorite("token",status,[],false,true));await assert.rejects(api.reviewFavorite("token",status,["other"],false,true));await assert.rejects(api.reviewFavorite("token",status,[root.id,root.id],false,true));await assert.rejects(api.reviewFavorite("token",status,[root.id],false,false));assert.equal(calls,0);
 assert.equal((await api.reviewFavorite("token",status,[root.id],false,true)).id,review.id);assert.equal(calls,1);
});
test("all seven future movement, full-folder, source, writer and refresh grants are independent",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;assert.equal(p,"/api/v1/movie-like/authorize");assert.deepEqual(JSON.parse(o.body),{id:review.id,fingerprint:review.fingerprint,...grants});return response({...status,active:true})});
 for(const field of Object.keys(grants))await assert.rejects(api.authorizeFavorite("token",review,{...grants,[field]:false}));assert.equal(calls,0);
 assert.equal((await api.authorizeFavorite("token",review,grants)).active,true);assert.equal(calls,1);
});
test("original continuation preserves complete directory identity and needs fresh independent grants",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;if(p.endsWith("/review")){assert.deepEqual(JSON.parse(o.body),{confirmRead:true});return response(execution)}assert.equal(p,"/api/v1/movie-like/receipts/"+original.id+"/continue");assert.deepEqual(JSON.parse(o.body),{fingerprint:execution.move.fingerprint,confirmRead:true,...grants});return response({...original,state:"completed",phase:"completed"})});
 await assert.rejects(api.reviewFavoriteExecution("token",pending,true));await assert.rejects(api.reviewFavoriteExecution("token",original,false));assert.equal(calls,0);
 const out=await api.reviewFavoriteExecution("token",original,true);assert.equal(out.move.owners[0].id,"f".repeat(64));
 for(const field of Object.keys(grants))await assert.rejects(api.continueFavorite("token",out,{...grants,[field]:false}));assert.equal(calls,1);
 assert.equal((await api.continueFavorite("token",out,grants)).state,"completed");assert.equal(calls,2);
});
test("uncertain writes are never automatically retried or redirected into another original move",async t=>{
 for(const out of [null,{...original,id:"3".repeat(64),state:"completed",phase:"completed"},{...original,state:"running"}]){
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(out)});await assert.rejects(api.continueFavorite("token",execution,grants));assert.equal(calls,1);
 }
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw Error("lost acknowledgement")});await assert.rejects(api.authorizeFavorite("token",review,grants));assert.equal(calls,1);
 t.mock.method(globalThis,"fetch",async()=>response({...execution,move:{...execution.move,id:"4".repeat(64)}}));await assert.rejects(api.reviewFavoriteExecution("token",original,true));
});
test("stopping future moves or an unfinished original never claims files returned",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(p,o)=>{calls++;if(p.endsWith("/stop")){assert.deepEqual(JSON.parse(o.body),{fingerprint:status.fingerprint,confirm:true});return response(status)}assert.deepEqual(JSON.parse(o.body),{confirm:true,reason:"external review"});return response({...original,state:"ignored",reason:"external review"})});
 await assert.rejects(api.stopFavorite("token",status,false));await assert.rejects(api.ignoreFavorite("token",original,"",true));await assert.rejects(api.ignoreFavorite("token",original,"external review",false));assert.equal(calls,0);
 assert.equal((await api.stopFavorite("token",status,true)).active,false);assert.equal((await api.ignoreFavorite("token",original,"external review",true)).state,"ignored");assert.equal(calls,2);
});
test("full favorite schema refuses traversal, false completion, oversized or mismatched scopes",()=>{
 assert.equal(api.validFavoriteStatus(status),true);assert.equal(api.validFavoriteReview(review),true);assert.equal(api.validFavoriteReceipt(pending),true);assert.equal(api.validFavoriteExecution(execution),true);
 for(const changed of [{source:"../Movie"},{target:"Favorites/Other"},{files:20001},{bytes:2**50+1},{state:"completed"},{owners:[{...execution.move.owners[0],id:"unsafe"}]}])assert.equal(api.validFavoriteExecution({...execution,move:{...execution.move,...changed}}),false);
 assert.equal(api.validFavoriteReceipt({...pending,state:"completed"}),false);
 assert.equal(api.validFavoriteReview({...review,roots:[]}),false);assert.equal(api.validFavoriteReview({...review,spec:{...spec,directory:"../unsafe"}}),false);
 assert.equal(api.validFavoriteStatus({...status,categories:["../outside"]}),false);
});
test("authentication, malformed replies and unrelated original reads remain errors",async t=>{
 t.mock.method(globalThis,"fetch",async()=>new Response(JSON.stringify({code:401,success:false}),{status:401}));await assert.rejects(api.getFavoriteStatus("token"),e=>e.code===401);
 t.mock.method(globalThis,"fetch",async()=>new Response("invalid"));await assert.rejects(api.getFavoriteReceipt("token",original.id));
 t.mock.method(globalThis,"fetch",async()=>response({...pending,id:"5".repeat(64)}));await assert.rejects(api.getFavoriteReceipt("token",original.id));
 t.mock.method(globalThis,"fetch",async()=>response(Array(31).fill(pending)));await assert.rejects(api.listFavoriteReceipts("token"));
});
test("favorite UI initially stays idle with no grants, selected roots or execution action",async()=>{
 const require=createRequire(import.meta.url);const source=compile(readFileSync(new URL("../src/components/MovieLikeManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/movieLike"',JSON.stringify(apiURL));
 const {MovieLikeManager}=await import(url(source));const html=renderToStaticMarkup(createElement(MovieLikeManager,{token:"token",onSessionExpired(){}}));
 for(const phrase of ["电影精选移动管理","三组路径映射","原历史不改","不自动继续","原精选移动稳定编号","按原精选移动编号只读查回"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!html.includes("checked="));assert.ok(!html.includes("启用这份未来精选移动授权"));assert.ok(!html.includes("仅按原完整范围执行本次继续"));
});
