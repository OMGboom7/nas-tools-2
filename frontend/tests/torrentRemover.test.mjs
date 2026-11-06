import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {createRequire} from 'node:module';
import {pathToFileURL} from 'node:url';
import test from 'node:test';
import ts from 'typescript';
import {createElement} from 'react';
import {renderToStaticMarkup} from 'react-dom/server';
const compile=s=>ts.transpileModule(s,{compilerOptions:{module:ts.ModuleKind.ESNext,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
const url=s=>'data:text/javascript;base64,'+Buffer.from(s).toString('base64');
const require=createRequire(import.meta.url);
const client=url(compile(readFileSync(new URL('../src/api/client.ts',import.meta.url),'utf8')));
const source=compile(readFileSync(new URL('../src/components/TorrentRemoverManager.tsx',import.meta.url),'utf8')).replace('"react/jsx-runtime"',JSON.stringify(pathToFileURL(require.resolve('react/jsx-runtime')).href)).replace('"react"',JSON.stringify(pathToFileURL(require.resolve('react')).href)).replace('"../api/client"',JSON.stringify(client));
const {TorrentRemoverManager,validTorrentRemoverStatus}=await import(url(source));
const status={fingerprint:'a'.repeat(64),goOnly:true,installed:true,enabled:true,active:false,epoch:'',targets:[{id:'9007199254740993',name:'Fixture',type:'transmission',receiver:'http://fixture.test:9091'}],records:[{event:{id:'c'.repeat(64),title:'Fixture movie',path:'/fixture/movie.mkv'},receipt:{id:'b'.repeat(64),eventId:'c'.repeat(64),state:'unknown',reason:'lost reply',created:'fixture',tasks:[{id:'d'.repeat(64),downloader:'9007199254740993',task:'e'.repeat(40),role:'original',state:'unknown',reason:'original intent retained'}]}}]};
test('removal status preserves original unknown receipts and exact downloader IDs',()=>{
 assert.equal(validTorrentRemoverStatus(status),true);
 for(const bad of [{fingerprint:'bad'},{goOnly:'true'},{targets:[{...status.targets[0],id:'1.5'}]},{records:[{...status.records[0],event:{...status.records[0].event,id:'f'.repeat(64)}}]},{records:[{...status.records[0],receipt:{...status.records[0].receipt,state:'fabricated'}}]}])assert.equal(validTorrentRemoverStatus({...status,...bad}),false);
});
test('removal UI renders an idle explanation without dispatching any request',t=>{
 let calls=0;t.mock.method(globalThis,'fetch',()=>{calls++;throw Error('unexpected network')});
 const html=renderToStaticMarkup(createElement(TorrentRemoverManager,{token:'fixture',onSessionExpired(){}}));assert.ok(html.includes('下载任务联动删除'));assert.ok(html.includes('保留全部文件'));assert.equal(calls,0);assert.equal(html.includes('checked'),false);
});
