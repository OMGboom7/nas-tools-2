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
const apiURL=url(compile(readFileSync(new URL("../src/api/mediaWebhooks.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const status={server:"emby",endpoint:"/emby",revision:"",enabled:false,current:false,ready:true,actor:"",changed:"",apiKeyRequired:true,ipv4:"192.0.2.0/24",ipv6:"",fingerprint:"a".repeat(64),reason:""};
const event={version:1,server:"emby",action:"playback.start",mediaType:"MOV",itemId:"one",name:"Movie",user:"viewer",client:"Web",device:"room",address:"192.0.2.8",overview:"",percentage:25,image:"https://emby.media/notificationicon.png",link:"https://play.test/web/#!/item?id=one"};
const receipt={id:"b".repeat(32),eventId:"c".repeat(64),received:"2026-10-27T12:00:00Z",event,deduplicated:false,deliveries:[]};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("webhook control is read-only until all independent receive and notification consent is present",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;if(o.method==="GET")return response(status);const input=JSON.parse(o.body);assert.equal(input.confirm,true);assert.equal(input.confirmNotifications,true);assert.equal(input.legacyStopped,true);return response({...status,revision:"d".repeat(32),enabled:true,current:true})});
 assert.equal((await api.reviewMediaWebhooks("token")).enabled,false);
 for(const consent of [{},{receiver:true},{receiver:true,notifications:true},{notifications:true,legacyStopped:true}])await assert.rejects(api.changeMediaWebhooks("token",status,true,consent));
 assert.equal(calls,1);await api.changeMediaWebhooks("token",status,true,{receiver:true,notifications:true,legacyStopped:true});assert.equal(calls,2);
});
test("unknown, mismatched and lost webhook control responses never retry a write",async t=>{
 const consent={receiver:true,notifications:true,legacyStopped:true};
 for(const out of [{...status,enabled:false},{...status,enabled:true,revision:"d".repeat(32),current:true,server:"plex",endpoint:"/plex"},null]){
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(out)});await assert.rejects(api.changeMediaWebhooks("token",status,true,consent));assert.equal(calls,1);
 }
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw Error("lost")});await assert.rejects(api.changeMediaWebhooks("token",status,true,consent));assert.equal(calls,1);
});
test("original webhook receipt lookup and complete bounded pages cannot trigger delivery",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.equal(o.method,"GET");return response(path.includes("?page=")?[receipt]:receipt)});
 assert.equal((await api.getMediaWebhookReceipt("token",receipt.id)).event.percentage,25);assert.equal((await api.getMediaWebhookReceipts("token",2)).length,1);
 for(const id of ["short","../x","A".repeat(32)])await assert.rejects(api.getMediaWebhookReceipt("token",id));for(const page of [0,-1,1.5,10001])await assert.rejects(api.getMediaWebhookReceipts("token",page));assert.equal(calls,2);
});
test("webhook receipt validation rejects false delivery facts and credential-bearing links",()=>{
 assert.equal(api.validMediaWebhookReceipt(receipt),true);
 for(const changed of [{...event,percentage:101},{...event,link:"https://user:pass@evil.test/"},{...event,link:"https://play.test/?api_key=secret"},{...event,version:2},{...event,action:"delete-files"}])assert.equal(api.validMediaWebhookReceipt({...receipt,event:changed}),false);
 assert.equal(api.validMediaWebhookReceipt({...receipt,deliveries:[{id:"d".repeat(64),eventId:"e".repeat(64),channelId:1,channelName:"x",channelType:"webhook",state:"sent",updated:receipt.received}]}),false);
 const delivery={id:"d".repeat(64),eventId:receipt.eventId,channelId:"9007199254740993",channelName:"large",channelType:"webhook",state:"pending",updated:receipt.received};
 assert.equal(api.validMediaWebhookReceipt({...receipt,deliveries:[delivery]}),true);
 assert.equal(api.validMediaWebhookReceipt({...receipt,deliveries:[{...delivery,channelId:9007199254740992}]}),false);
});
test("webhook read authorization failures and invalid JSON stay failures",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({success:false,code:401}),{status:401})});await assert.rejects(api.reviewMediaWebhooks("token"),e=>e.code===401);
 t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("bad")});await assert.rejects(api.getMediaWebhookReceipt("token",receipt.id));assert.equal(calls,2);
});
test("webhook UI starts without receiver consent or automatic external callbacks",async()=>{
 const require=createRequire(import.meta.url),source=compile(readFileSync(new URL("../src/components/MediaWebhookManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/mediaWebhooks"',JSON.stringify(apiURL));
 const {MediaWebhookManager}=await import(url(source));const html=renderToStaticMarkup(createElement(MediaWebhookManager,{token:"token",onSessionExpired(){}}));
 for(const phrase of ["媒体服务器回调接收","原配置不会自动启用","只读审核接收设置","按原回调编号只读查回","未提供编号时不猜测去重"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!html.includes("checked="));assert.ok(!html.includes("启用此原服务接收"));
});
