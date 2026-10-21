export type UserInfo = {
  userid: number | string;
  username: string;
  userpris: string[];
};

export type LibraryScrapeInput = { rootId: string; path: string; mode: "no_force" | "force_nfo" | "force_all"; excludes: string[]; requestId?: string; allowDiscExpansion?: boolean };
export type LibraryScrapePreview = { input: LibraryScrapeInput & { requestId: string }; fingerprint: string; total: number; skipped: number; blocked: number; sample: { path: string; blocked?: string; disc?: OrganizationDisc }[] };
export type LibraryScrapeJob = { id: string; state: string; mode: string; rootId?: string; path?: string; created: string; updated: string; reason: string; total: number; done: number; blocked: number };
export type LibraryScrapeOutput = { id: string; jobId: string; target: string; state: string; reason: string };
export type LibraryScrapeStatus = { roots: OrganizationRoot[]; jobs: LibraryScrapeJob[] };
export type LibraryScrapeDetail = { job: LibraryScrapeJob; items: { ordinal: number; path: string; state: string; reason: string; disc?: OrganizationDisc }[]; outputs: LibraryScrapeOutput[]; page: number; outputPage: number };
export type LibraryScrapeRecovery = { output: LibraryScrapeOutput; evidence: string; fingerprint: string; canRestore: boolean; canReconcile: boolean };
const libraryScrapeBase = "/api/v1/organization/library-scraper";
const libraryHash = (s: unknown): s is string => typeof s === "string" && /^[a-f0-9]{64}$/.test(s);
const libraryDiscValid = (s: unknown): boolean => {
  if(s===undefined)return true;
  const d=s as OrganizationDisc|null;
  return !!d && Number.isSafeInteger(d.files) && d.files>0 && Number.isSafeInteger(d.directories) && d.directories>=0 && d.files+d.directories+1<=20000 && typeof d.expanded==="boolean" && libraryHash(d.fingerprint) && Number.isFinite(d.latestModified) && d.latestModified>=0;
};
const libraryRelativePath = (s: unknown): s is string => typeof s==="string" && s.length>0 && (s==="." || !s.startsWith("/") && s.split("/").every(p=>p!=="" && p!=="." && p!=="..")) && !s.includes("\0");
const libraryPreviewValid = (p: LibraryScrapePreview, input: LibraryScrapeInput): boolean => {
  const selected=input.path||".";
  return !!p && libraryHash(p.fingerprint) && /^[a-f0-9]{32}$/.test(p.input?.requestId) && libraryRelativePath(selected)
    && [p.total,p.blocked,p.skipped].every(n=>Number.isSafeInteger(n) && n>=0) && p.total<=50000 && p.blocked<=p.total
    && Array.isArray(p.sample) && p.sample.length<=Math.min(200,p.total) && p.sample.every(m=>!!m && libraryRelativePath(m.path) && (m.blocked===undefined || typeof m.blocked==="string") && libraryDiscValid(m.disc)
      && (m.disc?.expanded ? p.input.allowDiscExpansion===true && selected!==m.path && (m.path==="." || selected.startsWith(m.path+"/")) : selected==="." || m.path===selected || m.path.startsWith(selected+"/")))
    && p.input.rootId===input.rootId && p.input.mode===input.mode && p.input.path===selected && (p.input.allowDiscExpansion===undefined || typeof p.input.allowDiscExpansion==="boolean") && !!p.input.allowDiscExpansion===!!input.allowDiscExpansion
    && (input.requestId===undefined || input.requestId===p.input.requestId) && Array.isArray(p.input.excludes) && p.input.excludes.every(libraryRelativePath) && Array.isArray(input.excludes)
    && [...p.input.excludes].sort().join("\0")===[...input.excludes].sort().join("\0");
};
const libraryItemValid = (i: LibraryScrapeDetail["items"][number]): boolean => !!i && libraryRelativePath(i.path) && typeof i.state==="string" && typeof i.reason==="string" && Number.isSafeInteger(i.ordinal) && i.ordinal>=0 && libraryDiscValid(i.disc);
const libraryJobValid = (s: unknown): s is LibraryScrapeJob => {
  const j = s as LibraryScrapeJob | null;
  return !!j && libraryHash(j.id) && ["ready","running","completed","partial","needs_review","cancelled"].includes(j.state) && ["no_force","force_nfo","force_all"].includes(j.mode) && [j.total,j.done,j.blocked].every((v)=>Number.isSafeInteger(v) && v>=0) && j.done+j.blocked<=j.total;
};
const libraryOutputValid = (s: unknown): s is LibraryScrapeOutput => { const o=s as LibraryScrapeOutput | null;return !!o && libraryHash(o.id) && libraryHash(o.jobId) && typeof o.target==="string" && typeof o.state==="string" && typeof o.reason==="string"; };
async function libraryRequest<T>(token:string,path:string,body?:unknown):Promise<T> {
  const result=await readResponse<T>(await fetch(libraryScrapeBase+path,body===undefined ? {headers:{Authorization:token}} : {method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(body)}));
  if(!result.success || result.code!==0 || result.data==null)throw new ApiError(result.message || "媒体库刮削未核实，请只读刷新，不要重发",result.code);
  return result.data;
}
export async function getLibraryScraper(token:string):Promise<LibraryScrapeStatus>{
  const s=await libraryRequest<LibraryScrapeStatus>(token,"");
  if(!Array.isArray(s.roots) || !s.roots.every((r)=>typeof r.id==="string" && typeof r.path==="string" && typeof r.label==="string") || !Array.isArray(s.jobs) || !s.jobs.every(libraryJobValid))throw new ApiError("媒体库任务响应不完整",503);return s;
}
export async function previewLibraryScrape(token:string,input:LibraryScrapeInput):Promise<LibraryScrapePreview>{
  const p=await libraryRequest<LibraryScrapePreview>(token,"/preview",input);
  if(!libraryPreviewValid(p,input))throw new ApiError("媒体库范围审核不完整",503);return p;
}
export async function createLibraryScrape(token:string,preview:LibraryScrapePreview,confirm=false,confirmOldExecutorsStopped=false,confirmOverwrite=false):Promise<LibraryScrapeJob>{
  if(!confirm || !confirmOldExecutorsStopped || preview.input.mode!=="no_force" && !confirmOverwrite || !libraryPreviewValid(preview,preview.input))throw new ApiError("请先审核范围、停止旧执行器并独立确认覆盖",400);
  const job=await libraryRequest<LibraryScrapeJob>(token,"/jobs",{...preview.input,fingerprint:preview.fingerprint,confirm,confirmOldExecutorsStopped,confirmOverwrite});if(!libraryJobValid(job) || job.mode!==preview.input.mode || job.total!==preview.total)throw new ApiError("任务提交回执未核实，请只读刷新",503);return job;
}
export async function getLibraryScrapeJob(token:string,id:string,page=1,outputPage=1):Promise<LibraryScrapeDetail>{
  if(!libraryHash(id) || !Number.isSafeInteger(page) || page<1 || !Number.isSafeInteger(outputPage) || outputPage<1)throw new ApiError("无效任务或页码",400);
  const d=await libraryRequest<LibraryScrapeDetail>(token,`/jobs/${id}?${new URLSearchParams({page:String(page),outputPage:String(outputPage)})}`);
  if(!libraryJobValid(d.job) || d.job.id!==id || !Array.isArray(d.items) || !d.items.every(libraryItemValid) || !Array.isArray(d.outputs) || !d.outputs.every(o=>libraryOutputValid(o) && o.jobId===id) || d.page!==page || d.outputPage!==outputPage)throw new ApiError("任务详情未核实",503);return d;
}
export async function cancelLibraryScrape(token:string,id:string,confirm=false):Promise<LibraryScrapeDetail>{
  if(!confirm || !libraryHash(id))throw new ApiError("请明确确认停止后续写入",400);
  const d=await libraryRequest<LibraryScrapeDetail>(token,`/jobs/${id}/cancel`,{confirm});if(!libraryJobValid(d.job) || d.job.id!==id || d.job.state!=="cancelled" || !Array.isArray(d.items) || !d.items.every(libraryItemValid) || !Array.isArray(d.outputs) || !d.outputs.every(o=>libraryOutputValid(o) && o.jobId===id))throw new ApiError("停止回执未核实，请只读刷新",503);return d;
}
export async function reviewLibraryScrapeOutput(token:string,id:string):Promise<LibraryScrapeRecovery>{
  if(!libraryHash(id))throw new ApiError("无效输出回执",400);
  const d=await libraryRequest<LibraryScrapeRecovery>(token,`/outputs/${id}`);if(!libraryHash(d.fingerprint) || !libraryOutputValid(d.output) || d.output.id!==id || typeof d.canRestore!=="boolean" || typeof d.canReconcile!=="boolean" || typeof d.evidence!=="string")throw new ApiError("恢复证据未核实",503);return d;
}
export async function recoverLibraryScrapeOutput(token:string,review:LibraryScrapeRecovery,action:"reconcile"|"restore",confirm=false,confirmOldExecutorsStopped=false):Promise<LibraryScrapeOutput>{
  if(!confirm || !confirmOldExecutorsStopped || !libraryHash(review.fingerprint) || !libraryOutputValid(review.output) || action==="restore" && !review.canRestore || action==="reconcile" && !review.canReconcile || !["restore","reconcile"].includes(action))throw new ApiError("请重新核查当前证据并明确确认本次动作",400);
  const o=await libraryRequest<LibraryScrapeOutput>(token,`/outputs/${review.output.id}/${action}`,{fingerprint:review.fingerprint,confirm,confirmOldExecutorsStopped});if(!libraryOutputValid(o) || o.id!==review.output.id || o.jobId!==review.output.jobId || o.target!==review.output.target || o.state!==(action==="restore"?"restored":review.evidence))throw new ApiError("恢复回执未核实，请只读刷新",503);return o;
}

export type ScraperOptions = { scraper_nfo: Record<string, Record<string, boolean>>; scraper_pic: Record<string, Record<string, boolean>> };
export type ScraperReceipt = { id: string; jobId: string; ordinal: number; name: string; state: string; reason?: string; updated: string; results: { name: string; state: string; reason?: string }[] };
export type ScraperStatus = { enabled: boolean; options: ScraperOptions; fields: ScraperOptions; fingerprint: string; warnings: string[]; receipts: ScraperReceipt[] };
function verifiedScraperStatus(value: unknown): value is ScraperStatus {
  const s = value as ScraperStatus | null;
  return !!s && typeof s.enabled === "boolean" && /^[a-f0-9]{64}$/.test(s.fingerprint) && !!s.options?.scraper_nfo && !!s.options.scraper_pic && !!s.fields?.scraper_nfo && !!s.fields.scraper_pic && Array.isArray(s.warnings) && Array.isArray(s.receipts) && s.receipts.every((row) => typeof row.id === "string" && typeof row.jobId === "string" && Array.isArray(row.results));
}
export async function getIntakeScraper(token: string): Promise<ScraperStatus> {
  const result = await readResponse<ScraperStatus>(await fetch("/api/v1/organization/scraper", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !verifiedScraperStatus(result.data)) throw new ApiError(result.message || "刮削配置和回执不可用", result.code);
  return result.data;
}
export async function saveIntakeScraperOptions(token: string, fingerprint: string, options: ScraperOptions, confirm = false): Promise<ScraperStatus> {
  if (!confirm || !/^[a-f0-9]{64}$/.test(fingerprint)) throw new ApiError("请审核并明确确认刮削配置", 400);
  const result = await readResponse<ScraperStatus>(await fetch("/api/v1/organization/scraper/options", { method: "PUT", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ fingerprint, options, confirm }) }));
  if (!result.success || result.code !== 0 || !verifiedScraperStatus(result.data)) throw new ApiError(result.message || "配置保存未核实，请只读刷新，不要自动重试", result.code);
  return result.data;
}
export async function setIntakeScraperEnabled(token: string, enabled: boolean, confirm = false): Promise<ScraperStatus> {
  if (!confirm || typeof enabled !== "boolean") throw new ApiError("请独立确认入库刮削开关", 400);
  const result = await readResponse<ScraperStatus>(await fetch("/api/v1/organization/scraper/enabled", { method: "PUT", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ enabled, confirm }) }));
  if (!result.success || result.code !== 0 || !verifiedScraperStatus(result.data) || result.data.enabled !== enabled) throw new ApiError(result.message || "开关保存未核实，请只读刷新", result.code);
  return result.data;
}

