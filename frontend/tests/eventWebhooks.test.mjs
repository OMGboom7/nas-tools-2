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
const apiURL=url(compile(readFileSync(new URL("../src/api/eventWebhooks.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const status={active:false,epoch:"",actor:"",created:"",origin:"",method:"",encoding:"",current:true,workerRunning:true};
const receipt={id:"a".repeat(64),eventId:"b".repeat(64),type:"emby.webhook",reference:"media-webhook:original",state:"uncertain",reason:"receiver response not confirmed",actor:"",created:"2026-10-28T02:00:00Z",updated:"2026-10-28T02:01:00Z",status:0};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("event webhook status and receipt browsing are read-only and retain original stable identity",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.equal(o.method,"GET");assert.equal(o.headers.Authorization,"token");return response(path.includes("?page=")?[receipt]:path.endsWith(receipt.id)?receipt:status)});
 assert.equal((await api.reviewEventWebhooks("token")).active,false);assert.equal((await api.listEventWebhooks("token",2)).length,1);assert.equal((await api.getEventWebhook("token",receipt.id)).id,receipt.id);assert.equal(calls,3);
 for(const page of [0,-1,1.5,10001])await assert.rejects(api.listEventWebhooks("token",page));
 for(const id of ["short","../x","A".repeat(64)])await assert.rejects(api.getEventWebhook("token",id));assert.equal(calls,3);
});
test("event webhook dismissal needs original receipt, independent confirmation and reason",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.equal(path,"/api/v1/event-webhooks/receipts/"+receipt.id+"/dismiss");assert.equal(o.method,"POST");assert.deepEqual(JSON.parse(o.body),{confirm:true,reason:"reviewed receiver"});return response({...receipt,state:"dismissed",actor:"admin",reason:"reviewed receiver"})});
 for(const [p,c,r]of [[receipt,false,"reviewed"],[receipt,true,""],[{...receipt,state:"sent",status:200},true,"reviewed"],[{...receipt,id:"wrong"},true,"reviewed"]])await assert.rejects(api.dismissEventWebhook("token",p,c,r));
 assert.equal(calls,0);assert.equal((await api.dismissEventWebhook("token",receipt,true,"reviewed receiver")).state,"dismissed");assert.equal(calls,1);
});
test("lost, malformed and mismatched dismissal responses never replay the write",async t=>{
 for(const out of [{...receipt,state:"pending"},{...receipt,state:"dismissed",actor:"admin",reason:"reviewed",eventId:"c".repeat(64)},null]){
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(out)});await assert.rejects(api.dismissEventWebhook("token",receipt,true,"reviewed"));assert.equal(calls,1);
 }
 let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw Error("response lost")});await assert.rejects(api.dismissEventWebhook("token",receipt,true,"reviewed"));assert.equal(calls,1);
});
test("event webhook public validation rejects false delivery facts and credential-bearing origins",()=>{
 assert.equal(api.validEventWebhookReceipt(receipt),true);
 for(const changed of [{state:"sent",status:0},{state:"dismissed",reason:"",actor:"admin"},{type:"arbitrary.event"},{eventId:"wrong"},{updated:"invalid"},{status:700}])assert.equal(api.validEventWebhookReceipt({...receipt,...changed}),false);
 assert.equal(api.validEventWebhookStatus(status),true);
 for(const origin of ["https://user:secret@receiver.test","https://receiver.test/?token=private","https://receiver.test/path","file:///tmp/data"])assert.equal(api.validEventWebhookStatus({...status,origin}),false);
 assert.equal(api.validEventWebhookStatus({...status,active:true}),false);
});
test("event webhook authentication and malformed read responses remain errors",async t=>{
 t.mock.method(globalThis,"fetch",async()=>new Response(JSON.stringify({code:401,success:false}),{status:401}));await assert.rejects(api.reviewEventWebhooks("token"),e=>e.code===401);
 t.mock.method(globalThis,"fetch",async()=>new Response("invalid"));await assert.rejects(api.getEventWebhook("token",receipt.id));
 t.mock.method(globalThis,"fetch",async()=>response({...receipt,id:"c".repeat(64)}));await assert.rejects(api.getEventWebhook("token",receipt.id));
 t.mock.method(globalThis,"fetch",async()=>response(Array(31).fill(receipt)));await assert.rejects(api.listEventWebhooks("token"));
});
test("event webhook UI starts without automatic requests, sender controls or replay action",async()=>{
 const require=createRequire(import.meta.url);
 const source=compile(readFileSync(new URL("../src/components/EventWebhookReceipts.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/eventWebhooks"',JSON.stringify(apiURL));
 const {EventWebhookReceipts}=await import(url(source));
 const html=renderToStaticMarkup(createElement(EventWebhookReceipts,{token:"token",onSessionExpired(){}}));
 for(const phrase of ["Webhook 事件外发回执","只读核对外发状态","按原外发编号只读查回","响应不明、重启和刷新均不会重发"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!html.includes("checked="));assert.ok(!html.includes("仅忽略此原回执"));assert.ok(!html.includes("重新发送"));
});
