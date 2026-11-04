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
const apiURL=url(compile(readFileSync(new URL("../src/api/siteSignin.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL),id="a".repeat(64),jobID="b".repeat(64),nonce="c".repeat(64);
const settings={enabled:false,cron:"23.5",sign_sites:["9007199254740993"],special_sites:[],retry_keyword:"",queue_cnt:3,notify:false,onlyonce:false,clean:false,auto_cf:0,missed_detection:false,missed_schedule:""};
const job={id:jobID,batch:id,siteId:"9007199254740993",name:"Fixture",host:"fixture.test",actor:"admin",state:"waiting_input",day:"2026-11-02",created:"2026-11-02T01:00:00Z",updated:"2026-11-02T01:00:01Z",outcome:{state:"",reason:"",authenticated:false,retryable:false},requests:1,actions:0,canStop:true,runtimeActive:true,challenge:{kind:"captcha",nonce,deadline:"2026-11-02T01:01:00Z",question:"fixture",choices:[],multiple:false,image:"data:image/png;base64,AAAA"}};
const batch={id,items:[job],summary:"",notified:false};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("sign-in original batch lookup is read-only and retains exact large IDs",async t=>{let calls=0;t.mock.method(globalThis,"fetch",async(path,o)=>{calls++;assert.equal(path,"/api/v1/site-signin/batches/"+id);assert.equal(o.method,"GET");assert.equal(o.headers.Authorization,"token");assert.equal(o.body,undefined);return response(batch)});assert.equal((await api.getSigninBatch("token",id)).items[0].siteId,"9007199254740993");for(const bad of ["../x","A".repeat(64),"short"])await assert.rejects(api.getSigninBatch("token",bad));assert.equal(calls,1);t.mock.method(globalThis,"fetch",async()=>response({...batch,id:"d".repeat(64),items:[]}));await assert.rejects(api.getSigninBatch("token",id));});
test("unknown start and challenge responses are never retried by the client",async t=>{let calls=0;t.mock.method(globalThis,"fetch",async(_path,o)=>{calls++;assert.equal(o.method,"POST");throw Error("lost acknowledgement")});await assert.rejects(api.startSignin("token",{id,fingerprint:nonce}));assert.equal(calls,1);await assert.rejects(api.inputSignin("token",jobID,nonce,["fixture-input"]));assert.equal(calls,2)});
test("sign-in input accepts only original data images and string site IDs",()=>{assert.equal(api.signinJobValid(job),true);for(const bad of [{...job,siteId:9007199254740992},{...job,requests:-1},{...job,challenge:{...job.challenge,image:"https://foreign.test/image.png"}},{...job,challenge:{...job.challenge,image:"data:image/svg+xml;base64,AAAA"}},{...job,state:"finished",outcome:{...job.outcome,state:"success"}}])assert.equal(api.signinJobValid(bad),false)});
test("sign-in review preserves original helper HTTP receivers and explicit options",async t=>{const review={id,fingerprint:nonce,mode:"manual",expires:"2026-11-02T01:10:00Z",location:"Asia/Shanghai",settings,targets:[{id:"9007199254740993",name:"Fixture",scheme:"https",host:"fixture.test",path:"/",adapter:"generic",browser:false}],helpers:[{kind:"original OCR",scheme:"http",host:"127.0.0.1:9988",path:"/captcha",data:"fixture image"}],remote:[]};const options={ocr:true,douban:false,lens:false,ai:false,savedLogin:false,remote:false};t.mock.method(globalThis,"fetch",async(_path,o)=>{assert.deepEqual(JSON.parse(o.body),{mode:"manual",siteIds:["9007199254740993"],options});return response(review)});assert.equal((await api.reviewSignin("token","manual",["9007199254740993"],options)).helpers[0].scheme,"http");t.mock.method(globalThis,"fetch",async()=>response({...review,helpers:[{...review.helpers[0],scheme:"ftp"}]}));await assert.rejects(api.reviewSignin("token","manual",["9007199254740993"],options));});
test("sign-in settings validation preserves explicit empty site selection",()=>{assert.equal(api.signinSettingsValid({...settings,sign_sites:[]}),true);assert.equal(api.signinSettingsValid({...settings,sign_sites:[9007199254740992]}),false);assert.equal(api.signinSettingsValid({...settings,queue_cnt:0}),false);});
const require=createRequire(import.meta.url);
const component=compile(readFileSync(new URL("../src/components/SiteSigninManager.tsx",import.meta.url),"utf8"))
  .replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href))
  .replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href))
  .replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/siteSignin"',JSON.stringify(apiURL))
  .replace('import "./SiteSigninManager.css";',"");