export type MediaSyncLibrary = { id: string; name: string; type: "movie" | "show" };
export type MediaSyncCatalog = { server: string; fingerprint: string; libraries: MediaSyncLibrary[] };
export type MediaSyncSnapshot = { server: string; generation: string; configDigest: string; libraries: string[]; actor: string; updated: string; total: number; movies: number; series: number; page: number; items: { id: string; library: string; title: string; type: string; year?: string; tmdbid?: string; episodes: { season_num: number; episode_num: number }[] }[] };
export type MediaSyncStatus = { snapshot: MediaSyncSnapshot; current: boolean; displayOnly: true };
export type MediaCachePresence = { state: "present" | "not-recorded" | "unavailable"; seasons: number; episodes: number };
export type MediaCacheSummary = { state: "ready" | "unsynced" | "scope-changed" | "unavailable"; server: string; generation: string; updated: string; libraries: string[]; displayOnly: true };
export type MediaCacheQuery = { tmdbid: string; title: string; year: string; type: "movie" | "show"; season?: number; episode?: number };
export async function lookupMediaCache(token: string, items: MediaCacheQuery[]): Promise<{ cache: MediaCacheSummary; results: MediaCachePresence[] }> {
  if (!items.length || items.length > 200) throw new ApiError("每次查询 1 至 200 个展示身份", 400);
  const result = await readResponse<{ cache: MediaCacheSummary; results: MediaCachePresence[] }>(await fetch("/api/v1/library/sync/lookup", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ items }) }));
  if (!result.success || result.code !== 0 || result.data?.cache?.displayOnly !== true || !Array.isArray(result.data.results) || result.data.results.length !== items.length) throw new ApiError(result.message || "媒体展示快照不可用", result.code);
  return result.data;
}
export type MediaSyncProgress = { stage: string; library: string; librariesDone: number; librariesTotal: number; itemsDone: number; episodesDone: number; pagesRead: number };
export type MediaSyncActivity = { id: string; kind: string; server: string; actor: string; libraries: string[]; state: string; reason: string; started: string; updated: string; finished: string; snapshotGeneration: string; progress: MediaSyncProgress };
export async function getMediaSyncProgress(token: string, signal?: AbortSignal): Promise<MediaSyncActivity[]> {
  const result = await readResponse<{ activities: MediaSyncActivity[]; indeterminate: boolean }>(await fetch("/api/v1/library/sync/progress", { headers: { Authorization: token }, signal }));
  if (!result.success || result.code !== 0 || !Array.isArray(result.data?.activities) || result.data.activities.length > 50 || result.data.activities.some((a) => !/^[a-f0-9]{32}$/.test(a.id) || !["manual", "scheduled"].includes(a.kind) || !["collecting", "completed", "failed", "interrupted", "stopped", "paused", "superseded"].includes(a.state) || !a.progress || [a.progress.librariesDone, a.progress.librariesTotal, a.progress.itemsDone, a.progress.episodesDone, a.progress.pagesRead].some((n) => !Number.isInteger(n) || n < 0) || a.state === "completed" && !/^[a-f0-9]{32}$/.test(a.snapshotGeneration))) throw new ApiError(result.message || "同步进度不可用", result.code);
  return result.data.activities;
}
export type MediaScheduleDefinition = { server: string; configDigest: string; authorityDigest: string; actor: string; libraries: string[]; intervalSeconds: number };
export type MediaSyncSchedule = { revision: string; enabled: boolean; definition: MediaScheduleDefinition; nextRun: string; runId: string; lastState: string; reason: string; changedBy: string; updated: string };
export type MediaScheduledRun = { id: string; revision: string; definition: MediaScheduleDefinition; state: string; reason: string; started: string; finished: string; snapshotGeneration: string };
export type MediaScheduleStatus = { schedule: MediaSyncSchedule; runs: MediaScheduledRun[]; current: boolean; workerRunning: boolean; suggestedHours: number; warning: string };
export type MediaScheduleReview = { definition: MediaScheduleDefinition; revision: string; replacesEnabled: boolean; fingerprint: string };
export async function getMediaSchedule(token: string): Promise<MediaScheduleStatus> {
  const result = await readResponse<MediaScheduleStatus>(await fetch("/api/v1/library/sync/schedule", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || typeof result.data?.schedule?.enabled !== "boolean" || !Array.isArray(result.data.runs)) throw new ApiError(result.message || "定时同步状态不可用", result.code);
  return result.data;
}
export async function reviewMediaSchedule(token: string, catalog: MediaSyncCatalog, libraries: string[], hours: number): Promise<MediaScheduleReview> {
  if (!Number.isFinite(hours) || hours < 1 / 60 || hours > 8760 || !libraries.length || libraries.length > 100 || new Set(libraries).size !== libraries.length || libraries.some((id) => !catalog.libraries.some((library) => library.id === id)) || !/^[a-f0-9]{64}$/.test(catalog.fingerprint)) throw new ApiError("请选择媒体库和 1 分钟至 1 年的周期", 400);
  const result = await readResponse<MediaScheduleReview>(await fetch("/api/v1/library/sync/schedule/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ libraries, hours, configFingerprint: catalog.fingerprint }) }));
  const d = result.data?.definition;
  if (!result.success || result.code !== 0 || !result.data || !/^[a-f0-9]{64}$/.test(result.data.fingerprint || "") || d?.server !== catalog.server || d.configDigest !== catalog.fingerprint || d.intervalSeconds !== Math.round(hours * 3600) || !Array.isArray(d.libraries) || [...d.libraries].sort().join("\0") !== [...libraries].sort().join("\0")) throw new ApiError(result.message || "定时同步审核范围不匹配", result.code);
  return result.data;
}
export async function activateMediaSchedule(token: string, review: MediaScheduleReview, confirm = false, legacyStopped = false): Promise<MediaSyncSchedule> {
  if (!confirm || !legacyStopped || !/^[a-f0-9]{64}$/.test(review.fingerprint)) throw new ApiError("请分别确认周期执行和旧同步任务已经停止", 400);
  const result = await readResponse<MediaSyncSchedule>(await fetch("/api/v1/library/sync/schedule/activate", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ libraries: review.definition.libraries, hours: review.definition.intervalSeconds / 3600, configFingerprint: review.definition.configDigest, fingerprint: review.fingerprint, confirm, legacyStopped }) }));
  if (!result.success || result.code !== 0 || result.data?.enabled !== true || !/^[a-f0-9]{32}$/.test(result.data.revision || "")) throw new ApiError(result.message || "授权响应未核实，请只读刷新状态", result.code);
  return result.data;
}
export async function stopMediaSchedule(token: string, revision: string, confirm = false): Promise<MediaSyncSchedule> {
  if (!confirm || !/^[a-f0-9]{32}$/.test(revision)) throw new ApiError("请明确确认停止当前代次", 400);
  const result = await readResponse<MediaSyncSchedule>(await fetch("/api/v1/library/sync/schedule/stop", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ revision, confirm }) }));
  if (!result.success || result.code !== 0 || result.data?.enabled !== false) throw new ApiError(result.message || "停止响应未核实，请只读刷新状态", result.code);
  return result.data;
}
export async function getMediaSyncStatus(token: string, page = 1): Promise<MediaSyncStatus> {
  if (!Number.isInteger(page) || page < 1 || page > 1000) throw new ApiError("媒体快照页码无效", 400);
  const result = await readResponse<MediaSyncStatus>(await fetch(`/api/v1/library/sync?page=${page}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || result.data?.displayOnly !== true || !Array.isArray(result.data.snapshot?.items)) throw new ApiError(result.message || "媒体快照不可用", result.code);
  return result.data;
}
export async function getMediaSyncLibraries(token: string): Promise<MediaSyncCatalog> {
  const result = await readResponse<MediaSyncCatalog>(await fetch("/api/v1/library/sync/libraries", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !/^[a-f0-9]{64}$/.test(result.data?.fingerprint || "") || !Array.isArray(result.data?.libraries)) throw new ApiError(result.message || "媒体库列表不可用", result.code);
  return result.data;
}
export async function synchronizeMediaLibraries(token: string, catalog: MediaSyncCatalog, libraries: string[], confirm = false, legacyStopped = false): Promise<MediaSyncSnapshot> {
  if (!confirm || !legacyStopped || !/^[a-f0-9]{64}$/.test(catalog.fingerprint) || !libraries.length || libraries.length > 100 || new Set(libraries).size !== libraries.length || libraries.some((id) => !catalog.libraries.some((library) => library.id === id))) throw new ApiError("请选择媒体库，确认停止旧同步任务并确认替换展示快照", 400);
  const result = await readResponse<MediaSyncSnapshot>(await fetch("/api/v1/library/sync", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ libraries, confirm, legacyStopped, fingerprint: catalog.fingerprint }) }));
  if (!result.success || result.code !== 0 || result.data?.server !== catalog.server || !/^[a-f0-9]{32}$/.test(result.data?.generation || "") || result.data.configDigest !== catalog.fingerprint) throw new ApiError(result.message || "同步响应未核实，请只读刷新快照", result.code);
  return result.data;
}

export type TransmissionLimits = { uploadKB: number; downloadKB: number; ratio: number; idleMinutes: number };
export type TransmissionProfile = { id: string; state: string; reason?: string; fingerprint: string; downloaderId: string; name: string; infoHash: string; options: { DownloadDir: string; Paused: boolean; Labels: string[] }; limits: TransmissionLimits; seedingPolicyId?: string; created: string; updated: string };
export type TransmissionProfileReview = { profile: TransmissionProfile; current: { hash: string; directory: string; labels: string[]; status: number; uploadKB: number; downloadKB: number; uploadLimited: boolean; downloadLimited: boolean; ratio: number; ratioMode: number; idleMinutes: number; idleMode: number }; next: "configure" | "start" | "acknowledge"; actor: string; fingerprint: string };
export async function getTransmissionProfiles(token: string, page = 1): Promise<TransmissionProfile[]> {
  if (!Number.isInteger(page) || page < 1 || page > 10000) throw new ApiError("设置回执页码无效", 400);
  const result = await readResponse<TransmissionProfile[]>(await fetch(`/api/v1/downloads/transmission-profiles?page=${page}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !Array.isArray(result.data)) throw new ApiError(result.message || "Transmission 设置回执不可用", result.code);
  return result.data;
}
export async function reviewTransmissionProfile(token: string, id: string): Promise<TransmissionProfileReview> {
  if (!/^[a-f0-9]{32}$/.test(id)) throw new ApiError("设置回执 ID 无效", 400);
  const result = await readResponse<TransmissionProfileReview>(await fetch(`/api/v1/downloads/transmission-profiles/${id}/review`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: "{}" }));
  if (!result.success || result.code !== 0 || result.data?.profile?.id !== id || !/^[a-f0-9]{64}$/.test(result.data.fingerprint) || !["configure", "start", "acknowledge"].includes(result.data.next)) throw new ApiError(result.message || "任务身份或设置证据不能核实", result.code);
  return result.data;
}
export async function recoverTransmissionProfile(token: string, review: TransmissionProfileReview, confirm = false, confirmSettings = false, confirmStart = false): Promise<TransmissionProfile> {
  if (!confirm || !/^[a-f0-9]{32}$/.test(review.profile.id) || !/^[a-f0-9]{64}$/.test(review.fingerprint) || review.next === "configure" && !confirmSettings || review.next === "start" && !confirmStart) throw new ApiError("先重新审核，并分别确认设置写入或启动任务", 400);
  const result = await readResponse<TransmissionProfile>(await fetch(`/api/v1/downloads/transmission-profiles/${review.profile.id}/recover`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm, fingerprint: review.fingerprint, confirmSettings, confirmStart }) }));
  if (!result.success || result.code !== 0 || result.data?.id !== review.profile.id) throw new ApiError(result.message || "设置响应不确定，请只读刷新，不重放操作", result.code);
  return result.data;
}

