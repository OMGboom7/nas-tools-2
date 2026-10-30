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
const apiURL=url(compile(readFileSync(new URL("../src/api/siteLogin.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL),id="a".repeat(64),jobID="b".repeat(64),nonce="c".repeat(64);
const job={id:jobID,batch:id,siteId:"9007199254740993",name:"Fixture",host:"https://site.test",actor:"root",state:"waiting_input",reason:"",loginState:"",created:"2026-10-30T12:00:00Z",updated:"2026-10-30T12:00:00Z",requests:1,actions:0,canStop:true,runtimeActive:true,challenge:{kind:"two_step",nonce,deadline:"2026-10-30T12:01:00Z"}};
const batch={id,items:[job],rememberState:"not_requested",rememberReason:""};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("site login lookup is read-only, original-batch-bound and retains exact large site IDs",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.equal(o.method,"GET");assert.equal(o.headers.Authorization,"token");assert.ok(!path.includes("password"));return response(path.includes("?page=")?{page:1,items:[job]}:batch)});
 assert.equal((await api.getSiteLoginBatch("token",id)).items[0].siteId,"9007199254740993");await api.getSiteLoginHistory("token",1);
 for(const bad of ["../x","short","A".repeat(64)])await assert.rejects(api.getSiteLoginBatch("token",bad));
 assert.equal(calls,2);
 t.mock.method(globalThis,"fetch",async()=>response({...batch,id:"d".repeat(64),items:[]}));await assert.rejects(api.getSiteLoginBatch("token",id));
});
test("site login start and one-time input never automatically replay unknown responses",async t=>{
 let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.equal(o.method,"POST");throw Error("lost response")});
 await assert.rejects(api.startSiteLogin("token",{scopeToken:"opaque",username:"fixture",password:"private",twoStepCode:"123456",confirm:true}));
 assert.equal(calls,1);await assert.rejects(api.submitSiteLoginInput("token",jobID,nonce,"654321"));assert.equal(calls,2);
});
test("site login job rejects foreign image URLs, wrong identity and impossible receipt counters",()=>{
 assert.equal(api.siteLoginJobValid(job),true);
 for(const bad of [{...job,siteId:9007199254740992},{...job,state:"success"},{...job,requests:-1},{...job,challenge:{...job.challenge,image:"https://other.test/captcha.png"}},{...job,challenge:{...job.challenge,image:"data:image/svg+xml;base64,AAAA"}}])assert.equal(api.siteLoginJobValid(bad),false);
});
test("site login credentials remain in the explicit POST body and are never part of read URLs",async t=>{
 t.mock.method(globalThis,"fetch",async(path,o)=>{assert.equal(path,"/api/v1/site-login/requests");assert.equal(o.headers.Authorization,"token");assert.deepEqual(JSON.parse(o.body),{scopeToken:"opaque-plan",username:"fixture",password:"only-this-post",twoStepCode:"one-time",confirm:true});return response(batch)});
 await api.startSiteLogin("token",{scopeToken:"opaque-plan",username:"fixture",password:"only-this-post",twoStepCode:"one-time",confirm:true});
});
test("site login status and review reject malformed successful envelopes",async t=>{
 for(const bad of [{},null,{browserConfigured:true,goOnly:true,savedConfigured:true,maxSites:0}]){
  t.mock.method(globalThis,"fetch",async()=>response(bad));await assert.rejects(api.getSiteLoginStatus("token"));
 }
 t.mock.method(globalThis,"fetch",async()=>new Response("bad JSON"));await assert.rejects(api.reviewSiteLogin("token",{siteIds:["1"],useSaved:false,remember:false,ocr:false}));
});
test("site login review retains the original HTTP OCR receiver and rejects unknown schemes",async t=>{
 const review={id,scopeToken:"opaque-encrypted-original-scope-token",expires:"2026-10-30T12:10:00Z",targets:[{id:"1",name:"Fixture",host:"site.test",path:"/login.php",scheme:"https"}],ocrReceivers:[{kind:"原服务端 OCR",scheme:"http",host:"ocr-fixture.internal:9898",path:"/captcha/base64"}]};
 t.mock.method(globalThis,"fetch",async()=>response(review));
 const out=await api.reviewSiteLogin("token",{siteIds:["1"],useSaved:false,remember:false,ocr:true});assert.equal(out.ocrReceivers[0].scheme,"http");
 t.mock.method(globalThis,"fetch",async()=>response({...review,ocrReceivers:[{...review.ocrReceivers[0],scheme:"ftp"}]}));
 await assert.rejects(api.reviewSiteLogin("token",{siteIds:["1"],useSaved:false,remember:false,ocr:true}));
});
test("site login panel starts with no transmission or saved-credential consent and exposes original receipt lookup",async()=>{
 const require=createRequire(import.meta.url);
 const source=compile(readFileSync(new URL("../src/components/SiteLoginPanel.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/siteLogin"',JSON.stringify(apiURL)).replace('import "./SiteLoginPanel.css";',"");
 const {SiteLoginPanel}=await import(url(source));
 const html=renderToStaticMarkup(createElement(SiteLoginPanel,{session:{token:"token",userinfo:{username:"root",userid:"0",userpris:[]}},sites:[],onClose(){},onSaved(){},onSessionExpired(){}}));
 for(const phrase of ["登录并更新站点会话","原站点登录回执","只读刷新登录回执","关闭面板不会撤回"])assert.ok(html.includes(phrase),phrase);
 assert.ok(!html.includes("登录并更新所选站点 Cookie"));assert.ok(!html.includes("value=\"password\""));
});
