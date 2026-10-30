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
const dataURL=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const clientURL=dataURL(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const apiURL=dataURL(compile(readFileSync(new URL("../src/api/openSubtitles.ts",import.meta.url),"utf8")).replace('"./client"',JSON.stringify(clientURL)));
const api=await import(apiURL);
const pending=()=>({id:"a".repeat(64),reference:"b".repeat(32),name:"Movie",provider:"org",origin:"https://www.opensubtitles.org",state:"pending",created:new Date().toISOString(),updated:new Date().toISOString(),reason:"",libraryId:"",requests:0,outputs:[]});
const response=data=>new Response(JSON.stringify({code:0,success:true,data}),{status:200,headers:{"Content-Type":"application/json"}});
function mock(t,fn){const before=globalThis.fetch,calls=[];t.after(()=>{globalThis.fetch=before});globalThis.fetch=async(path,options)=>{calls.push({path,options});return fn(path,options,calls.length)};return calls}
test("OpenSubtitles receipts require bounded original identities, fixed provider and actual file results",()=>{
 const p=pending(),libraryId="c".repeat(64),output={id:"d".repeat(64),jobId:libraryId,target:"Movie/Movie.chi.zh-cn.srt",state:"created",reason:"original output"};
 assert.equal(api.validOpenSubtitleReceipt(p),true);
 assert.equal(api.validOpenSubtitleReceipt({...p,state:"completed",libraryId,requests:2,outputs:[output]}),true);
 for(const bad of [{...p,id:"short"},{...p,origin:"https://foreign.test"},{...p,origin:"https://user:key@www.opensubtitles.org"},{...p,requests:257},{...p,state:"completed",libraryId,requests:2},{...p,state:"uncertain"},{...p,state:"pending",libraryId},{...p,state:"stopped",requests:1},{...p,state:"partial",libraryId},{...p,state:"completed",libraryId,requests:2,outputs:[{...output,target:"../outside.srt"}]},{...p,state:"completed",libraryId,requests:2,outputs:[{...output,state:"writing"}]}]){
  assert.equal(api.validOpenSubtitleReceipt(bad),false,JSON.stringify(bad));
 }
});
test("original OpenSubtitles list and lookup are readonly and reject substituted or duplicate records",async t=>{
 const p=pending(),calls=mock(t,path=>response(path.includes("?")?[p]:p));
 assert.equal((await api.getOpenSubtitle("token",p.id)).id,p.id);assert.equal((await api.listOpenSubtitles("token",2)).length,1);
 assert.equal(calls.every(c=>c.options.method==="GET"),true);assert.equal(calls[1].path,"/api/v1/open-subtitles/requests?page=2");
 await assert.rejects(api.getOpenSubtitle("token","short"));await assert.rejects(api.listOpenSubtitles("token",0));assert.equal(calls.length,2);
 globalThis.fetch=async()=>response({...p,id:"e".repeat(64)});await assert.rejects(api.getOpenSubtitle("token",p.id));
 globalThis.fetch=async()=>response([p,p]);await assert.rejects(api.listOpenSubtitles("token",1));
});
test("explicit stop never cancels an already-started download, replays lost acknowledgement or accepts a different original",async t=>{
 const p=pending(),calls=mock(t,()=>response({...p,state:"stopped",reason:"stop original"}));
 await assert.rejects(api.stopOpenSubtitle("token",p,false,"stop original"));await assert.rejects(api.stopOpenSubtitle("token",p,true," "));
 for(const state of ["running","uncertain","completed","not_found","blocked"]){await assert.rejects(api.stopOpenSubtitle("token",{...p,state,libraryId:"c".repeat(64),requests:1},true,"stop original"))}
 assert.equal(calls.length,0);assert.equal((await api.stopOpenSubtitle("token",p,true,"stop original")).state,"stopped");assert.equal(calls.length,1);
 assert.deepEqual(JSON.parse(calls[0].options.body),{confirm:true,reason:"stop original"});
 globalThis.fetch=async()=>{calls.push("lost");throw Error("lost acknowledgement")};
 await assert.rejects(api.stopOpenSubtitle("token",p,true,"stop original"),/lost acknowledgement/);assert.equal(calls.length,2);
 globalThis.fetch=async()=>response({...p,state:"stopped",reference:"e".repeat(32)});await assert.rejects(api.stopOpenSubtitle("token",p,true,"stop original"),/同一原请求/);
});
test("OpenSubtitles readiness requires the original fixed origin and blanks unavailable scope",async t=>{
 const calls=mock(t,()=>response({wanted:true,ready:true,provider:"org",origin:"https://www.opensubtitles.org",remote:false}));
 assert.equal((await api.getOpenSubtitleStatus("token")).ready,true);assert.equal(calls.length,1);
 for(const s of [{wanted:true,ready:true,provider:"org",origin:"https://foreign.test",remote:false},{wanted:true,ready:false,provider:"org",origin:"https://www.opensubtitles.org",remote:false},{wanted:false,ready:true,provider:"org",origin:"https://www.opensubtitles.org",remote:false}]){
  globalThis.fetch=async()=>response(s);await assert.rejects(api.getOpenSubtitleStatus("token"));
 }
});
test("receipt manager starts idle without hidden network or consent and describes uncertain file facts",async t=>{
 const calls=mock(t,()=>{throw Error("no implicit network")});
 const source=compile(readFileSync(new URL("../src/components/OpenSubtitleReceipts.tsx",import.meta.url),"utf8"))
 .replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href))
 .replace('"../api/client"',JSON.stringify(clientURL)).replace('"../api/openSubtitles"',JSON.stringify(apiURL));
 const {OpenSubtitleReceipts,OpenSubtitleFact}=await import(dataURL(source));
 const html=renderToStaticMarkup(createElement(OpenSubtitleReceipts,{token:"fixture",onSessionExpired(){}}));
 assert.match(html,/OpenSubtitles 原请求与文件回执/);assert.match(html,/按原 OpenSubtitles 编号只读查回/);assert.doesNotMatch(html,/\schecked(?:=""|>|\s)/);assert.equal(calls.length,0);
 const fact=renderToStaticMarkup(createElement(OpenSubtitleFact,{receipt:{...pending(),state:"uncertain",libraryId:"c".repeat(64),requests:3}}));
 assert.match(fact,/结果不明，不会重发/);assert.match(fact,/不代表新增字幕数/);
});
