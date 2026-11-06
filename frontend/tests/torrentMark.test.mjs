import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {createRequire} from "node:module";
import {pathToFileURL} from "node:url";
import test from "node:test";
import { webcrypto } from "node:crypto";
if (!globalThis.crypto) Object.defineProperty(globalThis,"crypto",{configurable:true,value:webcrypto});
import ts from "typescript";
import {createElement} from "react";
import {renderToStaticMarkup} from "react-dom/server";
const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>"data:text/javascript;base64,"+Buffer.from(s).toString("base64");
const require=createRequire(import.meta.url);
const client=url(compile(readFileSync(new URL("../src/api/client.ts",import.meta.url),"utf8")));
const source=compile(readFileSync(new URL("../src/components/TorrentMarkManager.tsx",import.meta.url),"utf8")).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve("react/jsx-runtime")).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve("react")).href)).replace('"../api/client"',JSON.stringify(client));
const {TorrentMarkManager,validTorrentMarkStatus}=await import(url(source));
const status={fingerprint:"a".repeat(64),goOnly:true,active:false,exclusiveRequired:true,settings:{enable:true,onlyonce:false,cron:""},control:{Epoch:"",Next:"",Once:""},targets:[{id:"9007199254740993",name:"fixture",type:"transmission",receiver:"http://fixture.test:9091"}],runs:[{id:"b".repeat(64),state:"unknown",kind:"manual",created:"fixture",changed:0,skipped:0,unknown:1}],pending:[{id:"c".repeat(64),hash:"d".repeat(40),downloader:"9007199254740993",goal:"PT"}]};
test("torrent mark status preserves exact downloader identity and uncertain outcomes",()=>{
 assert.equal(validTorrentMarkStatus(status),true);
 for(const changed of [{fingerprint:"bad"},{settings:{enable:"true"}},{control:null},{runs:[{...status.runs[0],state:"invented"}]},{runs:[{...status.runs[0],unknown:-1}]},{pending:[{...status.pending[0],goal:"delete"}]},{targets:[{...status.targets[0],id:"1.1"}]}])assert.equal(validTorrentMarkStatus({...status,...changed}),false);
});
test("torrent mark UI renders an idle scope review without contacting a downloader",t=>{
 let calls=0;t.mock.method(globalThis,"fetch",()=>{calls++;throw Error("not allowed")});
 const html=renderToStaticMarkup(createElement(TorrentMarkManager,{token:"fixture",onSessionExpired(){}}));assert.ok(html.includes("种子标记"));assert.ok(html.includes("未知结果"));assert.equal(calls,0);assert.equal(html.includes("checked"),false);
});

// Drive component events with isolated hook state and a fake network.
async function harness(t,fetcher){
 const state=[],refs=[],effects=[];let si=0,ri=0;
 const mockURL=url('export const useState=(...x)=>globalThis.__torrentHooks.state(...x);export const useRef=(...x)=>globalThis.__torrentHooks.ref(...x);export const useEffect=(...x)=>globalThis.__torrentHooks.effect(...x);');
 t.mock.method(globalThis,'fetch',fetcher);
 globalThis.__torrentHooks={state(initial){const i=si++;if(!(i in state))state[i]=initial;return[state[i],v=>{state[i]=typeof v==='function'?v(state[i]):v}]},ref(initial){const i=ri++;if(!(i in refs))refs[i]={current:initial};return refs[i]},effect(fn){if(!effects.length)effects.push(fn)}};
 const module=await import(url(compile(readFileSync(new URL('../src/components/TorrentMarkManager.tsx',import.meta.url),'utf8')).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve('react/jsx-runtime')).href)).replace('"react"',JSON.stringify(mockURL)).replace('"../api/client"',JSON.stringify(client))));
 const render=()=>{si=0;ri=0;return module.TorrentMarkManager({token:'fixture',onSessionExpired(){}})};
 const flatten=(n)=>{if(!n||typeof n!=='object')return[];if(Array.isArray(n))return n.flatMap(flatten);return[n,...flatten(n.props?.children)]};
 const find=(tree,type,phrase)=>flatten(tree).find(n=>n.type===type&&JSON.stringify(n.props.children??'').includes(phrase));
 render();const cleanup=effects[0]();await new Promise(r=>setImmediate(r));
 return {render,find,cleanup(){cleanup();delete globalThis.__torrentHooks}};
}
const reply=data=>({ok:true,status:200,json:async()=>({code:0,success:true,data})});
test('a running manual request can still stop subsequent work',async t=>{
 let finish,stopped=0;
 const h=await harness(t,async(path,opts)=>{
  if(path.endsWith('/run'))return new Promise(resolve=>{finish=()=>resolve(reply({...status.runs[0],state:'stopped'}))});
  if(path.endsWith('/stop')){stopped++;return reply({stopped:true})}
  return reply(status);
 });
 try{
  let tree=h.render();h.find(tree,'input','')?.props.onChange({target:{checked:true}});
  // The first two input elements represent the scope and Transmission writer confirmations.
  const all=(n)=>!n||typeof n!=='object'?[]:Array.isArray(n)?n.flatMap(all):[n,...all(n.props?.children)];
  for(const input of all(tree).filter(n=>n.type==='input'))input.props.onChange({target:{checked:true}});
  tree=h.render();const running=h.find(tree,'button','执行一次').props.onClick();await new Promise(r=>setImmediate(r));
  tree=h.render();const stop=h.find(tree,'button','停止后续执行');assert.equal(stop.props.disabled,false);stop.props.onClick();await new Promise(r=>setImmediate(r));assert.equal(stopped,1);finish();await running;await new Promise(r=>setImmediate(r));
 }finally{finish?.();h.cleanup()}
});
