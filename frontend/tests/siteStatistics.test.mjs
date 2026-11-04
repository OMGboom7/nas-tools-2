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

test("traffic reads retain exact byte counts and validate the returned date and range",async t=>{
  const original={
    days:7,endDay:"2026-11-03",today:"2026-11-04",items:[
      {siteId:"9007199254740993",name:"Fixture",host:"one.fixture.test",upload:"9007199254740993",download:"4"},
      {siteId:"9007199254740995",name:"Fixture",host:"two.fixture.test",upload:"9007199254740992",download:"10"},
    ],
  };
  let data=original,calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{
    calls++;
    assert.equal(path,"/api/v1/site-statistics/traffic?days=7&endDay=2026-11-03");
    assert.equal(options.method,"GET");
    assert.equal(options.body,undefined);
    return response(data);
  });
  assert.deepEqual(await api.getStatsTraffic("viewer",7,"2026-11-03"),original);
  data={...original,endDay:"2026-11-02"};
  await assert.rejects(api.getStatsTraffic("viewer",7,"2026-11-03"));
  assert.equal(api.statsTrafficValid({...original,items:[original.items[0],original.items[0]]}),false);
  assert.equal(api.statsTrafficValid({...original,items:[{...original.items[0],upload:9007199254740992}]}),false);
  assert.throws(()=>api.getStatsTraffic("viewer",7,"2026-02-30"));
  assert.throws(()=>api.getStatsTraffic("viewer",2,"2026-11-03"));
  assert.equal(calls,2);
});

const viewURL=url(compile(readFileSync(new URL("../src/utils/statisticsView.ts",import.meta.url),"utf8")));
const view=await import(viewURL);
const png=await import(url(compile(readFileSync(new URL("../src/utils/statisticsPng.ts",import.meta.url),"utf8"))
  .replace('"./statisticsView"',JSON.stringify(viewURL))));
const makeItem=(url,username,upload)=>({
  site:"Original name",url,username,upload,download:"2",seedingSize:"3",seeding:1,ratio:1,bonus:0,
});
const first=makeItem("https://first.fixture.test","first-fixture-user","9007199254740993");
const second=makeItem("https://second.fixture.test","second-fixture-user","9007199254740992");
const rows=view.statsRows({items:[second,first],targets:[
  {id:"9007199254740995",name:"Same name",host:"second.fixture.test",url:second.url},
  {id:"9007199254740993",name:"Same name",host:"first.fixture.test",url:first.url},
  {id:"42",name:"Original name"},
]});
test("statistics charts and share sorting distinguish same-name origins and sum exact counters",()=>{
  assert.equal(rows[0].item.username,"second-fixture-user");
  assert.equal(rows[1].item.username,"first-fixture-user");
  assert.equal(rows[2].item,undefined);
  assert.deepEqual(view.statsTotals(rows),{upload:18014398509481985n,download:4n,seeding:2n,seedingSize:6n});
  const selected=new Set(rows.map(row=>row.id));
  const opts={...view.shareDefaults,hideSite:false,hideUser:false};
  const descending=view.shareProjection(rows,selected,opts);
  assert.deepEqual(descending.rows.map(row=>row[1]),["first-fixture-user","second-fixture-user"]);
  const ascending=view.shareProjection(rows,selected,{...opts,direction:"asc"});
  assert.deepEqual(ascending.rows.map(row=>row[1]),["second-fixture-user","first-fixture-user"]);
  assert.equal(view.shareProjection(rows,new Set(),opts).rows.length,0);
});

test("masked share projects only selected public text into the PNG drawing surface",()=>{
  const report=view.shareProjection(rows,new Set([rows[1].id]),view.shareDefaults);
  const drawn=[];
  const canvas={
    getContext:()=>({
      fillRect(){},fillText(text){drawn.push(text);},measureText(value){return {width:value.length*6};},
    }),
  };
  png.drawStatisticsPng(canvas,report);
  assert.equal(canvas.width,1080);
  assert.equal(canvas.height,260);
  assert.equal(report.totals.upload,9007199254740993n);
  assert.ok(drawn.includes("站点 1"));
  assert.ok(drawn.includes("已隐藏"));
  assert.doesNotMatch(drawn.join(" "),/fixture|Same name|站点 2/);
  assert.throws(()=>png.drawStatisticsPng(canvas,{...report,rows:[]}));
});

test("PNG export includes a selected origin only once",()=>{
  const aliases=[rows[1],{...rows[1],id:"44",name:"Alternate name"}];
  const report=view.shareProjection(aliases,new Set(aliases.map(row=>row.id)),view.shareDefaults);
  assert.deepEqual(view.shareCandidates(aliases).map(row=>row.id),[rows[1].id]);
  assert.equal(report.rows.length,1);
  assert.equal(report.totals.upload,9007199254740993n);
});

test("scatter sampling keeps both extrema and the final observed point",()=>{
  const seeds=Array.from({length:1501},()=>["1","1"]);
  seeds[749]=["9223372036854775807","2"];
  seeds[1100]=["2","9007199254740993"];
  seeds[1500]=["4","5"];
  const sample=view.scatterSample(seeds);
  assert.equal(sample.points.length,1500);
  assert.equal(sample.maxCount,9223372036854775807n);
  assert.equal(sample.maxBytes,9007199254740993n);
  assert.ok(sample.points.includes(seeds[749]));
  assert.ok(sample.points.includes(seeds[1100]));
  assert.deepEqual(sample.points.at(-1),["4","5"]);
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