export type ManualDownloadReceipt = { id: string; state: string; taskId?: string; reason?: string; created: string; updated: string; downloaderId: string; downloaderName: string; downloaderType: string; infoHash: string; actor: string; title: string; source: string };
export async function getManualDownloadReceipts(token: string, page = 1): Promise<ManualDownloadReceipt[]> {
  if (!Number.isInteger(page) || page < 1 || page > 10000) throw new ApiError("提交回执页码无效", 400);
  const result = await readResponse<ManualDownloadReceipt[]>(await fetch(`/api/v1/downloads/submissions?page=${page}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !Array.isArray(result.data)) throw new ApiError(result.message || "手动下载回执不可用", result.code);
  return result.data;
}
export async function reconcileManualDownload(token: string, id: string, confirm = false): Promise<ManualDownloadReceipt> {
  if (!confirm || !/^[a-f0-9]{32}$/.test(id)) throw new ApiError("需明确确认只读核实原下载器，不重新下种", 400);
  const result = await readResponse<ManualDownloadReceipt>(await fetch(`/api/v1/downloads/submissions/${encodeURIComponent(id)}/reconcile`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm }) }));
  if (!result.success || result.code !== 0 || result.data?.id !== id || result.data.state !== "submitted") throw new ApiError(result.message || "尚无正向任务证据，请保留待核实回执，不重试下种", result.code);
  return result.data;
}

export type BusinessDelivery = { id: string; eventId: string; channelId: number; channelName: string; channelType: string; state: string; reason?: string; actor?: string; updated: string };
export type BusinessEvent = { id: string; kind: string; reference: string; title: string; text: string; created: string; deliveries: BusinessDelivery[] };
export async function getBusinessEvents(token: string): Promise<BusinessEvent[]> {
  const result = await readResponse<BusinessEvent[]>(await fetch("/api/v1/notifications/events", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !Array.isArray(result.data)) throw new ApiError(result.message || "业务通知回执不可用", result.code);
  return result.data;
}
export async function dismissBusinessDelivery(token: string, id: string, reason: string, confirm = false): Promise<void> {
  if (!confirm || !/^[a-f0-9]{64}$/.test(id) || !reason.trim() || reason.length > 1000) throw new ApiError("需明确确认及填写处置原因；不会重发消息", 400);
  const result = await readResponse<unknown>(await fetch(`/api/v1/notifications/events/${encodeURIComponent(id)}/dismiss`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm, reason }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "处置响应未核实，请只读刷新，不自动重试", result.code);
}

export type AuthSession = {
  token: string;
  user: UserInfo;
};

export type SeedingScope = { downloaderId: number; taskId: string };
export type QbProfile = { id: string; fingerprint: string; state: string; created: string; updated: string; downloaderId: number; name: string; infoHash: string; seedingPolicyId: string; options: { SavePath: string; Category: string; Tags: string[]; Paused: boolean; UploadLimitKB: number; DownloadLimitKB: number } };
export type QbProfileReview = { profile: QbProfile; current: { hash: string; version: string; state: string; directory: string; category: string; tags: string[]; ratioLimit: number; seedLimit: number; idleLimit: number; effectiveRatio: number; effectiveSeed: number; effectiveIdle: number; shareAction: string }; next: "configure" | "start" | "acknowledge"; actor: string; fingerprint: string };
export async function getQbProfiles(token: string, page = 1): Promise<QbProfile[]> {
  if (!Number.isInteger(page) || page < 1 || page > 10000) throw new ApiError("qB 回执页码无效", 400);
  const result = await readResponse<QbProfile[]>(await fetch(`/api/v1/downloads/qbittorrent-profiles?page=${page}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !Array.isArray(result.data)) throw new ApiError(result.message || "qB 设置回执不可用", result.code);
  return result.data;
}
export async function reviewQbProfile(token: string, id: string): Promise<QbProfileReview> {
  if (!/^[a-f0-9]{32}$/.test(id)) throw new ApiError("qB 回执身份无效", 400);
  const result = await readResponse<QbProfileReview>(await fetch(`/api/v1/downloads/qbittorrent-profiles/${id}/review`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: "{}" }));
  const data = result.data;
  if (!result.success || result.code !== 0 || data?.profile?.id !== id || !/^qb:[a-f0-9]{64}$/.test(data.profile.fingerprint) || !/^[a-f0-9]{64}$/.test(data.profile.seedingPolicyId) || !/^[a-f0-9]{64}$/.test(data.fingerprint) || data.current?.hash !== data.profile.infoHash || !["configure", "start", "acknowledge"].includes(data.next)) throw new ApiError(result.message || "qB 原任务审核范围不可核实", result.code);
  return data;
}
export async function recoverQbProfile(token: string, review: QbProfileReview, confirm = false, confirmSettings = false, confirmStart = false): Promise<void> {
  if (!confirm || !/^[a-f0-9]{32}$/.test(review.profile.id) || !/^[a-f0-9]{64}$/.test(review.fingerprint) || !["configure", "start", "acknowledge"].includes(review.next) || review.next === "configure" && !confirmSettings || review.next === "start" && !confirmStart) throw new ApiError("需明确确认当前审核范围及设置/启动下一步", 400);
  const result = await readResponse<QbProfile>(await fetch(`/api/v1/downloads/qbittorrent-profiles/${review.profile.id}/recover`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm, fingerprint: review.fingerprint, confirmSettings, confirmStart }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "qB 恢复响应不确定，请只读刷新，不重发或重新添加任务", result.code);
}
export type PTAdmissionGrant = { id: string; downloaderId: number; actor: string; created: string; enabled: boolean };
export type PTAdmissionReview = { downloaderId: number; name: string; type: string; grant: PTAdmissionGrant | null; active: boolean; enabled: boolean; fingerprint: string; warning: string };
function validatePTAdmissionId(id: number) { if (!Number.isSafeInteger(id) || id < 1) throw new ApiError("需准确下载器 ID", 400); }
export async function getPTAdmission(token: string, id: number): Promise<PTAdmissionReview> {
  validatePTAdmissionId(id);
  const result = await readResponse<PTAdmissionReview>(await fetch(`/api/v1/downloads/pt-admission?downloaderId=${id}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || result.data?.downloaderId !== id) throw new ApiError(result.message || "PT 接管授权无法核实", result.code);
  return result.data;
}
export async function reviewPTAdmission(token: string, id: number, enabled: boolean): Promise<PTAdmissionReview> {
  validatePTAdmissionId(id);
  const result = await readResponse<PTAdmissionReview>(await fetch("/api/v1/downloads/pt-admission/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ downloaderId: id, enabled }) }));
  if (!result.success || result.code !== 0 || result.data?.downloaderId !== id || result.data.enabled !== enabled || !/^[a-f0-9]{64}$/.test(result.data.fingerprint)) throw new ApiError(result.message || "PT 接管审核范围无法核实", result.code);
  return result.data;
}
export async function applyPTAdmission(token: string, review: PTAdmissionReview, confirm = false, confirmOldExecutorsStopped = false, confirmUnknownHold = false, confirmUnlimitedSeeding = false): Promise<void> {
  validatePTAdmissionId(review.downloaderId);
  if (!confirm || !/^[a-f0-9]{64}$/.test(review.fingerprint) || review.enabled && (review.type !== "transmission" && review.type !== "qbittorrent" || !confirmOldExecutorsStopped || !confirmUnknownHold || !confirmUnlimitedSeeding)) throw new ApiError("需独立确认原范围、旧执行器停止、未知约束持续拦截及禁用自动停种上限", 400);
  const result = await readResponse<PTAdmissionGrant>(await fetch("/api/v1/downloads/pt-admission/apply", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ downloaderId: review.downloaderId, enabled: review.enabled, fingerprint: review.fingerprint, confirm, confirmOldExecutorsStopped, confirmUnknownHold, confirmUnlimitedSeeding }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "接管授权响应不确定；只读刷新，不自动重发", result.code);
}
export type SeedingRequirements = { seconds: number | null; ratio: number | null; hr: boolean | null };
export type SeedingPolicy = { id: string; downloaderId: number; downloaderName: string; downloaderType: string; infoHash: string; title: string; source: string; requirements: SeedingRequirements; actor: string; created: string };
export type SeedingEvidence = { hash: string; state: string; complete: boolean | null; seconds: number | null; ratio: number | null; addedAt?: number | null };
export type SeedingDecision = { allowed: boolean; reasons: string[]; basis?: "operator-attestation" };
export type SeedingPolicyStatus = { registered: boolean; policies: SeedingPolicy[]; evidence: SeedingEvidence; decision: SeedingDecision; actions?: Record<"stop" | "remove" | "move", SeedingDecision>; manualVerification?: PTVerificationState; warning: string };
export type VerificationActions = { stop: boolean; remove: boolean; move: boolean };
export type PTVerification = { id: string; kind: "operator-attestation"; downloaderId: number; infoHash: string; policyDigest: string; policyIds: string[]; actor: string; note: string; actions: VerificationActions; observed: SeedingEvidence; created: string; expires: string };
export type PTVerificationState = { record: PTVerification | null; revoked: boolean; revokedBy?: string; revokedAt?: string };
export type PTVerificationInput = SeedingScope & { mode: "attest" | "revoke"; minutes: number; note: string; actions: VerificationActions };
export type PTVerificationReview = PTVerificationInput & { name: string; policies: SeedingPolicy[]; evidence: SeedingEvidence | null; current: PTVerificationState; fingerprint: string; reviewExpires: string; warning: string };
export async function getPTVerification(token: string, scope: SeedingScope): Promise<PTVerificationState> {
  validateSeedingScope(scope);
  const result = await readResponse<PTVerificationState>(await fetch(`/api/v1/downloads/pt-verification?downloaderId=${scope.downloaderId}&taskId=${encodeURIComponent(scope.taskId)}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data || result.data.record && (result.data.record.downloaderId !== scope.downloaderId || result.data.record.infoHash !== scope.taskId.toLowerCase() || result.data.record.kind !== "operator-attestation")) throw new ApiError(result.message || "限时人工核实记录不可核实", result.code);
  return result.data;
}
export async function reviewPTVerification(token: string, input: PTVerificationInput): Promise<PTVerificationReview> {
  validateSeedingScope(input);
  if (!["attest", "revoke"].includes(input.mode) || input.mode === "attest" && (!Number.isInteger(input.minutes) || input.minutes < 1 || input.minutes > 30 || !input.note.trim() || !Object.values(input.actions).some(Boolean))) throw new ApiError("需选择准确范围、有效期、动作及核实说明", 400);
  const result = await readResponse<PTVerificationReview>(await fetch("/api/v1/downloads/pt-verification/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  const data = result.data;
  if (!result.success || result.code !== 0 || !data || data.downloaderId !== input.downloaderId || data.taskId !== input.taskId.toLowerCase() || data.mode !== input.mode || data.minutes !== input.minutes || data.note !== input.note.trim() || !(["stop", "remove", "move"] as const).every((key) => data.actions?.[key] === input.actions[key]) || !/^[a-f0-9]{64}$/.test(data.fingerprint) || !Number.isFinite(Date.parse(data.reviewExpires)) || input.mode === "attest" && (data.evidence?.hash !== input.taskId.toLowerCase() || !Number.isSafeInteger(data.evidence.addedAt) || (data.evidence.addedAt ?? 0) < 1)) throw new ApiError(result.message || "人工核实审核的准确范围不可建立", result.code);
  return data;
}
export async function applyPTVerification(token: string, review: PTVerificationReview, confirm = false, confirmAccountReviewed = false, confirmAutomationScope = false, confirmOldExecutorsStopped = false): Promise<void> {
  validateSeedingScope(review);
  if (!confirm || !/^[a-f0-9]{64}$/.test(review.fingerprint) || !Number.isFinite(Date.parse(review.reviewExpires)) || Date.parse(review.reviewExpires) <= Date.now() || !["attest", "revoke"].includes(review.mode) || review.mode === "attest" && (!confirmAccountReviewed || !confirmAutomationScope || !confirmOldExecutorsStopped)) throw new ApiError("需有效审核及独立确认账户核实、自动流程影响与旧执行器停止", 400);
  const { downloaderId, taskId, mode, minutes, note, actions, fingerprint } = review;
  const result = await readResponse<unknown>(await fetch(`/api/v1/downloads/pt-verification/${mode === "attest" ? "record" : "revoke"}`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ downloaderId, taskId, mode, minutes, note, actions, fingerprint, confirm, confirmAccountReviewed, confirmAutomationScope, confirmOldExecutorsStopped }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "声明响应不确定，请只读刷新，不自动重发或执行任务", result.code);
}
export type SeedingPolicyReview = { policy: SeedingPolicy; policies: SeedingPolicy[]; evidence: SeedingEvidence; decision: SeedingDecision; fingerprint: string; needsUnknownHold: boolean; warning: string };
function validateSeedingScope(scope: SeedingScope) {
  if (!Number.isSafeInteger(scope.downloaderId) || scope.downloaderId < 1 || !/^[a-fA-F0-9]{40}(?:[a-fA-F0-9]{24})?$/.test(scope.taskId)) throw new ApiError("需准确的下载器 ID 和完整任务 hash", 400);
}
export async function getSeedingEvidence(token: string, scope: SeedingScope): Promise<SeedingPolicyStatus> {
  validateSeedingScope(scope);
  const result = await readResponse<SeedingPolicyStatus>(await fetch(`/api/v1/downloads/seeding?downloaderId=${scope.downloaderId}&taskId=${encodeURIComponent(scope.taskId)}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !Array.isArray(result.data?.policies) || result.data.evidence?.hash !== scope.taskId.toLowerCase()) throw new ApiError(result.message || "做种证据不可用；不会授权任务操作", result.code);
  return result.data;
}
export async function reviewSeedingPolicy(token: string, scope: SeedingScope & { resourceId: string }): Promise<SeedingPolicyReview> {
  validateSeedingScope(scope);
  if (!/^go-search-[a-f0-9]{32}$/.test(scope.resourceId)) throw new ApiError("需当前用户未过期的搜索资源", 400);
  const result = await readResponse<SeedingPolicyReview>(await fetch("/api/v1/downloads/seeding/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(scope) }));
  if (!result.success || result.code !== 0 || !result.data || !/^[a-f0-9]{64}$/.test(result.data.fingerprint) || result.data.policy?.downloaderId !== scope.downloaderId || result.data.policy.infoHash !== scope.taskId.toLowerCase() || typeof result.data.needsUnknownHold !== "boolean") throw new ApiError(result.message || "接管审核范围无法核实", result.code);
  return result.data;
}
export async function bindSeedingPolicy(token: string, scope: SeedingScope & { resourceId: string }, review: SeedingPolicyReview, confirm = false, confirmOldExecutorsStopped = false, confirmUnknownHold = false): Promise<void> {
  validateSeedingScope(scope);
  if (!/^go-search-[a-f0-9]{32}$/.test(scope.resourceId) || !confirm || !confirmOldExecutorsStopped || review.needsUnknownHold && !confirmUnknownHold || !/^[a-f0-9]{64}$/.test(review.fingerprint) || review.policy.downloaderId !== scope.downloaderId || review.policy.infoHash !== scope.taskId.toLowerCase()) throw new ApiError("需新审核及不可降低、旧执行器停止、未知 HR 持续拦截的分别确认", 400);
  const result = await readResponse<SeedingPolicy>(await fetch("/api/v1/downloads/seeding/bind", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ ...scope, fingerprint: review.fingerprint, confirm, confirmOldExecutorsStopped, confirmUnknownHold }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "登记响应不确定；只读刷新原任务，不自动重发", result.code);
}

export type ResumeItem = {
  id: string | number;
  name: string;
  type: string;
  image?: string;
  link?: string;
  percent?: number;
};

export type DashboardData = {
  statistics: {
    movies: string;
    series: string;
    episodes: string;
    music: string;
    users: string;
  };
  storage: {
    total: string;
    used: string;
    free: string;
    usedPercent: number;
  };
  resume: ResumeItem[];
  latest: ResumeItem[];
  warnings: string[];
};

export type SearchResource = {
  id: string;
  season: string;
  name: string;
  description: string;
  site: string;
  pageUrl: string;
  size: string;
  seeders: number;
  resolution: string;
  medium: string;
  effect: string;
  releaseGroup: string;
  videoCodec: string;
  labels: string[];
  uploadFactor: number;
  downloadFactor: number;
  promotionKnown?: boolean;
  seedersKnown?: boolean;
  minimumSeedTime?: number;
  minimumRatio?: number;
  exists?: boolean;
  existsKnown?: boolean;
};

export type DownloadTask = {
  id: string;
  title: string;
  progress: number;
  speed: string;
  state: string;
  siteUrl: string;
  image: string;
  canControl: boolean;
  canRemove: boolean;
  showProgress: boolean;
};

export type DownloadHistory = {
  id: string;
  title: string;
  type: string;
  year: string;
  image: string;
  torrent: string;
  date: string;
  site: string;
};

export type DownloadsData = {
  active: DownloadTask[];
  history: DownloadHistory[];
  historyPage: number;
  warnings: string[];
};

export type SubscriptionItem = {
  id: string;
  name: string;
  year: string;
  type: "MOV" | "TV";
  season: string;
  tmdbId: string;
  image: string;
  overview: string;
  state: string;
  stateLabel: string;
  total: number;
  remaining: number;
  progress: number;
  rssSites: string[];
  searchSites: string[];
  overEdition: boolean;
  quality: string;
  resolution: string;
  releaseGroup: string;
  include: string;
  exclude: string;
  keyword: string;
  fuzzyMatch: boolean;
  filterRule: string;
  savePath: string;
  downloadSetting: string;
  totalEpisodes: number;
  currentEpisode: number;
  pendingSubmission?: boolean;
};

export type SubscriptionOptions = {
  rssSites: { value: string; label: string }[];
  searchSites: { value: string; label: string }[];
  filterRules: { value: string; label: string }[];
  downloadSettings: { value: string; label: string }[];
  savePaths: string[];
  warnings: string[];
};

export type SubscriptionInput = {
  id?: string; name: string; year: string; type: "MOV" | "TV"; season?: string; mediaId?: string;
  keyword?: string; fuzzyMatch: boolean; overEdition: boolean; rssSites: string[]; searchSites: string[];
  quality?: string; resolution?: string; releaseGroup?: string; filterRule?: string; include?: string;
  exclude?: string; savePath?: string; downloadSetting?: string; totalEpisodes?: number; currentEpisode?: number;
};

export type SubscriptionHistory = {
  id: string;
  name: string;
  year: string;
  type: "MOV" | "TV";
  season: string;
  tmdbId: string;
  image: string;
  overview: string;
  finishTime: string;
  total: number;
  start: number;
};

export type SubscriptionsData = {
  items: SubscriptionItem[];
  history: SubscriptionHistory[];
  warnings: string[];
};

export type DiscoveryMedia = {
  id: string;
  title: string;
  year: string;
  type: "MOV" | "TV";
  mediaType: string;
  vote: string;
  image: string;
  backdrop: string;
  overview: string;
  link: string;
  subscribed: boolean;
  cachedPresence?: MediaCachePresence;
};

export type DiscoveryData = {
  category: string;
  page: number;
  items: DiscoveryMedia[];
  cache?: MediaCacheSummary;
};
export type MediaDetailKind = "movie" | "tv";
export type MediaPerson = { id: number; name: string; originalName: string; role: string; job: string; department: string; image: string; link: string };
export type MediaSeason = { number: number; id: number; name: string; overview: string; airDate: string; episodeCount: number | null; poster: string; cachedPresence: MediaCachePresence };
export type MediaDisplayDetail = { id: string; kind: MediaDetailKind; sourceId: string; title: string; originalTitle: string; year: string; overview: string; tagline: string; vote: number; runtime: number | null; poster: string; backdrop: string; genres: string[]; link: string; doubanId: string; doubanLink: string; facts: { label: string; value: string }[]; cast: MediaPerson[]; crew: MediaPerson[]; seasons: MediaSeason[]; subscribed: boolean; subscriptionsKnown: boolean; subscriptions: { id: string; season: string }[]; cache: MediaCacheSummary; cachedPresence: MediaCachePresence; warnings: string[] };
export type MediaSeasonDetail = { id: string; season: number; seasonId: number; name: string; overview: string; poster: string; airDate: string; page: number; total: number; episodes: { id: number; number: number; season: number; name: string; overview: string; airDate: string; productionCode: string; runtime: number | null; vote: number; image: string; cachedPresence: MediaCachePresence }[]; cache: MediaCacheSummary };
function validMediaDetailSelector(kind: MediaDetailKind, id: string): boolean { return ["movie", "tv"].includes(kind) && (/^[1-9]\d{0,18}$/.test(id) || /^DB:[1-9]\d{0,18}$/.test(id) || kind === "tv" && /^BG:[1-9]\d{0,18}$/.test(id)); }
export async function getMediaDetail(token: string, kind: MediaDetailKind, id: string, signal?: AbortSignal): Promise<MediaDisplayDetail> {
  if (!validMediaDetailSelector(kind, id)) throw new ApiError("媒体身份无效", 400);
  const result = await readResponse<MediaDisplayDetail>(await fetch(`/api/v1/media/details/${kind}/${encodeURIComponent(id)}`, { headers: { Authorization: token }, signal }));
  const d = result.data;
  if (!result.success || result.code !== 0 || !d || d.kind !== kind || d.sourceId !== id || /^[1-9]\d*$/.test(id) && d.id !== id || !/^[1-9]\d{0,18}$/.test(d.id) || !d.title || !Array.isArray(d.seasons) || !Array.isArray(d.cast) || !Array.isArray(d.crew) || !Array.isArray(d.subscriptions) || !Array.isArray(d.facts) || !Array.isArray(d.genres) || !Array.isArray(d.warnings) || d.cache?.displayOnly !== true) throw new ApiError(result.message || "媒体详情不可用", result.code);
  return d;
}
export async function getMediaSeason(token: string, id: string, season: number, page = 1, signal?: AbortSignal): Promise<MediaSeasonDetail> {
  if (!/^[1-9]\d{0,18}$/.test(id) || !Number.isInteger(season) || season < 0 || season > 10000 || !Number.isInteger(page) || page < 1 || page > 100) throw new ApiError("季集选择无效", 400);
  const result = await readResponse<MediaSeasonDetail>(await fetch(`/api/v1/media/details/tv/${id}/seasons/${season}?page=${page}`, { headers: { Authorization: token }, signal }));
  const d = result.data;
  if (!result.success || result.code !== 0 || !d || d.id !== id || d.season !== season || d.page !== page || !Array.isArray(d.episodes) || d.episodes.length > 100 || d.episodes.some((ep) => ep.season !== season || !Number.isInteger(ep.number) || ep.number < 0) || d.cache?.displayOnly !== true) throw new ApiError(result.message || "季集详情不可用", result.code);
  return d;
}
export async function getRelatedMedia(token: string, kind: MediaDetailKind, id: string, relation: "similar" | "recommendations", page = 1, signal?: AbortSignal): Promise<DiscoveryData> {
  if (!/^[1-9]\d{0,18}$/.test(id) || !["movie", "tv"].includes(kind) || !["similar", "recommendations"].includes(relation) || !Number.isInteger(page) || page < 1 || page > 100) throw new ApiError("相关媒体选择无效", 400);
  const result = await readResponse<DiscoveryData>(await fetch(`/api/v1/media/details/${kind}/${id}/related/${relation}?page=${page}`, { headers: { Authorization: token }, signal }));
  if (!result.success || result.code !== 0 || !result.data || result.data.category !== relation || result.data.page !== page || !Array.isArray(result.data.items)) throw new ApiError(result.message || "相关媒体不可用", result.code);
  return result.data;
}
export async function getMediaPlayback(token: string, kind: MediaDetailKind, id: string): Promise<string> {
  if (!/^[1-9]\d{0,18}$/.test(id) || !["movie", "tv"].includes(kind)) throw new ApiError("播放身份无效", 400);
  const result = await readResponse<{ id: string; kind: string; link: string; liveVerified: boolean }>(await fetch(`/api/v1/media/details/${kind}/${id}/playback`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data || result.data.id !== id || result.data.kind !== kind || result.data.liveVerified !== true) throw new ApiError(result.message || "当前播放入口不可用", result.code);
  let link: URL; try { link = new URL(result.data.link); } catch { throw new ApiError("播放入口格式无效", 502); }
  if (!["http:", "https:"].includes(link.protocol) || link.username || link.password || link.search) throw new ApiError("播放入口不安全", 502);
  return result.data.link;
}
export async function subscribeMediaDetail(token: string, detail: MediaDisplayDetail, season?: number, confirm = false): Promise<void> {
  if (!confirm || !/^[1-9]\d{0,18}$/.test(detail.id) || !detail.title || !["movie", "tv"].includes(detail.kind) || detail.kind === "tv" && (season === undefined || !Number.isInteger(season) || season < 0 || season > 999 || !detail.seasons.some((item) => item.number === season)) || detail.kind === "movie" && season !== undefined) throw new ApiError("请核对媒体身份及明确确认所选季订阅", 400);
  const body = { name: detail.title, year: detail.year, type: detail.kind === "tv" ? "TV" : "MOV", mediaId: detail.id, ...(season === undefined ? {} : { season: String(season) }), fuzzyMatch: false, overEdition: false, rssSites: [], searchSites: [] };
  const result = await readResponse<unknown>(await fetch("/api/v1/subscriptions", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(body) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "订阅响应未核实，请先查看订阅记录，不自动重试", result.code);
}

export type SiteSummary = {
  id: string;
  name: string;
  priority: number;
  host: string;
  capabilities: string[];
  rssEnabled: boolean;
  brushEnabled: boolean;
  statisticEnabled: boolean;
  parseEnabled: boolean;
  messageEnabled: boolean;
  browserEnabled: boolean;
  proxyEnabled: boolean;
};

export type SitesData = {
  items: SiteSummary[];
};

export type SiteTestResult = {
  id: string;
  ok: boolean;
  duration: number;
  message: string;
};

export type SiteDetail = {
  id: string;
  name: string;
  priority: number;
  siteUrl: string;
  rssEnabled: boolean;
  brushEnabled: boolean;
  statisticEnabled: boolean;
  parseEnabled: boolean;
  messageEnabled: boolean;
  browserEnabled: boolean;
  proxyEnabled: boolean;
  subtitleEnabled: boolean;
  tags: string;
  filterRule: string;
  downloadSetting: string;
  limitInterval: string;
  limitCount: string;
  limitSeconds: string;
  rssConfigured: boolean;
  cookieConfigured: boolean;
  apiKeyConfigured: boolean;
  userAgentConfigured: boolean;
};

export type SiteInput = Omit<SiteDetail, "id" | "rssConfigured" | "cookieConfigured" | "apiKeyConfigured" | "userAgentConfigured"> & {
  rssUrl: string;
  cookie: string;
  apiKey: string;
  userAgent: string;
  clearRssUrl: boolean;
  clearCookie: boolean;
  clearApiKey: boolean;
  clearUserAgent: boolean;
};

export type SiteOptions = {
  filterRules: { value: string; label: string }[];
  downloadSettings: { value: string; label: string }[];
  warnings: string[];
};

export type ManagedService = {
  id: string;
  kind: "downloader" | "media" | "indexer";
  name: string;
  type: string;
  host: string;
  summary: string;
  configured: boolean;
  enabled: boolean;
  active: boolean;
  default: boolean;
  monitoring: boolean;
  canTest: boolean;
  sourceCount: number;
};

export type ServicesData = {
  downloaders: ManagedService[];
  mediaServers: ManagedService[];
  indexers: ManagedService[];
  warnings: string[];
};

export type ServiceTestResult = {
  kind: ManagedService["kind"];
  id: string;
  ok: boolean;
  duration: number;
  message: string;
};

export type DownloaderConfigDetail = {
  id: string;
  name: string;
  type: string;
  host: string;
  port: string;
  proxy: string;
  torrentManagement: string;
  rmtMode: string;
  enabled: boolean;
  transfer: boolean;
  onlyNastool: boolean;
  matchPath: boolean;
  default: boolean;
  usernameConfigured: boolean;
  passwordConfigured: boolean;
  secretConfigured: boolean;
  cookieConfigured: boolean;
  directoriesConfigured: boolean;
  directories: DownloadDirectory[];
};

export type DownloadDirectory = {
  type: "" | "电影" | "电视剧" | "动漫";
  category: string;
  savePath: string;
  containerPath: string;
  label: string;
};

export type DownloaderOptions = {
  categories: Record<string, string[]>;
  warnings: string[];
};

export type DownloaderConfigInput = Omit<DownloaderConfigDetail,
  "id" | "default" | "usernameConfigured" | "passwordConfigured" | "secretConfigured" | "cookieConfigured" | "directoriesConfigured"> & {
  username: string;
  password: string;
  secret: string;
  cookie: string;
  clearUsername: boolean;
  clearPassword: boolean;
  clearSecret: boolean;
  clearCookie: boolean;
};

export type MediaConfigDetail = {
  id: "emby" | "jellyfin" | "plex";
  name: string;
  host: string;
  playHost: string;
  serverName: string;
  active: boolean;
  apiKeyConfigured: boolean;
  tokenConfigured: boolean;
  usernameConfigured: boolean;
  passwordConfigured: boolean;
};

export type MediaConfigInput = Pick<MediaConfigDetail, "host" | "playHost" | "serverName"> & {
  apiKey: string;
  token: string;
  username: string;
  password: string;
  activate: boolean;
  clearApiKey: boolean;
  clearToken: boolean;
  clearUsername: boolean;
  clearPassword: boolean;
};

export type NotificationChannel = {
  id: string;
  name: string;
  type: string;
  typeLabel: string;
  enabled: boolean;
  interactive: boolean;
  canInteract: boolean;
  configured: boolean;
  switches: string[];
  switchLabels: string[];
};

export type NotificationEvent = { id: string; label: string };

export type NotificationsData = {
  items: NotificationChannel[];
  events: NotificationEvent[];
};

export type NotificationTestResult = {
  id: string;
  ok: boolean;
  duration: number;
  message: string;
};

export type NotificationFieldChoice = { value: string; label: string };

export type NotificationFieldOption = {
  key: string;
  title: string;
  type: "text" | "textarea" | "select" | "switch";
  required: boolean;
  tooltip: string;
  placeholder: string;
  default: string | number | boolean | null;
  options: NotificationFieldChoice[];
  writeOnly: boolean;
};

export type NotificationChannelOption = {
  type: string;
  name: string;
  canInteract: boolean;
  fields: NotificationFieldOption[];
};

export type NotificationOptions = {
  channels: NotificationChannelOption[];
  events: NotificationEvent[];
};

export type NotificationDetail = {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  interactive: boolean;
  canInteract: boolean;
  events: string[];
  config: Record<string, string | number | boolean>;
  configuredFields: string[];
};

export type NotificationInput = {
  name: string;
  type: string;
  enabled: boolean;
  interactive: boolean;
  events: string[];
  config: Record<string, string | boolean>;
  clearConfig: string[];
};

export type CustomMessageInput = {
  title: string;
  text: string;
  image: string;
  channelIds: string[];
};

export type CustomMessageResult = { channelCount: number };

export type PluginSummary = {
  id: string;
  name: string;
  description: string;
  version: string;
  author: string;
  authorUrl: string;
  installed: boolean;
  running: boolean;
  configurable: boolean;
	 hasPage: boolean;
  stateKnown?: boolean;
  native?: boolean;
  actionsAvailable?: boolean;
};

export type PluginsData = {
  items: PluginSummary[];
  installedCount: number;
  runningCount: number;
  unknownStateCount?: number;
};

export type PluginConfigChoice = { value: string; label: string };

export type PluginConfigField = {
  key: string;
  title: string;
  section: string;
  type: "text" | "textarea" | "select" | "switch" | "multiselect";
  required: boolean;
  tooltip: string;
  placeholder: string;
  options: PluginConfigChoice[];
  default: string | boolean | null;
  writeOnly: boolean;
  configured: boolean;
  readOnly: boolean;
};

export type PluginConfigDetail = {
  id: string;
  name: string;
  fields: PluginConfigField[];
  values: Record<string, string | boolean | string[]>;
  meta: { hasPage: boolean; fingerprint?: string };
};

export type PluginConfigInput = {
  fingerprint?: string;
  values: Record<string, string | boolean | string[]>;
  clearConfig: string[];
};

export type PluginPageDeleteConfirmation = "confirm" | "typeRecordId";

export type PluginPageRowAction = { type: "delete"; recordId: string; confirmation: PluginPageDeleteConfirmation };

export type PluginPageTable = {
  columns: string[];
  rows: string[][];
  rowActions: Array<PluginPageRowAction | null>;
};

export type PluginPageData = {
  title: string;
  sections: string[];
  tables: PluginPageTable[];
  readOnly: boolean;
  actionsOmitted: boolean;
  canDeleteRecords: boolean;
  deleteConfirmation: PluginPageDeleteConfirmation | "";
};

export type SearchMedia = {
  key: string;
  title: string;
  year: string;
  type: string;
  vote: string;
  tmdbId: string;
  poster: string;
  overview: string;
  exists: boolean;
  existsKnown?: boolean;
  resources: SearchResource[];
};

export type SearchData = {
  keyword: string;
  total: number;
  items: SearchMedia[];
  warnings?: string[];
};

type ApiResponse<T> = {
  code: number;
  success: boolean;
  data?: T;
  message?: string;
};

type LoginData = {
  token: string;
  userinfo: UserInfo;
};

export class ApiError extends Error {
  constructor(message: string, readonly code?: number) {
    super(message);
    this.name = "ApiError";
  }
}

export async function login(username: string, password: string): Promise<AuthSession> {
  const body = new URLSearchParams({ username, password });
  const response = await fetch("/api/v1/user/login", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body,
  });
  const result = await readResponse<LoginData>(response);

  if (!result.success || result.code !== 0 || !result.data?.token) {
    throw new ApiError(result.message || "用户名或密码错误", result.code);
  }

  return { token: result.data.token, user: result.data.userinfo };
}

export async function logout(token: string): Promise<void> {
  const response = await fetch("/api/v1/system/logout", {
    method: "POST",
    headers: { Authorization: token },
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) {
    throw new ApiError(result.message || "退出失败", result.code);
  }
}

export async function getDashboard(token: string): Promise<DashboardData> {
  const response = await fetch("/api/v1/dashboard", {
    headers: { Authorization: token },
  });
  const result = await readResponse<DashboardData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "媒体库数据加载失败", result.code);
  }
  return result.data;
}

export async function searchResources(token: string, keyword: string, quick = false): Promise<SearchData> {
  const response = await fetch("/api/v1/search/resources", {
    method: "POST",
    headers: {
      Authorization: token,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ keyword, quick }),
  });
  const result = await readResponse<SearchData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "资源搜索失败", result.code);
  }
  return result.data;
}

export async function addSearchResource(token: string, resourceId: string): Promise<string> {
  const response = await fetch("/api/v1/downloads/resource", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ resourceId }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "添加下载失败", result.code);
  return result.message || "已添加到下载器";
}

export async function getDownloads(token: string, historyPage = 1): Promise<DownloadsData> {
  const response = await fetch(`/api/v1/downloads?historyPage=${historyPage}`, { headers: { Authorization: token } });
  const result = await readResponse<DownloadsData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "下载任务加载失败", result.code);
  }
  return result.data;
}

export async function controlDownload(token: string, id: string, action: "start" | "stop" | "remove"): Promise<void> {
  const response = await fetch(`/api/v1/downloads/${encodeURIComponent(id)}/${action}`, {
    method: "POST",
    headers: { Authorization: token },
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "下载任务操作失败", result.code);
}

export async function addMagnetDownload(token: string, magnet: string): Promise<void> {
  const response = await fetch("/api/v1/downloads/magnet", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ magnet }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "磁力任务添加失败", result.code);
}

export async function addTorrentDownload(token: string, file: File): Promise<void> {
  const body = new FormData();
  body.append("torrent", file);
  const response = await fetch("/api/v1/downloads/torrent", {
    method: "POST",
    headers: { Authorization: token },
    body,
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "种子文件添加失败", result.code);
}

export async function addSiteTorrentLink(token: string, siteId: string, url: string): Promise<void> {
  const response = await fetch("/api/v1/downloads/link", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ siteId, url }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "站点种子链接添加失败", result.code);
}

export async function getSubscriptions(token: string): Promise<SubscriptionsData> {
  const response = await fetch("/api/v1/subscriptions", { headers: { Authorization: token } });
  const result = await readResponse<SubscriptionsData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "订阅数据加载失败", result.code);
  }
  return result.data;
}

export type OrganizationRoot = { id: string; path: string; label: string; type: string };
export type OrganizationRoots = { sources: OrganizationRoot[]; targets: OrganizationRoot[]; executionModes?: string[]; recordManagement?: boolean };
export type OrganizationMediaAssignment = { source: string; identity: string; modified: string; size: number; kind: "movie" | "tv" | "anime"; tmdbId: string; season?: number; episode?: number; endEpisode?: number; episodeFormat?: string; episodeOffset?: number; partOverride?: string };
export type UnknownDeleteReview = { record: { id: string; path: string | null; dest: string | null; mode: string | null; state: string | null }; fingerprint: string; pendingJobId?: string; eligible: boolean };
export type UnknownDeleteReceipt = { id: string; review: UnknownDeleteReview; actor: string; created: string; restore?: { actor: string; created: string } };

async function unknownRecordRequest<T>(token: string, path: string, body?: object): Promise<T> {
  const result = await readResponse<T>(await fetch(path, { ...(body ? { method: "POST", body: JSON.stringify(body) } : {}), headers: { Authorization: token, ...(body ? { "Content-Type": "application/json" } : {}) } }));
  if (!result.success || result.code !== 0 || result.data === undefined) throw new ApiError(result.message || "记录操作未确认，请刷新回执，不要自动重试", result.code);
  return result.data;
}
export function reviewUnknownDelete(token: string, id: string) { return unknownRecordRequest<UnknownDeleteReview>(token, `/api/v1/organization/unknown/${encodeURIComponent(id)}/delete-review`); }
export function deleteUnknownRecord(token: string, id: string, reviewFingerprint: string, confirmRecordDeletion = false) { return unknownRecordRequest<UnknownDeleteReceipt>(token, `/api/v1/organization/unknown/${encodeURIComponent(id)}/delete`, { confirm: true, confirmRecordDeletion, reviewFingerprint }); }
export function getUnknownDeletions(token: string) { return unknownRecordRequest<UnknownDeleteReceipt[]>(token, "/api/v1/organization/unknown/deletions"); }
export function restoreUnknownRecord(token: string, receipt: string, confirmRecordRestore = false) { return unknownRecordRequest<UnknownDeleteReceipt>(token, `/api/v1/organization/unknown/deletions/${encodeURIComponent(receipt)}/restore`, { confirm: true, confirmRecordRestore }); }
export type OrganizationEpisodeSelection = { tmdbId: string; season: number; episodeFormat: string; startEpisode?: number; endEpisode?: number; episodeOffset?: number; partOverride?: string };
export type OrganizationUnknownItem = { id: number; idText?: string; path: string; dest: string; mode: string; state: string; fingerprint: string; sourceId?: string; targetId?: string; relative?: string; nativeMode?: string; jobId?: string; reason?: string };
export type OrganizationUnknownData = { items: OrganizationUnknownItem[]; total: number; page: number; pageSize: number; totalPages: number };
export type OrganizationHistoryItem = { id: number; idText?: string; mode: string; type: string; title: string; year: string; tmdbId: string; sourcePath: string; sourceFilename: string; dest: string; destPath: string; destFilename: string; date: string; fingerprint: string; sourceId?: string; targetId?: string; relative?: string; nativeMode?: string; reason?: string };
export type OrganizationHistoryData = { items: OrganizationHistoryItem[]; total: number; page: number; totalPages: number };
export type OrganizationUnknownReview = { jobId: string; state: string; original: { id: number; path: string; dest: string; mode: string; state: string }; current: { id: number; path: string; dest: string; mode: string; state: string } | null; fingerprint: string; eligible: boolean };
export type OrganizationDisc = { files: number; directories: number; expanded: boolean; fingerprint: string; latestModified: number };
export type OrganizationRemote = {driver:"rclone";atomic:false;recoveryCopy:true;directories:boolean;evidence:"exclusive-namespace/full-content"};
export type RemoteOrganizationConsent = {confirmRemoteExclusive:boolean;confirmRemoteRecoveryCopy:boolean;confirmOldExecutorsStopped:boolean};
export type OrganizationRemoteTarget={targetId:string;path:string;remote:OrganizationRemote};
export const organizationMovesSource=(mode?:string)=>mode==="move" || mode==="rclone";
export type OrganizationPlanInput = { sourceId: string; targetId: string; path: string; mode: string; allowDiscExpansion?: boolean; assignments?: OrganizationMediaAssignment[]; episodeSelection?: OrganizationEpisodeSelection; unknownRecord?: { id: number; fingerprint: string }; historyRecord?: { id: number; fingerprint: string } };
export type OrganizationEpisodeRule = { episodeFormat?: string; episodeOffset?: number; partOverride?: string };
export type OrganizationPlanPartition={targetId:string;targetRoot:string;items:OrganizationPlan["items"]};
export type OrganizationPlan = { partitions?:OrganizationPlanPartition[];remote?:OrganizationRemote; scraperEnabled?: boolean; previewOnly: boolean; fingerprint: string; mode: string; skipped: number; items: Array<{ disc?: OrganizationDisc; source: string; target?: string; kind: string; size: number; modified: string; identity: string; status: string; reason?: string; tmdbId?: string; title?: string; year?: string; mediaType?: string; category?: string; assigned?: boolean; part?: string } & OrganizationEpisodeRule> };
export type OrganizationLegacySelection = OrganizationEpisodeRule & { kind?: string; tmdbId?: string; season?: number; episodeDetails?: string; minimumMB?: number };
export type OrganizationLegacyRenameOptions = { expandedScope?: boolean; preserveUnknown?: boolean };
export type OrganizationSyncRecord = { id: string; source: string; dest: string; unknown: string; mode: string; compatibility: number; rename: number; enabled: number; locating: number; note: string; nullFields?: string[] };
export type OrganizationSyncOrigin = { record: OrganizationSyncRecord; scope: string; scopeIdentity: string; graphDigest: string; actor: string; monitorGeneration?: string; monitorEvent?: string };
export type SyncConfigPatch = Partial<Pick<OrganizationSyncRecord, "source" | "dest" | "unknown" | "mode" | "note">> & { enabled?: boolean; rename?: boolean; compatibility?: boolean; locating?: boolean };
export type SyncConfigInput = { action: "create" | "update" | "delete" | "status" | "restore"; id?: string; patch?: SyncConfigPatch; restoreId?: string };
export type SyncConfigReview = { action: string; actor: string; before: OrganizationSyncRecord[]; after: OrganizationSyncRecord[]; jobs: Array<{ id: string; configId: string; mode: string }>; fingerprint: string; restoreId?: string };
export type SyncConfigReceipt = { id: string; review: SyncConfigReview; after: OrganizationSyncRecord[]; created: string; restoreId?: string };
export type SyncMonitorInput = { ids: string[]; includeExisting: boolean };
export type DownloadCompletionInput = { downloaderId: string; taskId: string };
export type DownloadMonitorInput = { ids: string[]; includeExisting: boolean; allowOmitted: boolean };
export type DownloadMonitorReview = { remoteTargets?:OrganizationRemoteTarget[];lease: { ids: string[]; actor: string; includeExisting: boolean; allowOmitted: boolean }; sources: Array<{ id: string; name: string; type: string; mode: string; directories: string[] }>; candidates: Record<string, string[]>; needsSourceRemoval: boolean; needsPauseTasks: boolean; needsTaskRemoval: boolean; needsExclusiveLabels: boolean; fingerprint: string };
export type DownloadMonitorConsent = {confirmRemoteExclusive?:boolean;confirmRemoteRecoveryCopy?:boolean; confirmOldExecutorsStopped: boolean; confirmSourceRemoval: boolean; confirmPauseTasks: boolean; confirmResultWrites: boolean; confirmTaskRemoval: boolean; confirmExclusiveLabels: boolean };
export type DownloadMonitorStatus = { control: { state: string; reason?: string; lease: { generation?: string; actor?: string; ids?: string[] } }; events: Array<{ key: string; generation: string; downloaderId: string; taskId: string; state: string; firstSeen?: string; updated: string; batchId?: string; reason?: string }> };
export async function reviewDownloadMonitor(token: string, input: DownloadMonitorInput): Promise<DownloadMonitorReview> {
  const result = await readResponse<DownloadMonitorReview>(await fetch("/api/v1/organization/downloads/monitor/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data?.fingerprint) throw new ApiError(result.message || "下载完成监控范围无法核实", result.code); return result.data;
}
export async function getDownloadMonitor(token: string): Promise<DownloadMonitorStatus> {
  const result = await readResponse<DownloadMonitorStatus>(await fetch("/api/v1/organization/downloads/monitor", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "监控状态无法核实", result.code); return result.data;
}
export async function activateDownloadMonitor(token: string, input: DownloadMonitorInput, fingerprint: string, confirmed: boolean, consent: DownloadMonitorConsent): Promise<void> {
  if((consent.confirmRemoteExclusive!==undefined || consent.confirmRemoteRecoveryCopy!==undefined) && (!consent.confirmRemoteExclusive || !consent.confirmRemoteRecoveryCopy))throw new ApiError("请独立确认本轮远程独占／非原子发布及保留恢复副本费用",400);
  if (!confirmed || !consent.confirmOldExecutorsStopped || !consent.confirmResultWrites || !/^[a-f0-9]{64}$/.test(fingerprint)) throw new ApiError("先审核范围并分别确认自动执行、旧执行器实际停止及结果回写", 400);
  const result = await readResponse<unknown>(await fetch("/api/v1/organization/downloads/monitor/activate", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ ...input, fingerprint, confirm: confirmed, ...consent }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "启用回执未核实，只读刷新，不自动重发", result.code);
}
export async function stopDownloadMonitor(token: string, confirm = false): Promise<void> {
  if (!confirm) throw new ApiError("请求暂停须单独确认，正在运行的步骤仍需核对", 400);
  const result = await readResponse<unknown>(await fetch("/api/v1/organization/downloads/monitor/stop", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "暂停响应未核实，请只读查询状态与活动回执", result.code);
}
export async function dismissDownloadMonitorEvent(token: string, key: string, reason: string, confirm = false): Promise<void> {
  if (!confirm || !reason.trim()) throw new ApiError("须核对关联任务并填写事件忽略原因", 400);
  const result = await readResponse<unknown>(await fetch("/api/v1/organization/downloads/monitor/dismiss", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ key, reason: reason.trim(), confirm }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "事件仍需核对，未自动重试或清理", result.code);
}
export type DownloadOrganizationOrigin = { batchId: string; downloaderId: string; downloaderName: string; downloaderType: string; actor: string; sourceRoot: string; task: DownloadCompletionReview["task"] };
export type DownloadOrganizationReview = { completion: DownloadCompletionReview; plans: OrganizationPlan[]; omitted: DownloadCompletionReview["files"]; fingerprint: string };
export type DownloadOrganizationBatch = { remote?:OrganizationRemote;id: string; origin: DownloadOrganizationOrigin; fingerprint: string; state: string; reason?: string; jobs: string[]; omitted: DownloadCompletionReview["files"]; created: string; resultProof?: string; expected: number; mode: string };
export type DownloadResultReview = { batchId: string; actor: string; operation: string; beforeLabels: string[]; afterLabels: string[]; needsExclusiveLabels: boolean; fingerprint: string };
export type DownloadBatchConsent = {confirmOldExecutorsStopped?:boolean;confirmRemoteExclusive?:boolean;confirmRemoteRecoveryCopy?:boolean; confirmSourceRemoval?: boolean; confirmTaskStopped?: boolean; confirmTaskRemoval?: boolean; confirmExclusiveLabels?: boolean };
export type DownloadReservationReview = { batchId: string; actor: string; reason: string; state: string; jobs: string[]; missing: number[]; plans: OrganizationPlan[]; omitted: DownloadCompletionReview["files"]; fingerprint: string };
export async function reviewDownloadReservation(token: string, id: string, reason: string): Promise<DownloadReservationReview> {
  if (!reason.trim() || reason.length > 1000) throw new ApiError("请说明补齐原预留的原因", 400);
  const result = await readResponse<DownloadReservationReview>(await fetch(`/api/v1/organization/downloads/batches/${encodeURIComponent(id)}/reservation-review`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ reason }) }));
  if (!result.success || result.code !== 0 || result.data?.batchId !== id || !/^[a-f0-9]{64}$/.test(result.data.fingerprint)) throw new ApiError(result.message || "原预留不能安全补齐，请逐任务核对", result.code);
  return result.data;
}
export async function recoverDownloadReservation(token: string, id: string, review: DownloadReservationReview, confirm = false, confirmOldExecutorsStopped = false, confirmOmitted = false): Promise<DownloadOrganizationBatch> {
  if (!confirm || !confirmOldExecutorsStopped || review.batchId !== id || !/^[a-f0-9]{64}$/.test(review.fingerprint) || review.omitted.length > 0 && !confirmOmitted) throw new ApiError("先审核原预留、确认旧执行器停止及本次仅补齐任务", 400);
  const result = await readResponse<DownloadOrganizationBatch>(await fetch(`/api/v1/organization/downloads/batches/${encodeURIComponent(id)}/recover-reservation`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ reason: review.reason, fingerprint: review.fingerprint, confirm, confirmOldExecutorsStopped, confirmOmitted }) }));
  if (!result.success || result.code !== 0 || result.data?.id !== id) throw new ApiError(result.message || "补齐响应未核实；只读查找原批次，不重复补齐或执行", result.code);
  return result.data;
}
export async function previewDownloadOrganization(token: string, input: DownloadCompletionInput): Promise<DownloadOrganizationReview> {
  const result = await readResponse<DownloadOrganizationReview>(await fetch("/api/v1/organization/downloads/plan", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data?.plans?.length || !/^[a-f0-9]{64}$/.test(result.data.fingerprint)) throw new ApiError(result.message || "下载整理范围无法核实", result.code);
  return result.data;
}
export async function saveDownloadOrganization(token: string, input: DownloadCompletionInput, fingerprint: string, confirm = false, confirmOldExecutorsStopped = false, confirmOmitted = false): Promise<DownloadOrganizationBatch> {
  if (!confirm || !confirmOldExecutorsStopped || !/^[a-f0-9]{64}$/.test(fingerprint)) throw new ApiError("先审核计划并确认旧执行器实际停止；保存不执行", 400);
  const result = await readResponse<DownloadOrganizationBatch>(await fetch("/api/v1/organization/downloads/batches", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ ...input, fingerprint, confirm, confirmOldExecutorsStopped, confirmOmitted }) }));
  if (!result.success || result.code !== 0 || !result.data?.id) throw new ApiError(result.message || "保存响应未核实，请只读查询原批次，不重新保存或执行", result.code);
  return result.data;
}
export async function findDownloadOrganization(token: string, input: DownloadCompletionInput): Promise<DownloadOrganizationBatch> {
  const result = await readResponse<DownloadOrganizationBatch>(await fetch(`/api/v1/organization/downloads/batches?${new URLSearchParams(input)}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data?.id) throw new ApiError(result.message || "未找到可核实的已保存批次", result.code);
  return result.data;
}
export async function controlDownloadOrganization(token: string, id: string, action: "execute" | "reconcile" | "reconcile-result", confirm = false, consent: DownloadBatchConsent = {}): Promise<DownloadOrganizationBatch> {
  if(action==="execute" && (consent.confirmRemoteExclusive!==undefined || consent.confirmRemoteRecoveryCopy!==undefined) && (!consent.confirmRemoteExclusive || !consent.confirmRemoteRecoveryCopy || !consent.confirmOldExecutorsStopped))throw new ApiError("请独立确认远程独占／非原子发布、保留副本及停止外部写入器",400);
  if (!confirm) throw new ApiError("此操作需要单独确认", 400);
  const result = await readResponse<DownloadOrganizationBatch>(await fetch(`/api/v1/organization/downloads/batches/${encodeURIComponent(id)}/${action}`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm, ...consent }) }));
  if (!result.success || result.code !== 0 || !result.data?.id) throw new ApiError(result.message || "操作响应未核实；只读刷新批次和任务，不自动重试", result.code);
  return result.data;
}
export async function reviewDownloadOrganizationResult(token: string, id: string): Promise<DownloadResultReview> {
  const result = await readResponse<DownloadResultReview>(await fetch(`/api/v1/organization/downloads/batches/${encodeURIComponent(id)}/result-review`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: "{}" }));
  if (!result.success || result.code !== 0 || !result.data?.fingerprint) throw new ApiError(result.message || "结果回写范围无法核实", result.code);
  return result.data;
}
export async function finalizeDownloadOrganization(token: string, id: string, fingerprint: string, confirm = false, consent: DownloadBatchConsent = {}): Promise<DownloadOrganizationBatch> {
  if (!confirm || !/^[a-f0-9]{64}$/.test(fingerprint)) throw new ApiError("先审核结果回写，并单独确认标签或任务记录操作", 400);
  const result = await readResponse<DownloadOrganizationBatch>(await fetch(`/api/v1/organization/downloads/batches/${encodeURIComponent(id)}/finalize`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm, fingerprint, ...consent }) }));
  if (!result.success || result.code !== 0 || !result.data?.id) throw new ApiError(result.message || "结果回写未核实；只读查询或明确核对，不自动重发", result.code);
  return result.data;
}
export type DownloadCompletionReview = { reviewOnly: true; downloaderId: string; downloaderName: string; downloaderType: string; mode: string; actor: string; task: { id: string; name: string; directory: string; state: string; complete: boolean; labelsKnown: boolean; labels: string[]; files: Array<{ path: string; size: number; selected: boolean; complete: boolean }> }; sourceRoot: string; rootIdentity: string; location: { root: string; directory: string; mapped: boolean }; files: Array<{ path: string; kind: string; size: number; modified: number; identity: string }>; eligible: boolean; reasons: string[]; fingerprint: string };
export async function reviewDownloadCompletion(token: string, input: DownloadCompletionInput): Promise<DownloadCompletionReview> {
  const result = await readResponse<DownloadCompletionReview>(await fetch("/api/v1/organization/downloads/completion/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || result.data?.reviewOnly !== true || !/^[a-f0-9]{64}$/.test(result.data.fingerprint)) throw new ApiError(result.message || "下载完成证据无法核实；未自动整理或标记任务", result.code);
  return result.data;
}
export type SyncMonitorReview = { remoteTargets?:OrganizationRemoteTarget[];lease: { ids: string[]; actor: string; includeExisting: boolean }; records: OrganizationSyncRecord[]; snapshots: Array<{ configId: string; scope: string; files: Array<{ path: string; kind: string; size: number; modified: number; identity: string }> }>; fingerprint: string; needsSourceRemoval: boolean };
export type SyncMonitorStatus = { control: { state: string; reason?: string; lease: { generation?: string; actor?: string; ids?: string[]; includeExisting?: boolean; sourceRemoval?: boolean } }; events: Array<{ key: string; generation: string; snapshot: { configId: string; scope: string }; firstSeen: string; state: string; jobs: string[]; reason?: string; updated: string }> };
export type OrganizationSourceRestoreItem = {index:number;source:string;outcome:string};
export type OrganizationSourceRestoreReview = {jobId:string;state:string;fingerprint:string;canRestore:boolean;reason:string;items:OrganizationSourceRestoreItem[]};
export type OrganizationSourceRestoreReceipt = {jobId:string;actor:string;reason:string;created:string;updated:string;state:string;targetsRetained:boolean;items:OrganizationSourceRestoreItem[]};
export type OrganizationJob = { legacyBatch?:{id:string;index:number};remote?:OrganizationRemote;remoteClosure?:{actor:string;reason:string;created:string}; sourceRestore?:OrganizationSourceRestoreReceipt; id: string; download?: DownloadOrganizationOrigin; sync?: OrganizationSyncOrigin; legacyTargetOverride?: boolean; legacyRename?: OrganizationLegacyRenameOptions; legacySelection?: OrganizationLegacySelection; legacyOrigin?: { action: string; kind: string; recordId: string; actor: string }; fingerprint: string; created: string; state: string; mode: string; episodeSelection?: OrganizationEpisodeSelection; sourceRoot: string; targetRoot: string; historyRecordId?: number; unknownRecordId?: number; unknownState?: string; unknownDetach?: { review: OrganizationUnknownReview; actor: string; reason: string; created: string }; items: Array<{ disc?: OrganizationDisc; index: number; source: string; target: string; kind: string; size: number; modified: string; state: string; reason?: string; tmdbId: string; assigned?: boolean; part?: string } & OrganizationEpisodeRule> };

const sourceRestoreItemsValid=(items:unknown):items is OrganizationSourceRestoreItem[]=>Array.isArray(items) && items.length<=1000 && items.every((i,n)=>!!i && i.index===n && typeof i.source==="string" && !!i.source && ["pending","blocked","not_started","completed_retained","source_in_place","isolated_source","witness_only"].includes(i.outcome));
const sourceRestoreReviewValid=(r:OrganizationSourceRestoreReview):boolean=>!!r && /^[a-f0-9]{32}$/.test(r.jobId) && typeof r.state==="string" && /^[a-f0-9]{64}$/.test(r.fingerprint) && typeof r.canRestore==="boolean" && typeof r.reason==="string" && sourceRestoreItemsValid(r.items) && (!r.canRestore || ["needs_review","source_restoring"].includes(r.state) && r.items.length>0 && !r.items.some(i=>i.outcome==="blocked" || i.outcome==="pending"));
export async function reviewOrganizationSourceRestore(token:string,id:string):Promise<OrganizationSourceRestoreReview>{
  if(!/^[a-f0-9]{32}$/.test(id))throw new ApiError("无效整理任务",400);
  const r=await readResponse<OrganizationSourceRestoreReview>(await fetch(`/api/v1/organization/jobs/${id}/source-restore`,{headers:{Authorization:token}}));
  if(!r.success || r.code!==0 || !r.data || r.data.jobId!==id || !sourceRestoreReviewValid(r.data))throw new ApiError(r.message||"原源恢复审核未核实",r.code||503);return r.data;
}
export async function restoreOrganizationSource(token:string,review:OrganizationSourceRestoreReview,reason:string,confirmRestoreSource=false,confirmOldExecutorsStopped=false):Promise<OrganizationJob>{
  if(!sourceRestoreReviewValid(review) || !review.canRestore || !confirmRestoreSource || !confirmOldExecutorsStopped || !reason.trim() || new TextEncoder().encode(reason).length>512)throw new ApiError("请重新审核原源并独立确认恢复、停止外部写入器及处置原因",400);
  const r=await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${review.jobId}/restore-source`,{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify({confirm:true,confirmRestoreSource,confirmOldExecutorsStopped,reviewFingerprint:review.fingerprint,reason})}));
  const j=r.data,a=j?.sourceRestore;
  if(!r.success || r.code!==0 || !j || j.id!==review.jobId || j.state!=="source_restored" || !["move","rclone"].includes(j.mode) || !a || a.jobId!==j.id || a.state!==j.state || a.reason!==reason || typeof a.actor!=="string" || !a.actor || !a.created || !a.updated || a.targetsRetained!==true || !sourceRestoreItemsValid(a.items) || a.items.length!==review.items.length || a.items.some((i,n)=>i.source!==review.items[n].source || !["source_in_place","not_started","completed_retained"].includes(i.outcome)) || !Array.isArray(j.items) || j.items.length!==review.items.length || j.items.some((i,n)=>i.index!==n || i.source!==review.items[n].source || !["source_restored","source_restore_stopped","completed"].includes(i.state)))throw new ApiError(r.message||"原源恢复回执未核实，请只读刷新，不要重发",r.code||503);
  return j;
}

export async function getOrganizationHistory(token: string, page = 1, keyword = ""): Promise<OrganizationHistoryData> {
  const result = await readResponse<OrganizationHistoryData>(await fetch(`/api/v1/organization/history?${new URLSearchParams({ page: String(page), keyword })}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理历史读取失败", result.code);
  return result.data;
}

export async function reviewOrganizationUnknown(token: string, id: string): Promise<OrganizationUnknownReview> {
  const result = await readResponse<OrganizationUnknownReview>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}/unknown-status`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "关联快照核验失败", result.code);
  return result.data;
}
export async function detachOrganizationUnknown(token: string, id: string, reviewFingerprint: string, reason: string, confirmed = false): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}/detach-unknown`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm: true, confirmDetachUnknown: confirmed, reviewFingerprint, reason }) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "解除关联回执未完成，请重新核对", result.code);
  return result.data;
}