const {SiteSigninManager,SigninChallengeForm}=await import(url(component));
test("sign-in page begins with receipt lookup and no execution controls",()=>{
  const html=renderToStaticMarkup(createElement(SiteSigninManager,{session:{token:"token",user:{username:"admin"}},onSessionExpired(){}}));
  assert.ok(html.includes("站点签到管理"));assert.ok(html.includes("只读查询原批次"));
  assert.ok(!html.includes("确认执行本次签到"));assert.ok(!html.includes("确认启用这份设置"));
});
test("sign-in challenge constrains original values and deadline",()=>{
  const now=Date.parse("2026-11-02T01:00:01Z");
  const fixed={...job.challenge,minLength:6,maxLength:6};
  assert.match(api.signinInputError(fixed,["ABC"],now),/6 个字符/);
  assert.equal(api.signinInputError(fixed,["😀乙CD12"],now),"");
  assert.match(api.signinInputError(fixed,["ABC123"],Date.parse(fixed.deadline)),/已到期/);
  assert.equal(api.signinInputError(job.challenge,["ABCD"],now),"");
  assert.ok(api.signinInputError(job.challenge,["😀".repeat(33)],now));
  const email={...job.challenge,kind:"email"};
  assert.ok(api.signinInputError(email,["bad-address"],now));
  assert.equal(api.signinInputError(email,["fixture@example.test"],now),"");
  const choices=[{value:"2",text:"A"},{value:"4",text:"B"}];
  const choice={...job.challenge,kind:"choice",choices,multiple:true};
  assert.equal(api.signinInputError(choice,["4","2"],now),"");
  for(const input of [[],["2","2"],["unlisted"]])assert.ok(api.signinInputError(choice,input,now));
  assert.ok(api.signinInputError({...choice,multiple:false},["2","4"],now));
});
test("only a definite input response allows the same prompt to be filled again",async t=>{
  let result=response({received:true});
  let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;
    assert.equal(path,"/api/v1/site-signin/jobs/"+jobID+"/input");
    assert.equal(options.method,"POST");
    assert.ok(options.signal instanceof AbortSignal);
    assert.deepEqual(JSON.parse(options.body),{nonce,values:["ABC123"],confirm:true});
    return result;
  });
  assert.deepEqual(await api.inputSignin("token",jobID,nonce,["ABC123"]),{received:true});
  result=response({});
  await assert.rejects(api.inputSignin("token",jobID,nonce,["ABC123"]),error=>!api.signinInputRejected(error));
  result=new Response("incomplete", {status:409});
  await assert.rejects(api.inputSignin("token",jobID,nonce,["ABC123"]),error=>!api.signinInputRejected(error));
  result=new Response(JSON.stringify({code:409,success:false,message:"prompt rejected"}),{status:409});
  await assert.rejects(api.inputSignin("token",jobID,nonce,["ABC123"]),api.signinInputRejected);
  await assert.rejects(api.inputSignin("token",jobID,"invalid",["ABC123"]));
  assert.equal(calls,4);
});
test("image challenge starts with a labelled blank field and a disabled submit",()=>{
  const challenge={...job.challenge,deadline:new Date(Date.now()+60_000).toISOString(),minLength:6,maxLength:6};
  const html=renderToStaticMarkup(createElement(SigninChallengeForm,{
    job:{...job,challenge},busy:false,onInput:()=>assert.fail("unexpected input"),onRead:()=>assert.fail("unexpected read"),
  }));
  assert.match(html,/原站点本次验证码/);
  assert.match(html,/本次图片验证码<input[^>]*type="password"[^>]*value=""/);
  assert.match(html,/<button type="submit" disabled="">发送本次输入/);
  assert.ok(!html.includes("本次输入已接收"));
});
