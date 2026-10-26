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
const apiURL=url(compile(readFileSync(new URL("../src/api/subscriptionEditions.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const progress={previousOrder:98,order:99,topOrder:99,groupId:"777",cycleId:"a".repeat(64),target:[1,2,3],remaining:[2,3],cycleComplete:false};
const scope={type:"TV",id:"9223372036854775806",name:"Show",scopeValid:true,upgrade:progress,revision:"b".repeat(64)};
const receipt={id:progress.cycleId,type:"TV",subscriptionId:scope.id,name:"Show",year:"2026",state:"active",sourceMatches:true,progress,attempts:[{title:"Show.S01E01.1080p",order:99,episodes:[1],state:"accepted"}]};
const response=data=>new Response(JSON.stringify({code:0,success:true,data}));
test("edition source inspection is a local GET with full integer IDs and no client-provided paths",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/subscriptions/TV/"+scope.id+"/upgrade");assert.equal(options.method,undefined);return response(scope)});
  assert.equal((await api.getEditionScope("token","TV",scope.id)).upgrade.remaining.length,2);
  for(const id of ["1e3","0","../1","9223372036854775808"])await assert.rejects(api.getEditionScope("token","TV",id));assert.equal(calls,1);
});
test("edition receipt lookup and recent list never execute or change downloaders",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(options.method,undefined);if(path.endsWith(receipt.id))return response(receipt);assert.equal(path,"/api/v1/subscription-editions");return response([receipt])});
  assert.equal((await api.getEditionReceipt("token",receipt.id)).attempts[0].state,"accepted");assert.equal((await api.getEditionReceipts("token")).length,1);assert.equal(calls,2);
  await assert.rejects(api.getEditionReceipt("token","short"));assert.equal(calls,2);
});
test("edition closure requires a whole-cycle revision, explicit consent and a reason",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/subscriptions/TV/"+scope.id+"/upgrade/reset");assert.deepEqual(JSON.parse(options.body),{revision:scope.revision,reason:"new full cycle",confirm:true});return response({cycleId:receipt.id,state:"abandoned",executionStarted:false})});
  await assert.rejects(api.abandonEditionCycle("token",scope,"new full cycle",false));await assert.rejects(api.abandonEditionCycle("token",scope," ",true));await assert.rejects(api.abandonEditionCycle("token",{...scope,upgrade:{...progress,cycleId:""}},"reason",true));assert.equal(calls,0);
  await api.abandonEditionCycle("token",scope,"new full cycle",true);assert.equal(calls,1);
});
test("wrong edition closure receipts and lost replies never trigger another mutation",async t=>{
  for(const data of [{cycleId:"c".repeat(64),state:"abandoned",executionStarted:false},{cycleId:receipt.id,state:"completed",executionStarted:false},{cycleId:receipt.id,state:"abandoned",executionStarted:true}]){
    let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return response(data)});await assert.rejects(api.abandonEditionCycle("token",scope,"reason",true));assert.equal(calls,1);
  }
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;throw new Error("lost response")});await assert.rejects(api.abandonEditionCycle("token",scope,"reason",true));assert.equal(calls,1);
});
test("edition public validation rejects invented coverage, mixed priority and impossible completions",()=>{
  assert.equal(api.validEditionReceipt(receipt),true);
  for(const bad of [
    {...receipt,progress:{...progress,remaining:[4]}},
    {...receipt,state:"completed"},
    {...receipt,attempts:[{...receipt.attempts[0],order:98}]},
    {...receipt,attempts:[{...receipt.attempts[0],episodes:[2,1]}]},
    {...receipt,progress:{...progress,groupId:"9223372036854775808"}},
    {...receipt,progress:{...progress,order:101}},
  ])assert.equal(api.validEditionReceipt(bad),false);
  assert.equal(api.validEditionScope({...scope,upgrade:{...progress,cycleId:"",order:0,topOrder:0,groupId:"0",target:[],remaining:[]}}),true);
});
test("edition session expiry and incomplete JSON never retry closure",async t=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response(JSON.stringify({code:401,message:"expired"}),{status:401})});
  await assert.rejects(api.abandonEditionCycle("token",scope,"reason",true),e=>e.code===401);assert.equal(calls,1);
  t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("incomplete")});await assert.rejects(api.abandonEditionCycle("token",scope,"reason",true));assert.equal(calls,2);
});
test("edition UI explains quality cycles and file-selection gaps without implying media completion",async()=>{
  const require=createRequire(import.meta.url),raw=readFileSync(new URL("../src/components/SubscriptionEditionManager.tsx",import.meta.url),"utf8");
  const source=compile(raw).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/subscriptionEditions"',JSON.stringify(apiURL));
  const {SubscriptionEditionManager}=await import(url(source));
  const html=renderToStaticMarkup(createElement(SubscriptionEditionManager,{token:"token",selected:null,lastCycleId:"",onClose(){},onChanged(){},onSessionExpired(){}}));
  for(const phrase of ["不把已有库存当作完成","不混用不同质量","跨季包","文件选择模块","接收不等于下载","按轮次编号只读查回","不自动重发"])assert.ok(html.includes(phrase),phrase);
  assert.ok(!raw.includes("useState(true)"));const action=raw.match(/async function abandon\(\)\{([^\n]+)\}/)?.[1];assert.ok(action.indexOf("setLookup(scope.upgrade.cycleId)")<action.indexOf("await abandonEditionCycle"));
  assert.ok(!raw.match(/function clear\(\)\{([^\n]+)\}/)?.[1].includes("setLookup"));
});
test("edition page keeps ordinary progress separate and surfaces accepted priority receipts",()=>{
  const source=readFileSync(new URL("../src/pages/SubscriptionsPage.tsx",import.meta.url),"utf8"),editor=readFileSync(new URL("../src/components/SubscriptionEditor.tsx",import.meta.url),"utf8");
  assert.ok(source.includes("原普通记录（非本轮洗版完成）"));assert.ok(source.includes("洗版轮次与回执"));assert.ok(source.includes("result.upgrade?.cycleComplete"));assert.ok(editor.includes("不会释放未知提交"));
  assert.ok(source.includes("setEditionSelected(result.completed?null:"));assert.ok(source.includes("最高优先级 ${result.upgrade.order} 的目标已全部获得接收回执"));
});