export async function getOrganizationUnknown(token: string, page = 1, keyword = ""): Promise<OrganizationUnknownData> {
  const result = await readResponse<OrganizationUnknownData>(await fetch(`/api/v1/organization/unknown?${new URLSearchParams({ page: String(page), keyword })}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "未识别记录读取失败", result.code);
  return result.data;
}
export async function getOrganizationJob(token: string, id: string): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}`, { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务读取失败", result.code);
  return result.data;
}
export async function getOrganizationRoots(token: string): Promise<OrganizationRoots> {
  const result = await readResponse<OrganizationRoots>(await fetch("/api/v1/organization/roots", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理目录加载失败", result.code);
  return result.data;
}
export async function previewOrganization(token: string, input: OrganizationPlanInput): Promise<OrganizationPlan> {
  const result = await readResponse<OrganizationPlan>(await fetch("/api/v1/organization/plan", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data || result.data.previewOnly !== true || typeof result.data.fingerprint !== "string" || result.data.fingerprint.length !== 64) throw new ApiError(result.message || "整理预览失败", result.code);
  return result.data;
}
export async function getOrganizationJobs(token: string): Promise<OrganizationJob[]> {
  const result = await readResponse<OrganizationJob[]>(await fetch("/api/v1/organization/jobs", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务加载失败", result.code);
  return result.data;
}
export async function createOrganizationJob(token: string, input: OrganizationPlanInput & { fingerprint: string }): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch("/api/v1/organization/jobs", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务创建失败", result.code);
  return result.data;
}
export async function controlOrganizationJob(token: string, id: string, action: "execute" | "reconcile" | "cancel" | "resume-move" | "resume-publication" | "reconcile-unknown", confirmSourceRemoval = false, remote?: RemoteOrganizationConsent): Promise<OrganizationJob> {
  if(remote && (!remote.confirmRemoteExclusive || !remote.confirmRemoteRecoveryCopy || !remote.confirmOldExecutorsStopped))throw new ApiError("请独立确认远程目标独占、非原子发布、恢复副本保留及停止外部写入器",400);
  const result = await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}/${action}`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({confirm:true,...(confirmSourceRemoval?{confirmSourceRemoval:true}:{}),...(remote?{confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true,confirmOldExecutorsStopped:true}:{})}) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务操作失败，请刷新查看已保存状态", result.code);
  return result.data;
}

export async function closeRemoteOrganizationJob(token:string,id:string,reason:string,confirm=false,confirmOldExecutorsStopped=false):Promise<OrganizationJob>{
  if(!/^[a-f0-9]{32}$/.test(id) || !confirm || !confirmOldExecutorsStopped || !reason.trim() || new TextEncoder().encode(reason).length>512)throw new ApiError("请独立确认停止远程任务、停止旧执行器及处置原因",400);
  const r=await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${id}/close-remote`,{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify({confirm:true,confirmOldExecutorsStopped:true,reason})}));
  const job=r.data;
  if(!r.success || r.code!==0 || !job || job.id!==id || job.state!=="remote_closed" || !job.remote || job.remoteClosure?.reason!==reason || !job.remoteClosure.actor || !job.remoteClosure.created || !job.items?.length || job.items.some(i=>!["completed","remote_stopped"].includes(i.state) || i.state==="completed" && !!i.reason))throw new ApiError(r.message||"远程停止回执未核实，请只读刷新，不要重发",r.code||503);
  return job;
}

export async function abandonOrganizationJob(token: string, id: string, confirmDiscardStaging = false, confirmOldExecutorsStopped = false): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}/abandon-unpublished`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm: true, confirmDiscardStaging, confirmOldExecutorsStopped }) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "暂存处置未确认完成，请刷新查看保存状态", result.code);
  return result.data;
}

export type SubscriptionSearchRunResult = {
  submitted: number;
  remaining: number[];
  completed: boolean;
  uncertain: boolean;
};

export async function controlSubscription(token: string, type: "MOV" | "TV", id: string, action: "refresh" | "remove" | "reconcile"): Promise<SubscriptionSearchRunResult | undefined> {
  return subscriptionMutation<SubscriptionSearchRunResult>(token, `/api/v1/subscriptions/${type}/${encodeURIComponent(id)}/${action}`);
}

export async function controlSubscriptionHistory(token: string, type: "MOV" | "TV", id: string, action: "redo" | "remove"): Promise<void> {
  await subscriptionMutation(token, `/api/v1/subscriptions/history/${type}/${encodeURIComponent(id)}/${action}`);
}

async function subscriptionMutation<T = Record<string, never>>(token: string, path: string): Promise<T | undefined> {
  const response = await fetch(path, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<T>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "订阅操作失败", result.code);
  return result.data;
}

export async function getDiscovery(token: string, category: string, page = 1): Promise<DiscoveryData> {
  const response = await fetch("/api/v1/discovery", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ category, page }),
  });
  const result = await readResponse<DiscoveryData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "探索内容加载失败", result.code);
  return result.data;
}

export async function addDefaultSubscription(token: string, media: Pick<DiscoveryMedia, "id" | "title" | "year" | "type">): Promise<void> {
  const response = await fetch("/api/v1/subscriptions", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ name: media.title, year: media.year, type: media.type, mediaId: media.id,
      fuzzyMatch: false, overEdition: false, rssSites: [], searchSites: [] }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "添加订阅失败", result.code);
}

export async function getSubscriptionOptions(token: string): Promise<SubscriptionOptions> {
  const response = await fetch("/api/v1/subscriptions/options", { headers: { Authorization: token } });
  const result = await readResponse<SubscriptionOptions>(response);
  if (!result.success || !result.data) throw new ApiError(result.message || "订阅选项加载失败", result.code);
  return result.data;
}

export async function saveSubscription(token: string, input: SubscriptionInput): Promise<void> {
  const response = await fetch("/api/v1/subscriptions", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "保存订阅失败", result.code);
}

export async function getSites(token: string): Promise<SitesData> {
  const response = await fetch("/api/v1/sites", { headers: { Authorization: token } });
  const result = await readResponse<SitesData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点列表加载失败", result.code);
  }
  return result.data;
}

export async function testSiteConnection(token: string, id: string): Promise<SiteTestResult> {
  const response = await fetch(`/api/v1/sites/${encodeURIComponent(id)}/test`, {
    method: "POST",
    headers: { Authorization: token },
  });
  const result = await readResponse<SiteTestResult>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点连接测试失败", result.code);
  }
  return result.data;
}

export async function getSite(token: string, id: string): Promise<SiteDetail> {
  const response = await fetch(`/api/v1/sites/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<SiteDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点详情加载失败", result.code);
  }
  return result.data;
}

export async function saveSite(token: string, input: SiteInput, id?: string): Promise<void> {
  const response = await fetch(id ? `/api/v1/sites/${encodeURIComponent(id)}` : "/api/v1/sites", {
    method: id ? "PUT" : "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "站点保存失败", result.code);
}

export async function getSiteOptions(token: string): Promise<SiteOptions> {
  const response = await fetch("/api/v1/sites/options", { headers: { Authorization: token } });
  const result = await readResponse<SiteOptions>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点选项加载失败", result.code);
  }
  return result.data;
}

export async function deleteSite(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/sites/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "站点删除失败", result.code);
}

export async function getServices(token: string): Promise<ServicesData> {
  const response = await fetch("/api/v1/services", { headers: { Authorization: token } });
  const result = await readResponse<ServicesData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "服务状态加载失败", result.code);
  }
  return result.data;
}

