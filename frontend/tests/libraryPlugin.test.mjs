import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import test from "node:test";
import ts from "typescript";
const source=readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8");
const {outputText}=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022}});
const {getPluginConfig,savePluginConfig,getPluginPage}=await import(`data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`);
const fingerprint="f".repeat(64);
test("library plugin config reads native schema, scope choices and fresh authorization fingerprint only",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/plugins/LibraryScraper");assert.equal(options.method,undefined);assert.equal(options.headers.Authorization,"token");return new Response(JSON.stringify({code:0,success:true,data:{id:"LibraryScraper",name:"媒体库刮削",fields:[{key:"scraper_path",type:"multiselect",options:[{value:"root",label:"电影库"}]}],values:{scraper_path:["root"],go_confirm:false,go_old_stopped:false,go_overwrite:false,onlyonce:false},meta:{hasPage:true,fingerprint}}}));});
  const d=await getPluginConfig("token","LibraryScraper");assert.equal(d.meta.fingerprint,fingerprint);assert.equal(d.values.onlyonce,false);assert.equal(d.values.go_confirm,false);assert.equal(calls,1);
});
test("library plugin refuses an unreviewed save before sending any request",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async()=>{calls++;return new Response("{}");});await assert.rejects(savePluginConfig("token","LibraryScraper",{values:{onlyonce:true},clearConfig:[]}));assert.equal(calls,0);
});
test("library one-time and forced recurring authority remain explicit with root IDs",async(t)=>{
  const input={fingerprint,values:{mode:"force_all",scraper_path:["root"],cron:"0 0 * * *",onlyonce:true,go_confirm:true,go_old_stopped:true,go_overwrite:true},clearConfig:[]};let calls=0;
  t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/plugins/LibraryScraper");assert.equal(options.method,"PUT");assert.deepEqual(JSON.parse(options.body),input);return new Response(JSON.stringify({code:0,success:true}));});await savePluginConfig("token","LibraryScraper",input);assert.equal(calls,1);
});
test("lost scheduler response never retries onlyonce, installation or file operations",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path)=>{calls++;assert.equal(path,"/api/v1/plugins/LibraryScraper");throw new Error("lost response");});await assert.rejects(savePluginConfig("token","LibraryScraper",{fingerprint,values:{onlyonce:true,go_confirm:true,go_old_stopped:true},clearConfig:[]}));assert.equal(calls,1);
});
test("library stop is distinct from clearing saved schedule or requesting another execution",async(t)=>{
  let calls=0;const input={fingerprint,values:{go_stop:true,go_confirm:false,onlyonce:false},clearConfig:[]};t.mock.method(globalThis,"fetch",async(_path,options)=>{calls++;assert.deepEqual(JSON.parse(options.body),input);return new Response(JSON.stringify({code:0,success:true}));});await savePluginConfig("token","LibraryScraper",input);assert.equal(calls,1);
});
test("library plugin batch page is readonly and never resumes a paused job",async(t)=>{
  let calls=0;t.mock.method(globalThis,"fetch",async(path,options)=>{calls++;assert.equal(path,"/api/v1/plugins/LibraryScraper/page");assert.equal(options.method,undefined);return new Response(JSON.stringify({code:0,success:true,data:{title:"媒体库刮削",readOnly:true,sections:["paused"],tables:[]}}));});await getPluginPage("token","LibraryScraper");assert.equal(calls,1);
});
