import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import test from "node:test";
import ts from "typescript";
const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const client=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=url(compile(readFileSync(new URL("../src/api/siteStatistics.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(client)));
const api=await import(apiURL);
const job={id:"a".repeat(64),origin:"fixture",actor:"fixture-admin",state:"unknown",created:"fixture",updated:"fixture",requests:2,messageRequests:0,readMessages:false,results:[]};
const status={fingerprint:"b".repeat(64),goOnly:true,cron:"",current:false,control:{enabled:false,epoch:"",actor:"",fingerprint:"",cron:"",location:"",readMessages:false,remote:false,nextRun:"",created:""},targets:[],items:[],jobs:[job]};
const response=data=>({ok:true,status:200,json:async()=>({code:0,success:true,data})});
test("statistics status keeps interrupted jobs and rejects invented states",()=>{assert.equal(api.statsStatusValid(status),true);assert.equal(api.statsStatusValid({...status,jobs:[{...job,state:"invented_success"}]}),false);assert.equal(api.statsStatusValid({...status,control:{...status.control,readMessages:"yes"}}),false);});
test("statistics byte counters and seed sizes preserve decimal precision",()=>{const detail={siteId:"42",history:[{date:"2026-11-03",upload:"9007199254740993",download:"1",seedingSize:"9007199254740993",bonus:0,seeding:1}],seedingInfo:[["3","9007199254740993"]]};assert.equal(api.statsDetailValid(detail),true);assert.equal(api.statsDetailValid({...detail,seedingInfo:[[3,9007199254740993]]}),false);assert.equal(api.statsDetailValid({...detail,history:[{...detail.history[0],upload:"9223372036854775808"}]}),false);});
test("statistics detail cannot query an unrelated site or accept mismatched receipt",async t=>{let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/site-statistics/detail/42");assert.equal(options.method,"GET");return response({siteId:"43",history:[],seedingInfo:[]});});assert.throws(()=>api.getStatsDetail("fixture","../other"));await assert.rejects(()=>api.getStatsDetail("fixture","42"));assert.equal(calls,1);});
test("statistics refresh sends explicit message scope and preserves original request ID without retry",async t=>{let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/site-statistics/refresh");const body=JSON.parse(options.body);assert.equal(body.requestId,"c".repeat(64));assert.equal(body.readMessages,false);assert.deepEqual(body.siteIds,["42"]);return {ok:true,status:200,json:async()=>{throw new Error("lost fixture reply")}};});await assert.rejects(()=>api.refreshStatistics("fixture",status.fingerprint,"c".repeat(64),["42"],false));assert.equal(calls,1);});

const ranking={
  configured:true,site:"Fixture",current_level:"User",next_level:"Power User",overall_progress:50,
  requirements:[{key:"download_gb",label:"Download",unit:"GiB",current:1,required:2,progress:50,met:false,need:1}],
  level_steps:[{name:"User",state:"current",order:1,is_keep:true},{name:"Power User",state:"target",order:2,is_keep:false}],
  keep_account_level:"User",current_is_keep:true,rule_url:"https://rules.fixture.test/wiki",is_special_level:false,
  special_level_description:"",status_type:"normal",message:"",
};
const rankReceipt={siteId:"9007199254740993",ranking};
test("ranking accepts only the original exact site ID and a read-only request",async t=>{
  const controller=new AbortController();
  let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;
    assert.equal(path,"/api/v1/site-statistics/ranking/"+rankReceipt.siteId);
    assert.equal(options.method,"GET");
    assert.equal(options.body,undefined);
    assert.equal(options.signal,controller.signal);
    return response(calls===1?rankReceipt:{...rankReceipt,siteId:"9007199254740992"});
  });
  assert.deepEqual(await api.getStatsRanking("fixture",rankReceipt.siteId,controller.signal),rankReceipt);
  await assert.rejects(api.getStatsRanking("fixture",rankReceipt.siteId,controller.signal));
  assert.throws(()=>api.getStatsRanking("fixture","../site"));
  assert.throws(()=>api.getStatsRanking("fixture","9223372036854775808"));
  assert.equal(calls,2);
});

test("old statistics page can load the independent viewer overview",async t=>{
  const overview={targets:[],items:[]};
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    assert.equal(path,"/api/v1/site-statistics/overview");
    assert.equal(options.method,"GET");
    assert.equal(options.body,undefined);
    return response(overview);
  });
  assert.deepEqual(await api.getStatsOverview("viewer"),overview);
  assert.equal(api.statsStatusValid(overview),false);
});

test("ranking rejects invalid links, percentages and level steps",()=>{
  assert.equal(api.statsRankingValid(rankReceipt),true);
  for(const invalid of [
    {rule_url:"javascript:alert(1)"},{rule_url:"//rules.fixture.test"},{rule_url:"https://user:pw@rules.fixture.test/"},
    {overall_progress:101},{overall_progress:Infinity},{level_steps:[{...ranking.level_steps[0],state:"unknown"}]},
  ]){
    assert.equal(api.statsRankingValid({...rankReceipt,ranking:{...ranking,...invalid}}),false);
  }
});

test("ranking panel renders data as text and distinguishes missing and special levels",async()=>{
  const require=createRequire(import.meta.url);
  const renderSource=compile(readFileSync(new URL("../src/components/SiteRankingDetail.tsx",import.meta.url),"utf8"))
    .replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href))
    .replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href))
    .replace('"../api/client"',JSON.stringify(client))
    .replace('"../api/siteStatistics"',JSON.stringify(apiURL));
  const {SiteRankingDetail}=await import(url(renderSource));
  const html=renderToStaticMarkup(createElement(SiteRankingDetail,{ranking:{...ranking,current_level:"<img src=bad>"}}));
  assert.match(html,/&lt;img src=bad&gt;/);
  assert.match(html,/晋升条件/);
  assert.match(html,/保号等级 User/);
  assert.match(html,/rel="noreferrer"/);
  for(const [state,message] of [["special_level","特殊等级不参与晋升"],["not_configured","站点未配置等级数据"]]){
    const view=renderToStaticMarkup(createElement(SiteRankingDetail,{ranking:{...ranking,status_type:state,message,requirements:[],level_steps:[],next_level:null}}));
    assert.ok(view.includes(message));
    assert.ok(!view.includes("总体进度"));
  }
});