export async function testManagedService(token: string, service: Pick<ManagedService, "kind" | "id">): Promise<ServiceTestResult> {
  const response = await fetch(`/api/v1/services/${service.kind}/${encodeURIComponent(service.id)}/test`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<ServiceTestResult>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "服务测试失败", result.code);
  }
  return result.data;
}

export async function getDownloaderConfig(token: string, id: string): Promise<DownloaderConfigDetail> {
  const response = await fetch(`/api/v1/services/downloader/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<DownloaderConfigDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "下载器配置加载失败", result.code);
  return result.data;
}

export async function getDownloaderOptions(token: string): Promise<DownloaderOptions> {
  const response = await fetch("/api/v1/services/downloader-options", { headers: { Authorization: token } });
  const result = await readResponse<DownloaderOptions>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "下载目录选项加载失败", result.code);
  return result.data;
}

export async function saveDownloaderConfig(token: string, input: DownloaderConfigInput, id?: string): Promise<void> {
  const response = await fetch(id ? `/api/v1/services/downloader/${encodeURIComponent(id)}` : "/api/v1/services/downloader", {
    method: id ? "PUT" : "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "下载器保存失败", result.code);
}

export async function deleteDownloaderConfig(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/services/downloader/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "下载器删除失败", result.code);
}

export async function setDefaultDownloader(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/services/downloader/${encodeURIComponent(id)}/default`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "默认下载器设置失败", result.code);
}

