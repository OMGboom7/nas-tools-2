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
const apiURL=url(compile(readFileSync(new URL("../src/api/subscriptionFiles.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const receipt={id:"a".repeat(32),downloaderId:"9223372036854775806",downloaderType:"qbittorrent",title:"Show",subscriptionId:"2",season:1,episodes:[2],infoHash:"b".repeat(40),state:"added",sourceRecorded:false,files:[{index:0,path:"Show/S01E01.mkv",size:"9007199254740993",selected:false},{index:1,path:"Show/S01E02.mkv",size:"12",selected:true}]};
const review={receipt,current:{hash:receipt.infoHash,directory:"/downloads",labels:["NASTOOL-FILES-"+receipt.id],state:"stoppedDL",paused:true,files:receipt.files.map(f=>({...f,selected:true}))},next:"select",actor:"admin",presetEvidence:"",fingerprint:"c".repeat(64)};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("file-selection reads use stable IDs and bounded pages without side effects",async t=>{
 let count=0;t.mock.method(globalThis,"fetch",async(path,o)=>{count++;assert.equal(o.method,undefined);return response(path.includes("?page=")?[receipt]:receipt)});
 assert.equal((await api.getFileSelection("token",receipt.id)).files[0].size,"9007199254740993");assert.equal((await api.getFileSelections("token",2)).length,1);
 for(const id of ["short","../escape","A".repeat(32)])await assert.rejects(api.getFileSelection("token",id));for(const page of [0,-1,1.1,10001])await assert.rejects(api.getFileSelections("token",page));assert.equal(count,2);
});
test("file manifest validation rejects paths, aliases, indexes, loss of integer precision and false receipt completion",()=>{
 assert.equal(api.validFileSelection(receipt),true);
 for(const f of [{...receipt.files[0],path:"../x"},{...receipt.files[0],path:"C:/x"},{...receipt.files[0],path:"a//b"},{...receipt.files[0],size:9007199254740992},{...receipt.files[0],size:"9223372036854775808"},{...receipt.files[0],index:-1}]){
  assert.equal(api.validFileSelection({...receipt,files:[f,receipt.files[1]]}),false);
 }
 assert.equal(api.validFileSelection({...receipt,files:[receipt.files[1],receipt.files[1]]}),false);assert.equal(api.validFileSelection({...receipt,sourceRecorded:true}),false);
});
test("original review verifies the whole remote file list and exact task marker",async t=>{
 t.mock.method(globalThis,"fetch",async(path,o)=>{assert.ok(path.endsWith("/review"));assert.deepEqual(JSON.parse(o.body),{});return response(review)});
 assert.equal((await api.reviewFileSelection("token",receipt.id)).next,"select");
 for(const current of [{...review.current,labels:[]},{...review.current,files:review.current.files.slice(1)},{...review.current,files:[...review.current.files,{index:4,path:"extra.mkv",size:"1",selected:false}]}])assert.equal(api.validFileReview({...review,current}),false);
});
test("each consequential recovery step needs independent fresh consent",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.ok(path.endsWith("/recover"));const v=JSON.parse(o.body);assert.equal(v.fingerprint,review.fingerprint);assert.equal(v.confirm,true);return response({...receipt,state:v.confirmSettings?"added":v.confirmStart||v.confirmRecord?"ready":"selected",sourceRecorded:v.confirmRecord})});
 await assert.rejects(api.recoverFileSelection("token",review,false,{files:true}));await assert.rejects(api.recoverFileSelection("token",review,true));assert.equal(calls,0);
 await api.recoverFileSelection("token",review,true,{files:true});assert.equal(calls,1);
 for(const [next,key,state]of [["settings","settings","added"],["start","start","selected"],["record","record","ready"]]){const r={...review,next,receipt:{...receipt,state},current:{...review.current,files:receipt.files}};await assert.rejects(api.recoverFileSelection("token",r,true));await api.recoverFileSelection("token",r,true,{[key]:true})}
 assert.equal(calls,4);
});
test("lost, mismatched or incomplete recovery replies never trigger a second write",async t=>{
 for(const out of [{...receipt,id:"d".repeat(32)},{...receipt,files:receipt.files.slice(1)},{...receipt,downloaderId:"3"}]){
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(out)});await assert.rejects(api.recoverFileSelection("token",review,true,{files:true}));assert.equal(calls,1);
 }
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost response")});await assert.rejects(api.recoverFileSelection("token",review,true,{files:true}));assert.equal(calls,1);
});
test("file selection authorization expiry and invalid JSON are not retried",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:401,message:"expired"}),{status:401})});await assert.rejects(api.getFileSelection("token",receipt.id),e=>e.code===401);
 t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("bad JSON")});await assert.rejects(api.reviewFileSelection("token",receipt.id));assert.equal(calls,2);
});
test("file selection UI separates receipt reads, file choice, settings, startup and original source accounting",async()=>{
 const require=createRequire(import.meta.url),raw=readFileSync(new URL("../src/components/SubscriptionFileSelectionManager.tsx",import.meta.url),"utf8");
 const source=compile(raw).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/subscriptionFiles"',JSON.stringify(apiURL));
 const {SubscriptionFileSelectionManager}=await import(url(source));const html=renderToStaticMarkup(createElement(SubscriptionFileSelectionManager,{token:"token",lastId:"",onChanged(){},onSessionExpired(){}}));
 for(const phrase of ["缺集交集不是整包下载授权","完整种子文件清单","跨季订阅协调仍待迁移","不自动重发","按编号只读查回","只读核对原任务与下一阶段"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!raw.includes("useState(true)"));assert.ok(raw.includes('setLookup(saved.receipt.id);clearConsent();const out=await recoverFileSelection'));assert.ok(raw.includes("epoch.current++"));assert.ok(!raw.includes("setInterval"));
 assert.ok(raw.includes("entry.id===value.id?value:entry"));assert.ok(raw.includes("setReview(out);showReceipt(out.receipt)"));assert.ok(raw.includes("showReceipt(out);setNotice"));
 const page=readFileSync(new URL("../src/pages/SubscriptionsPage.tsx",import.meta.url),"utf8");assert.ok(page.includes("<SubscriptionFileSelectionManager"));assert.ok(page.includes("setFileSelectionLookup(result.fileSelectionId)"));
});
