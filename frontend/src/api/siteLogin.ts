import { ApiError } from "./client";

export type SiteLoginOptions = { siteIds: string[]; useSaved: boolean; remember: boolean; ocr: boolean };
export type SiteLoginStatus = { browserConfigured:boolean;goOnly:boolean;savedConfigured:boolean;maxSites:number };
export type SiteLoginReview = {id:string;scopeToken:string;expires:string;targets:{id:string;name:string;host:string;path:string;scheme:string}[];ocrReceivers:{kind:string;scheme:string;host:string;path:string}[]};
export type SiteLoginJob = {id:string;batch:string;siteId:string;name:string;host:string;actor:string;state:string;reason:string;loginState:string;created:string;updated:string;requests:number;actions:number;canStop:boolean;runtimeActive:boolean;challenge?:{kind:string;nonce:string;deadline:string;image?:string}};
export type SiteLoginBatch = {id:string;items:SiteLoginJob[];rememberState:string;rememberReason:string};
const hex = (value:unknown):value is string => typeof value === "string" && /^[a-f0-9]{64}$/.test(value);
const states = new Set(["queued","running","waiting_input","completed","failed","blocked","uncertain","stopped"]);
export const siteLoginActive = (state:string) => ["queued","running","waiting_input"].includes(state);
export function siteLoginJobValid(raw:unknown):raw is SiteLoginJob {
 const r=raw as SiteLoginJob;
 if(!r||!hex(r.id)||!hex(r.batch)||typeof r.siteId!=="string"||!/^[1-9][0-9]*$/.test(r.siteId)||!states.has(r.state))return false;
 if(!["name","host","actor","reason","loginState","created","updated"].every(k=>typeof (r as unknown as Record<string,unknown>)[k]==="string"))return false;
 if(!Number.isSafeInteger(r.requests)||r.requests<0||!Number.isSafeInteger(r.actions)||r.actions<0||typeof r.canStop!=="boolean"||typeof r.runtimeActive!=="boolean")return false;
 const c=r.challenge;
 return !c || r.state==="waiting_input"&&hex(c.nonce)&&["captcha","two_step","email","email_code"].includes(c.kind)&&typeof c.deadline==="string"&&(!c.image||c.image.length<3_000_000&&/^data:image\/(png|jpeg|gif);base64,[A-Za-z0-9+/=]+$/.test(c.image));
}
const batchValid=(r:unknown):r is SiteLoginBatch=>!!r&&hex((r as SiteLoginBatch).id)&&Array.isArray((r as SiteLoginBatch).items)&&(r as SiteLoginBatch).items.length<=50&&(r as SiteLoginBatch).items.every(i=>siteLoginJobValid(i)&&i.batch===(r as SiteLoginBatch).id)&&["not_requested","pending","saved","preserved","blocked","uncertain"].includes((r as SiteLoginBatch).rememberState)&&typeof (r as SiteLoginBatch).rememberReason==="string";
const statusValid=(r:unknown):r is SiteLoginStatus=>!!r&&["browserConfigured","goOnly","savedConfigured"].every(k=>typeof(r as unknown as Record<string,unknown>)[k]==="boolean")&&Number.isSafeInteger((r as SiteLoginStatus).maxSites)&&(r as SiteLoginStatus).maxSites>0;
const reviewValid=(r:unknown):r is SiteLoginReview=>{
 const v=r as SiteLoginReview;return !!v&&hex(v.id)&&typeof v.scopeToken==="string"&&v.scopeToken.length>20&&v.scopeToken.length<200000&&typeof v.expires==="string"&&Array.isArray(v.targets)&&v.targets.length>0&&v.targets.length<=50&&v.targets.every(t=>t&&typeof t.id==="string"&&/^[1-9][0-9]*$/.test(t.id)&&["name","host","path","scheme"].every(k=>typeof(t as unknown as Record<string,unknown>)[k]==="string")&&["http","https"].includes(t.scheme))&&Array.isArray(v.ocrReceivers)&&v.ocrReceivers.every(t=>t&&typeof t.kind==="string"&&["http","https"].includes(t.scheme)&&typeof t.host==="string"&&typeof t.path==="string");
};
async function data<T>(token:string,path:string,method="GET",body?:unknown,valid?:(raw:unknown)=>raw is T):Promise<T>{
 const response=await fetch("/api/v1/site-login"+path,{method,headers:{Authorization:token,...(body===undefined?{}:{"Content-Type":"application/json"})},...(body===undefined?{}:{body:JSON.stringify(body)})});
 let result:{code?:number;success?:boolean;message?:string;data?:unknown};
 try{result=await response.json();}catch{throw new ApiError("站点登录应答无法确认，请只读查询原编号",response.status);}
 if(!response.ok||result.code!==0||!result.success)throw new ApiError(result.message||"站点登录请求未完成",result.code||response.status);
 if(result.data===undefined||valid&&!valid(result.data))throw new ApiError("站点登录应答格式无效，请只读查询原编号",502);
 return result.data as T;
}
export function getSiteLoginStatus(token:string){return data<SiteLoginStatus>(token,"","GET",undefined,statusValid);}
export function reviewSiteLogin(token:string,options:SiteLoginOptions){return data<SiteLoginReview>(token,"/review","POST",options,reviewValid);}
export function startSiteLogin(token:string,input:{scopeToken:string;username:string;password:string;twoStepCode:string;confirm:true}){return data<SiteLoginBatch>(token,"/requests","POST",input,batchValid);}
export async function getSiteLoginBatch(token:string,id:string){if(!hex(id))throw new ApiError("原批次编号无效",400);const r=await data<SiteLoginBatch>(token,"/batches/"+id,"GET",undefined,batchValid);if(r.id!==id)throw new ApiError("应答不是原登录批次",502);return r;}
export async function getSiteLoginHistory(token:string,page:number){
 if(!Number.isSafeInteger(page)||page<1)throw new ApiError("页码无效",400);
 const out=await data<{page:number;items:SiteLoginJob[]}>(token,"/requests?page="+page);
 if(out.page!==page||!Array.isArray(out.items)||!out.items.every(siteLoginJobValid))throw new ApiError("原登录回执格式无效",502);return out;
}
export async function submitSiteLoginInput(token:string,id:string,nonce:string,value:string){
 if(!hex(id)||!hex(nonce))throw new ApiError("原验证码请求无效",400);
 const response=await fetch("/api/v1/site-login/requests/"+id+"/input",{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify({Nonce:nonce,Value:value})});
 const result=await response.json();if(!response.ok||result.code!==0||!result.success)throw new ApiError(result.message||"本次输入应答未确认，请刷新原请求",result.code||response.status);
}
export async function stopSiteLogin(token:string,id:string,reason:string){
 if(!hex(id))throw new ApiError("原请求编号无效",400);
 const response=await fetch("/api/v1/site-login/requests/"+id+"/stop",{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify({reason})});
 const result=await response.json();if(!response.ok||result.code!==0||!result.success)throw new ApiError(result.message||"本地停止应答未确认，请刷新",result.code||response.status);
}