export async function getMediaConfig(token: string, id: MediaConfigDetail["id"]): Promise<MediaConfigDetail> {
  const response = await fetch(`/api/v1/services/media/${id}`, { headers: { Authorization: token } });
  const result = await readResponse<MediaConfigDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "媒体服务器配置加载失败", result.code);
  return result.data;
}

export async function saveMediaConfig(token: string, id: MediaConfigDetail["id"], input: MediaConfigInput): Promise<void> {
  const response = await fetch(`/api/v1/services/media/${id}`, {
    method: "PUT",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "媒体服务器保存失败", result.code);
}

export async function getNotifications(token: string): Promise<NotificationsData> {
  const response = await fetch("/api/v1/notifications", { headers: { Authorization: token } });
  const result = await readResponse<NotificationsData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知渠道加载失败", result.code);
  return result.data;
}

export async function updateNotificationStatus(token: string, id: string, status: { enabled: boolean } | { interactive: boolean }): Promise<void> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}/status`, {
    method: "PUT",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(status),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "通知状态更新失败", result.code);
}

export async function testNotification(token: string, id: string): Promise<NotificationTestResult> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}/test`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<NotificationTestResult>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知测试失败", result.code);
  return result.data;
}

export async function deleteNotification(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "通知渠道删除失败", result.code);
}

export async function getNotificationOptions(token: string): Promise<NotificationOptions> {
  const response = await fetch("/api/v1/notifications/options", { headers: { Authorization: token } });
  const result = await readResponse<NotificationOptions>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知渠道选项加载失败", result.code);
  return result.data;
}

export async function getNotification(token: string, id: string): Promise<NotificationDetail> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<NotificationDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知渠道详情加载失败", result.code);
  return result.data;
}

export async function saveNotification(token: string, input: NotificationInput, id?: string): Promise<void> {
  const response = await fetch(id ? `/api/v1/notifications/${encodeURIComponent(id)}` : "/api/v1/notifications", {
    method: id ? "PUT" : "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "通知渠道保存失败", result.code);
}

export async function sendCustomMessage(token: string, input: CustomMessageInput): Promise<CustomMessageResult> {
  const response = await fetch("/api/v1/notifications/custom-message", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<CustomMessageResult>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "自定义消息发送失败", result.code);
  return result.data;
}

export async function getPlugins(token: string): Promise<PluginsData> {
  const response = await fetch("/api/v1/plugins", { headers: { Authorization: token } });
  const result = await readResponse<PluginsData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "插件列表加载失败", result.code);
  return result.data;
}

export async function installPlugin(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}/install`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件安装失败", result.code);
}

export async function uninstallPlugin(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件卸载失败", result.code);
}

export async function getPluginConfig(token: string, id: string): Promise<PluginConfigDetail> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<PluginConfigDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "插件配置加载失败", result.code);
  return result.data;
}

export async function savePluginConfig(token: string, id: string, input: PluginConfigInput): Promise<void> {
  if (id === "LibraryScraper" && !/^[a-f0-9]{64}$/.test(input.fingerprint || "")) throw new ApiError("请只读刷新媒体库刮削配置后再明确授权", 400);
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件配置保存失败", result.code);
}

export async function getPluginPage(token: string, id: string): Promise<PluginPageData> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}/page`, { headers: { Authorization: token } });
  const result = await readResponse<PluginPageData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "插件扩展页加载失败", result.code);
  return result.data;
}

export async function deletePluginPageRecord(token: string, id: string, recordId: string, confirmation = ""): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}/page/records`, {
    method: "DELETE",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ recordId, confirmation }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件扩展页记录删除失败", result.code);
}

async function readResponse<T>(response: Response): Promise<ApiResponse<T>> {
  let result: ApiResponse<T>;
  try {
    result = (await response.json()) as ApiResponse<T>;
  } catch {
    if (!response.ok) throw new ApiError(`服务请求失败（HTTP ${response.status}）`, response.status);
    throw new ApiError("服务返回了无法识别的数据");
  }
  if (!response.ok) throw new ApiError(result.message || `服务请求失败（HTTP ${response.status}）`, result.code || response.status);
  return result;
}

export type OrganizationRenameInput = { logid?: string; unknown_id?: string; syncmod: string; type?: "MOV" | "TV" | "ANIME"; tmdb?: string; season?: string; episode_format?: string; episode_details?: string; episode_offset?: string; episode_part?: string; min_filesize?: string; confirmParentScope?: boolean };
export type OrganizationRenameReview = { plan: OrganizationPlan; explicitTarget?: boolean; sourceRoot: string; targetRoot: string; origin: { kind: string; recordId: string; actor: string }; options: OrganizationLegacyRenameOptions; selection: OrganizationLegacySelection; unknown?: { path: string; mode: string; state: string }; history?: { sourcePath: string; sourceFilename: string; mode: string; title: string } };
export type OrganizationRenameOutcome = { batchId?:string;jobs?:string[];jobId: string; state: string; filtered?: number; unknownState?: string };
export class OrganizationRenameError extends ApiError {
  constructor(message: string, code?: number, readonly jobId?: string) { super(message, code); }
}
export async function previewRecordRename(token: string, input: OrganizationRenameInput): Promise<OrganizationRenameReview> {
  const result = await readResponse<OrganizationRenameReview>(await fetch("/api/v1/organization/unknown/rename/plan", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data?.plan?.fingerprint) throw new ApiError(result.message || "关联记录改名预览失败", result.code);
  return result.data;
}

export async function getDirectSyncRecords(token: string): Promise<OrganizationSyncRecord[]> {
  const result = await readResponse<{ items: OrganizationSyncRecord[] }>(await fetch("/api/v1/organization/sync", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "同步配置读取失败", result.code);
  return result.data.items;
}
export async function reviewSyncConfig(token: string, input: SyncConfigInput): Promise<SyncConfigReview> {
  const result = await readResponse<SyncConfigReview>(await fetch("/api/v1/organization/sync/config/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data?.fingerprint) throw new ApiError(result.message || "同步配置审核失败", result.code);
  return result.data;
}
export async function applySyncConfig(token: string, input: SyncConfigInput, fingerprint: string, confirmed = false, confirmInvalidateJobs = false): Promise<SyncConfigReceipt> {
  if (!confirmed || !/^[a-f0-9]{64}$/.test(fingerprint)) throw new ApiError("须先审核并明确确认配置变化", 400);
  const result = await readResponse<SyncConfigReceipt>(await fetch("/api/v1/organization/sync/config/apply", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ ...input, fingerprint, confirm: true, confirmInvalidateJobs }) }));
  if (!result.success || result.code !== 0 || !result.data?.id) throw new ApiError(result.message || "配置保存回执未核实，请只刷新回执，不自动重发", result.code);
  return result.data;
}
export async function getSyncConfigReceipts(token: string): Promise<SyncConfigReceipt[]> {
  const result = await readResponse<SyncConfigReceipt[]>(await fetch("/api/v1/organization/sync/config/receipts", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "配置回执读取失败", result.code);
  return result.data;
}
export async function getSyncMonitor(token: string): Promise<SyncMonitorStatus> {
  const result = await readResponse<SyncMonitorStatus>(await fetch("/api/v1/organization/sync/monitor", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "监控状态读取失败", result.code);
  return result.data;
}
export async function reviewSyncMonitor(token: string, input: SyncMonitorInput): Promise<SyncMonitorReview> {
  const result = await readResponse<SyncMonitorReview>(await fetch("/api/v1/organization/sync/monitor/review", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "监控范围审核失败", result.code);
  return result.data;
}
export async function activateSyncMonitor(token: string, input: SyncMonitorInput, fingerprint: string, confirm = false, confirmOldExecutorsStopped = false, confirmSourceRemoval = false, remote?:RemoteOrganizationConsent): Promise<void> {
  if(remote && (!remote.confirmRemoteExclusive || !remote.confirmRemoteRecoveryCopy || !remote.confirmOldExecutorsStopped))throw new ApiError("远程后台整理须独立确认独占／非原子发布、保留副本费用及实际停止外部写入器",400);
  if (!confirm || !confirmOldExecutorsStopped || !/^[a-f0-9]{64}$/.test(fingerprint)) throw new ApiError("须审核来源范围并确认实际已停止旧执行器", 400);
  const result = await readResponse<unknown>(await fetch("/api/v1/organization/sync/monitor/activate", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ ...input, fingerprint, confirm, confirmOldExecutorsStopped, confirmSourceRemoval,...(remote?{confirmRemoteExclusive:true,confirmRemoteRecoveryCopy:true}:{}) }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "监控启用回执未核实，请只读取状态，不自动重试", result.code);
}
export async function stopSyncMonitor(token: string, confirm = false): Promise<void> {
  if (!confirm) throw new ApiError("明确确认停止请求", 400);
  const result = await readResponse<unknown>(await fetch("/api/v1/organization/sync/monitor/stop", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "停止请求未核实，请读取状态和任务账本", result.code);
}
export async function dismissSyncMonitorEvent(token: string, key: string, reason: string, confirm = false): Promise<void> {
  if (!confirm || !reason.trim()) throw new ApiError("须审核事件并提供忽略原因", 400);
  const result = await readResponse<unknown>(await fetch("/api/v1/organization/sync/monitor/dismiss", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ key, reason: reason.trim(), confirm }) }));
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "事件仍需核对，不自动释放任务", result.code);
}
export async function previewDirectSync(token: string, id: string, path = "."): Promise<OrganizationPlan> {
  const result = await readResponse<OrganizationPlan>(await fetch("/api/v1/organization/sync/plan", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ id, path }) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "直接同步预览失败", result.code);
  return result.data;
}
export async function createDirectSync(token: string, id: string, path: string, fingerprint: string): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch("/api/v1/organization/sync/jobs", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ id, path, fingerprint }) }));
  if (!result.success || result.code !== 0 || !result.data?.id) throw new ApiError(result.message || "同步任务保存未能核实；请只刷新任务列表，不自动重试", result.code);
  return result.data;
}
export async function executeRecordRename(token: string, input: OrganizationRenameInput, reviewFingerprint: string, confirmed = false, confirmSourceRemoval = false,remote?:RemoteOrganizationConsent): Promise<OrganizationRenameOutcome> {
  return executeIdentification(token,input,reviewFingerprint,confirmed,confirmSourceRemoval,"rename",remote);
}
function identificationRemoteConsent(mode:string,remote?:RemoteOrganizationConsent){
  if(!["rclone","rclonecopy","Rclone移动","Rclone复制"].includes(mode))return remote?{confirmOldExecutorsStopped:remote.confirmOldExecutorsStopped}:{};
  if(!remote?.confirmRemoteExclusive || !remote.confirmRemoteRecoveryCopy || !remote.confirmOldExecutorsStopped)throw new ApiError("须独立确认远程独占／非原子发布、保留恢复副本及停止外部写入器",400);
  return remote;
}
export type OrganizationCustomInput=Omit<OrganizationRenameInput,"logid"|"unknown_id"|"confirmParentScope"> & {inpath:string;outpath:string};
export async function previewCustomIdentification(token:string,input:OrganizationCustomInput):Promise<OrganizationRenameReview>{
  const result=await readResponse<OrganizationRenameReview>(await fetch("/api/v1/organization/unknown/renameudf/plan",{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(input)}));
  if(!result.success || result.code!==0 || !result.data?.plan?.fingerprint)throw new ApiError(result.message || "自定义识别预览失败",result.code);
  return result.data;
}
export function executeCustomIdentification(token:string,input:OrganizationCustomInput,fingerprint:string,confirmed=false,sourceRemoval=false,remote?:RemoteOrganizationConsent){return executeIdentification(token,input,fingerprint,confirmed,sourceRemoval,"renameudf",remote);}
async function executeIdentification(token:string,input:OrganizationRenameInput|OrganizationCustomInput,reviewFingerprint:string,confirmed:boolean,confirmSourceRemoval:boolean,action:"rename"|"renameudf",remote?:RemoteOrganizationConsent):Promise<OrganizationRenameOutcome>{
  if (!confirmed || !/^[a-f0-9]{64}$/.test(reviewFingerprint) || ["move","rclone","移动","Rclone移动"].includes(input.syncmod) && !confirmSourceRemoval) throw new ApiError("须审核预览并明确确认；移动另需源移除授权", 400);
  const consent=identificationRemoteConsent(input.syncmod,remote);
  const response = await fetch(`/api/v1/organization/unknown/${action}`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ ...input, reviewFingerprint, confirmSourceRemoval,...consent }) });
  let result: ApiResponse<OrganizationRenameOutcome>;
  try { result = await response.json() as ApiResponse<OrganizationRenameOutcome>; } catch { throw new OrganizationRenameError("响应无法核实；请只刷新任务列表，不能自动重试", response.status); }
  if (!response.ok || !result.success || result.code !== 0 || result.data?.state !== "completed" || !result.data.jobId) throw new OrganizationRenameError(result.message || "改名未完成；请核对已保存任务，不要重跑", result.code || response.status, result.data?.jobId);
  return result.data;
}

export type OrganizationRedoInput={flag:"unidentification"|"history";ids:string[]};
export type OrganizationRedoReview={flag:OrganizationRedoInput["flag"];fingerprint:string;items:Array<{id:string;plan?:OrganizationPlan;sourceRoot?:string;targetRoot?:string;code:number;message?:string}>};
export type OrganizationRedoResult={id:string;batchId?:string;jobs?:string[];jobId?:string;state:string;code:number;message?:string};
function validateRedoIDs(input:OrganizationRedoInput){
  if(!["unidentification","history"].includes(input.flag) || !input.ids.length || input.ids.length>100 || new Set(input.ids).size!==input.ids.length || input.ids.some(id=>!(/^[1-9][0-9]{0,18}$/.test(id)) || BigInt(id)>9223372036854775807n))throw new ApiError("选择 1–100 个不重复、精确的正整数记录 ID",400);
}
export async function previewRecordRedo(token:string,input:OrganizationRedoInput):Promise<OrganizationRedoReview>{
  validateRedoIDs(input);
  const result=await readResponse<OrganizationRedoReview>(await fetch("/api/v1/organization/unknown/redo/plan",{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify(input)}));
  if(!result.success || result.code!==0 || !result.data?.fingerprint || result.data.flag!==input.flag || JSON.stringify(result.data.items.map(item=>item.id))!==JSON.stringify(input.ids))throw new ApiError(result.message || "重新识别范围审核失败",result.code);
  return result.data;
}
export async function executeRecordRedo(token:string,input:OrganizationRedoInput,review:OrganizationRedoReview,confirmed=false,sourceRemoval=false,remote?:RemoteOrganizationConsent):Promise<{success:boolean;message?:string;results:OrganizationRedoResult[]}>{
  validateRedoIDs(input);
  if(!confirmed || !/^[a-f0-9]{64}$/.test(review.fingerprint) || review.flag!==input.flag || JSON.stringify(review.items.map(item=>item.id))!==JSON.stringify(input.ids) || review.items.some(item=>organizationMovesSource(item.plan?.mode)) && !sourceRemoval)throw new ApiError("先审核同一批记录，移动需独立源移除授权",400);
  const cloud=review.items.find(item=>item.plan?.remote);
  const multi=review.items.some(item=>(item.plan?.partitions?.length || 0)>1);
  if(multi&&!remote?.confirmOldExecutorsStopped)throw new ApiError("多目标批次须独立确认已停止旧执行器和外部写入器",400);
  const consent=cloud?identificationRemoteConsent(cloud.plan!.mode,remote):multi?{confirmOldExecutorsStopped:true}:{};
  const response=await fetch("/api/v1/organization/unknown/redo",{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify({...input,reviewFingerprint:review.fingerprint,confirmSourceRemoval:sourceRemoval,...consent})});
  let result:ApiResponse<{results:OrganizationRedoResult[]}>;
  try{result=await response.json();}catch{throw new ApiError("响应无法核实；只读刷新已保存任务，不自动重发",response.status);}
  if(!response.ok || !result.data?.results || ![0,2].includes(result.code))throw new ApiError(result.message || "重新识别回执未核实，不重跑",result.code || response.status);
  return {success:result.success,message:result.message,results:result.data.results};
}

export type OrganizationLegacyBatch={id:string;fingerprint:string;reviewFingerprint:string;state:string;reason?:string;actor:string;created:string;jobs:string[];mode:string;origin:{action:string;kind:string;recordId?:string;actor:string};unknownState?:string;sourceRoot:string;scope:string;targets:string[]};
export type LegacyBatchConsent={confirm:boolean;confirmSourceRemoval?:boolean;confirmOldExecutorsStopped?:boolean;confirmRemoteExclusive?:boolean;confirmRemoteRecoveryCopy?:boolean;reason?:string};
export async function getLegacyOrganizationBatch(token:string,id:string):Promise<OrganizationLegacyBatch>{
  if(!/^[a-f0-9]{32}$/.test(id))throw new ApiError("请输入完整批次 ID",400);
  const result=await readResponse<OrganizationLegacyBatch>(await fetch(`/api/v1/organization/legacy/batches/${id}`,{headers:{Authorization:token}}));
  if(!result.success||result.code!==0||result.data?.id!==id||!/^[a-f0-9]{64}$/.test(result.data.reviewFingerprint)||!Array.isArray(result.data.jobs))throw new ApiError(result.message || "批次回执无法核实",result.code);
  return result.data;
}
export async function controlLegacyOrganizationBatch(token:string,batch:OrganizationLegacyBatch,action:"execute"|"reconcile"|"stop"|"resume"|"close",consent:LegacyBatchConsent):Promise<OrganizationLegacyBatch>{
  if(!consent.confirm||!/^[a-f0-9]{32}$/.test(batch.id)||!/^[a-f0-9]{64}$/.test(batch.reviewFingerprint))throw new ApiError("先读取完整批次并明确确认",400);
  if(["execute","resume","close"].includes(action)&&!consent.confirmOldExecutorsStopped)throw new ApiError("须确认已停止旧执行器与外部写入器",400);
  if(["stop","resume","close"].includes(action)&&(!consent.reason?.trim()||consent.reason.length>512))throw new ApiError("请填写独立处置原因",400);
  if(action==="execute"&&organizationMovesSource(batch.mode)&&!consent.confirmSourceRemoval)throw new ApiError("移动需独立源移除授权",400);
  if(["execute","resume"].includes(action)&&["rclone","rclonecopy"].includes(batch.mode)&&(!consent.confirmRemoteExclusive||!consent.confirmRemoteRecoveryCopy))throw new ApiError("远程需独立非原子／独占与恢复副本授权",400);
  const result=await readResponse<OrganizationLegacyBatch>(await fetch(`/api/v1/organization/legacy/batches/${batch.id}/${action}`,{method:"POST",headers:{Authorization:token,"Content-Type":"application/json"},body:JSON.stringify({...consent,reviewFingerprint:batch.reviewFingerprint})}));
  if(!result.success||result.code!==0||result.data?.id!==batch.id)throw new ApiError(result.message || "批次操作回执未核实，只读刷新、不自动重发",result.code);
  return result.data;
}
