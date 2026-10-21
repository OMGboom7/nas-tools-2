import { useEffect, useRef, useState, type FormEvent } from "react";
import { closeRemoteOrganizationJob, reviewOrganizationSourceRestore, restoreOrganizationSource, type OrganizationSourceRestoreReview } from "../api/client";
import { ApiError, getOrganizationRoots, previewOrganization, getOrganizationJobs, getOrganizationJob, getOrganizationUnknown, getOrganizationHistory, reviewOrganizationUnknown, detachOrganizationUnknown, createOrganizationJob, controlOrganizationJob, abandonOrganizationJob, type AuthSession, type OrganizationJob, type OrganizationPlan, type OrganizationRoots, type OrganizationMediaAssignment, type OrganizationUnknownItem, type OrganizationUnknownData, type OrganizationUnknownReview, type OrganizationHistoryItem, type OrganizationHistoryData } from "../api/client";
import { WorkspaceLayout } from "../components/WorkspaceLayout";
import { UnknownRecordManager } from "./UnknownRecordManager";
import { RecordRenameForm } from "./RecordRenameForm";
import { RecordRedoForm } from "./RecordRedoForm";
import { DirectSyncForm } from "./DirectSyncForm";
import { SyncConfigManager } from "./SyncConfigManager";
import { SyncMonitorManager } from "./SyncMonitorManager";
import { DownloadCompletionReviewForm } from "./DownloadCompletionReviewForm";
import { DownloadOrganizationManager } from "./DownloadOrganizationManager";
import { DownloadMonitorManager } from "./DownloadMonitorManager";
import { IntakeScraperManager } from "./IntakeScraperManager";
import { LibraryScraperManager } from "./LibraryScraperManager";
import { canAbandonOrganizationJob, buildOrganizationAssignment, buildOrganizationEpisodeSelection } from "./organizationState";

type Props = { session: AuthSession; currentPath: string; onNavigate: (path: string) => void; onLogout: () => Promise<void>; onSessionExpired: () => void };
const statusLabels: Record<string, string> = { available: "可规划", conflict: "目标冲突", blocked: "已阻止", unmatched: "附件未匹配", unrecognized: "媒体未识别", filtered: "明确筛除，不执行" };
const jobLabels: Record<string, string> = { ready: "待执行", completed: "已完成", needs_review: "需核对，禁止盲目重试", cancelled: "已取消", planned: "等待执行", running: "执行中或已中断", prepared: "转移凭证已保存，待核实", moving: "源移除意图已保存，需核验恢复对象", quarantined: "源文件已转入恢复目录，待提交" };
const modeLabels: Record<string,string> = {copy:"复制",link:"硬链接",softlink:"软链接",move:"移动"};
modeLabels.rclonecopy="Rclone 远程复制";modeLabels.rclone="Rclone 远程移动";
const movingMode=(mode:string)=>mode==="move" || mode==="rclone";
jobLabels.publishing="远程发布意图已保存，须明确核验或续办";
jobLabels.remote_closed="远程任务已明确停止；对象和预留全部保留";
jobLabels.remote_stopped="项目已停止，不记整理成功";
jobLabels.abandoning = "放弃意图已保存，暂存清理待核实";
jobLabels.discarding = "暂存清理待回执";
jobLabels.abandoned = "已明确放弃，未记录整理成功";
jobLabels.staging = "写入前凭证已保存，尚未完整准备";
jobLabels.source_restoring = "原源恢复意图已保存，须明确续办";
jobLabels.source_restored = "原源恢复回执已保存，不再执行此任务";
jobLabels.source_restore_pending = "原源恢复／停止待回执";
jobLabels.source_restore_stopped = "未完成项目已停止，所有目标与证据保留";
const sourceOutcomes:Record<string,string>={pending:"待明确续办",blocked:"证据不足，不允许恢复",not_started:"未执行项停止（不声称已核验文件）",completed_retained:"已有成功项目及历史保持原状",source_in_place:"原源正向核验在位",isolated_source:"原源完整保存在隔离目录",witness_only:"仅剩已核验的原节点见证，可防覆盖恢复"};
const jobReasonLabels: Record<string, string> = {
  "Move source recovery cleanup pending; explicit source-removal confirmation required": "历史已提交，源恢复备份清理尚未确认完成；请检查后明确授权续办。",
  "Move target recovery cleanup pending": "源恢复备份已清理，目标暂存清理尚未确认完成；请核验续办。",
  "Execution interrupted or failed; verify the saved proof before any retry": "操作已中断或失败；必须核验保存凭证，不能盲目重试。",
  "Explicit unpublished staging disposal requested; no successful transfer recorded": "管理员已明确放弃未发布暂存；不是整理成功记录。",
  "Source restoration intent saved; no further transfer or source cleanup permitted":"原源恢复意图已保存，后续整理及源清理均停止。",
  "Source restoration closed this unfinished transfer; target, staging and recovery evidence retained":"此未完成项目已关闭；目标、暂存及恢复证据保留，不记成功历史。",
};

export function OrganizationPage({ session, currentPath, onNavigate, onLogout, onSessionExpired }: Props) {
  const [roots, setRoots] = useState<OrganizationRoots>({ sources: [], targets: [] });
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [path, setPath] = useState(".");
  const [mode, setMode] = useState("copy");
  const [allowDiscExpansion,setAllowDiscExpansion]=useState(false);
  useEffect(()=>{setAllowDiscExpansion(false);},[sourceId,targetId,path,mode,session.token]);
  const [syncConfigGeneration, setSyncConfigGeneration] = useState(0);
  const [plan, setPlan] = useState<OrganizationPlan | null>(null);
  const [assignments, setAssignments] = useState<OrganizationMediaAssignment[]>([]);
  const [unknown, setUnknown] = useState<OrganizationUnknownData>({ items: [], total: 0, page: 1, pageSize: 30, totalPages: 1 });
  const [unknownKeyword, setUnknownKeyword] = useState("");
  const [selectedUnknown, setSelectedUnknown] = useState<OrganizationUnknownItem | null>(null);
  const [history, setHistory] = useState<OrganizationHistoryData>({ items: [], total: 0, page: 1, totalPages: 1 });
  const [historyKeyword, setHistoryKeyword] = useState("");
  const [selectedHistory, setSelectedHistory] = useState<OrganizationHistoryItem | null>(null);
  const [detachReview, setDetachReview] = useState<OrganizationUnknownReview | null>(null);
  const [detachReason, setDetachReason] = useState("");
  const [detachConfirmed, setDetachConfirmed] = useState(false);
  const [manualSource, setManualSource] = useState("");
  const [manualKind, setManualKind] = useState<"movie" | "tv" | "anime">("movie");
  const [manualId, setManualId] = useState("");
  const [manualSeason, setManualSeason] = useState("");
  const [manualEpisode, setManualEpisode] = useState("");
  const [manualEndEpisode, setManualEndEpisode] = useState("");
  const [manualEpisodeFormat, setManualEpisodeFormat] = useState("");
  const [manualEpisodeOffset, setManualEpisodeOffset] = useState("");
  const [manualPartMode, setManualPartMode] = useState<"auto" | "set" | "clear">("auto");
  const [manualPart, setManualPart] = useState("");
  const [batchEnabled, setBatchEnabled] = useState(false);
  const [batchId, setBatchId] = useState("");
  const [batchSeason, setBatchSeason] = useState("");
  const [batchFormat, setBatchFormat] = useState("");
  const [batchStart, setBatchStart] = useState("");
  const [batchEnd, setBatchEnd] = useState("");
  const [batchOffset, setBatchOffset] = useState("");
  useEffect(() => { setBatchEnabled(false); setBatchId(""); setBatchSeason(""); setBatchFormat(""); setBatchStart(""); setBatchEnd(""); setBatchOffset(""); }, [session.token]);
  useEffect(() => { setManualPartMode("auto"); setManualPart(""); }, [manualSource, sourceId, targetId, path, mode, session.token]);
  useEffect(() => { setManualEpisodeFormat(""); setManualEpisodeOffset(""); }, [sourceId, targetId, path, mode, session.token]);
  const previewGeneration = useRef(0);
  const unknownGeneration = useRef(0);
  const detachGeneration = useRef(0);
  useEffect(() => () => { previewGeneration.current++; unknownGeneration.current++; detachGeneration.current++; }, []);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [jobs, setJobs] = useState<OrganizationJob[]>([]);
  const [jobId, setJobId] = useState("");
  const [confirmed, setConfirmed] = useState(false);
  const [sourceRemoval, setSourceRemoval] = useState(false);
  const [discardStaging, setDiscardStaging] = useState(false);
  const [oldExecutorsStopped, setOldExecutorsStopped] = useState(false);
  const [remoteExclusive,setRemoteExclusive]=useState(false),[remoteRecovery,setRemoteRecovery]=useState(false),[remoteStopped,setRemoteStopped]=useState(false);
  const [closeReason,setCloseReason]=useState(""),[closeConfirmed,setCloseConfirmed]=useState(false),[closeStopped,setCloseStopped]=useState(false);
  const [sourceReview,setSourceReview]=useState<OrganizationSourceRestoreReview|null>(null);
  const [restoreReason,setRestoreReason]=useState("");
  const [restoreConfirmed,setRestoreConfirmed]=useState(false),[restoreStopped,setRestoreStopped]=useState(false);
  const sourceRestoreGeneration=useRef(0),sourceRestoreInFlight=useRef(false);
  const job = jobs.find((item) => item.id === jobId);
  const remoteJob=!!job && ["rclonecopy","rclone"].includes(job.mode);
  const remoteApproved=!remoteJob || remoteExclusive && remoteRecovery && remoteStopped;
  useEffect(()=>{setRemoteExclusive(false);setRemoteRecovery(false);setRemoteStopped(false);setCloseConfirmed(false);setCloseStopped(false);setCloseReason("");},[jobId,job?.state,session.token]);
  useEffect(()=>{sourceRestoreGeneration.current++;sourceRestoreInFlight.current=false;setSourceReview(null);setRestoreConfirmed(false);setRestoreStopped(false);setRestoreReason(job?.sourceRestore?.reason||"");return()=>{sourceRestoreGeneration.current++;};},[jobId,session.token,job?.sourceRestore?.created]);
  const manualDisc = plan?.items.find((item)=>item.source===manualSource)?.disc;
  const selectedDest = selectedUnknown?.dest || selectedHistory?.dest;
  useEffect(() => { setConfirmed(false); setSourceRemoval(false); }, [jobId, session.token]);
  useEffect(() => { detachGeneration.current++; setDetachReview(null); setDetachReason(""); setDetachConfirmed(false); }, [jobId, session.token]);
  useEffect(() => { setDiscardStaging(false); setOldExecutorsStopped(false); }, [jobId, session.token]);
  useEffect(() => { previewGeneration.current++; unknownGeneration.current++; setUnknown({ items: [], total: 0, page: 1, pageSize: 30, totalPages: 1 }); setHistory({ items: [], total: 0, page: 1, totalPages: 1 }); setAssignments([]); setSelectedUnknown(null); setSelectedHistory(null); setPlan(null); setManualSource(""); setManualId(""); setBusy(false); }, [session.token]);
  useEffect(() => {
    let active = true;
    setLoading(true);
    void getOrganizationRoots(session.token).then(async (data) => {
      if (!active) return;
      setRoots(data); setSourceId(data.sources[0]?.id || ""); setTargetId(data.targets[0]?.id || ""); setError("");
      const records = await getOrganizationUnknown(session.token);
      if (active) setUnknown(records);
      const logs = await getOrganizationHistory(session.token);
      if (active) setHistory(logs);
      if (data.executionModes?.length) {
        const saved = await getOrganizationJobs(session.token);
        if (active) { setJobs(saved); setJobId(saved[0]?.id || ""); }
      }
    }).catch((err: unknown) => {
      if (!active) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "整理目录加载失败");
    }).finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [session.token, onSessionExpired]);

  async function preview(event: FormEvent) {
    event.preventDefault(); await generatePreview(assignments);
  }
  function batchInput() { return batchEnabled ? { episodeSelection: buildOrganizationEpisodeSelection(batchId, batchSeason, batchFormat, batchStart, batchEnd, batchOffset) } : {}; }
  async function generatePreview(selected: OrganizationMediaAssignment[]) {
    const generation = ++previewGeneration.current;
    setBusy(true); setError(""); setPlan(null); setManualSource("");
    try {
      const result = await previewOrganization(session.token, { sourceId, targetId, path, mode, allowDiscExpansion, ...batchInput(), ...(selected.length ? { assignments: selected } : {}), ...(selectedUnknown ? { unknownRecord: { id: selectedUnknown.id, fingerprint: selectedUnknown.fingerprint } } : {}), ...(selectedHistory ? { historyRecord: { id: selectedHistory.id, fingerprint: selectedHistory.fingerprint } } : {}) });
      if (generation === previewGeneration.current) setPlan(result);
    }
    catch (err) {
      if (generation !== previewGeneration.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "整理预览失败");
    } finally { if (generation === previewGeneration.current) setBusy(false); }
  }
  function invalidate() { previewGeneration.current++; setPlan(null); setAssignments([]); setManualSource(""); setManualId(""); setError(""); }
  async function refreshSavedJobs(id: string) {
    invalidate(); setSelectedUnknown(null); setSelectedHistory(null);
    const generation = previewGeneration.current;
    const saved = await getOrganizationJobs(session.token);
    if (generation !== previewGeneration.current) return;
    setJobs(saved); setJobId(id || saved[0]?.id || "");
    if (id && !saved.some((item) => item.id === id)) {
      const current = await getOrganizationJob(session.token, id);
      if (generation !== previewGeneration.current) return;
      setJobs([current, ...saved]);
    }
    await loadUnknown(1);
  }
  function selectUnknown(item: OrganizationUnknownItem) {
    if (item.reason || !Number.isSafeInteger(item.id) || !item.sourceId || !item.targetId || item.relative === undefined || item.nativeMode && !roots.executionModes?.includes(item.nativeMode)) return;
    invalidate(); setSelectedUnknown(item); setSelectedHistory(null); setSourceId(item.sourceId); setTargetId(item.targetId); setPath(item.relative);
    if (item.nativeMode) setMode(item.nativeMode);
  }
  function selectHistory(item: OrganizationHistoryItem) {
    if (item.reason || !Number.isSafeInteger(item.id) || !item.sourceId || !item.targetId || item.relative === undefined || item.nativeMode && !roots.executionModes?.includes(item.nativeMode)) return;
    invalidate(); setSelectedHistory(item); setSelectedUnknown(null); setSourceId(item.sourceId); setTargetId(item.targetId); setPath(item.relative);
    if (item.nativeMode) setMode(item.nativeMode);
  }
  async function loadHistory(page: number) {
    const generation = ++unknownGeneration.current;
    setBusy(true); setError("");
    try { const data = await getOrganizationHistory(session.token, page, historyKeyword); if (generation === unknownGeneration.current) setHistory(data); }
    catch (err) { if (generation !== unknownGeneration.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "历史读取失败"); }
    finally { if (generation === unknownGeneration.current) setBusy(false); }
  }
  async function loadUnknown(page: number, job?: string) {
    const generation = ++unknownGeneration.current;
    setBusy(true); setError("");
    try {
      if (job) { const saved = await getOrganizationJob(session.token, job); if (generation === unknownGeneration.current) { setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]); setJobId(saved.id); } }
      else { const saved = await getOrganizationUnknown(session.token, page, unknownKeyword); if (generation === unknownGeneration.current) setUnknown(saved); }
    } catch (err) { if (generation !== unknownGeneration.current) return; if (err instanceof ApiError && err.code === 401) onSessionExpired(); else setError(err instanceof Error ? err.message : "未识别记录读取失败"); }
    finally { if (generation === unknownGeneration.current) setBusy(false); }
  }
  async function assignIdentity(event: FormEvent) {
    event.preventDefault();
    const file = plan?.items.find((item) => item.source === manualSource && item.kind === "media");
    if (!file) { setError("请从当前预览选择要纠正身份的媒体文件。"); return; }
    try {
      const selection = buildOrganizationAssignment(file, manualKind, manualId, manualSeason, manualEpisode, manualEndEpisode, manualEpisodeFormat, manualEpisodeOffset, manualPartMode, manualPart);
      const updated = [...assignments.filter((item) => item.source !== selection.source), selection];
      setAssignments(updated); await generatePreview(updated);
    } catch (err) { setError(err instanceof Error ? err.message : "人工身份选择无效"); }
  }
  async function unknownDetachOperation(apply: boolean) {
    if (apply && (!detachConfirmed || !detachReview?.eligible || !detachReason.trim() || detachReview.jobId !== jobId)) return;
    const generation = ++detachGeneration.current;
    setBusy(true); setError(""); setDetachConfirmed(false);
    try {
      if (!apply) {
        const review = await reviewOrganizationUnknown(session.token, jobId);
        if (generation === detachGeneration.current) setDetachReview(review);
      } else {
        const saved = await detachOrganizationUnknown(session.token, jobId, detachReview!.fingerprint, detachReason.trim(), true);
        if (generation !== detachGeneration.current) return;
        setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]); setDetachReview(null); setDetachReason("");
        const records = await getOrganizationUnknown(session.token, unknown.page, unknownKeyword);
        if (generation === detachGeneration.current) setUnknown(records);
      }
    } catch (err) {
      if (generation !== detachGeneration.current) return;
      setDetachReview(null);
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else { setError(err instanceof Error ? err.message : "关联处置失败"); try { const saved = await getOrganizationJobs(session.token); if (generation === detachGeneration.current) setJobs(saved); } catch { /* never retry a mutation */ } }
    } finally { if (generation === detachGeneration.current) setBusy(false); }
  }
  async function sourceRestoreOperation(apply:boolean){
    if(busy || sourceRestoreInFlight.current || !job || apply && (!sourceReview?.canRestore || sourceReview.jobId!==jobId || !restoreConfirmed || !restoreStopped || !restoreReason.trim()))return;
    setRemoteExclusive(false);setRemoteRecovery(false);setRemoteStopped(false);setCloseConfirmed(false);setCloseStopped(false);setConfirmed(false);setSourceRemoval(false);
    const version=++sourceRestoreGeneration.current;sourceRestoreInFlight.current=true;setBusy(true);setError("");setRestoreConfirmed(false);setRestoreStopped(false);
    try{
      if(!apply){setSourceReview(null);const review=await reviewOrganizationSourceRestore(session.token,jobId);if(version===sourceRestoreGeneration.current)setSourceReview(review);}
      else{const saved=await restoreOrganizationSource(session.token,sourceReview!,restoreReason,true,true);if(version!==sourceRestoreGeneration.current)return;setJobs(current=>[saved,...current.filter(j=>j.id!==saved.id)]);setSourceReview(null);setConfirmed(false);setSourceRemoval(false);}
    }catch(err){if(version!==sourceRestoreGeneration.current)return;setSourceReview(null);if(err instanceof ApiError && err.code===401)onSessionExpired();else setError((err instanceof Error?err.message:"原源恢复未核实")+(apply?"。请只读刷新任务和审核，不要重发。":""));}
    finally{if(version===sourceRestoreGeneration.current){sourceRestoreInFlight.current=false;setBusy(false);}}
  }
  async function jobOperation(operation: "create" | "refresh" | "execute" | "reconcile" | "cancel" | "resume-move" | "resume-publication" | "abandon-unpublished" | "reconcile-unknown") {
    if(["execute","reconcile","resume-move","resume-publication"].includes(operation) && !remoteApproved)return;
    if (operation === "reconcile-unknown" && !confirmed) return;
    if (operation === "abandon-unpublished" && (!discardStaging || !oldExecutorsStopped || !job?.sourceRoot || !job.targetRoot)) return;
    if (operation === "resume-publication" && (!confirmed || !job?.sourceRoot || !job.targetRoot)) return;
    if ((operation === "execute" || operation === "resume-move" || operation === "resume-publication") && !!job && movingMode(job.mode) && (!confirmed || !sourceRemoval || !job.sourceRoot || !job.targetRoot)) return;
    setSourceReview(null);setRestoreConfirmed(false);setRestoreStopped(false);
    setCloseConfirmed(false);setCloseStopped(false);
    setBusy(true); setError("");
    try {
      if (operation === "refresh") { setJobs(await getOrganizationJobs(session.token)); return; }
      const saved = operation === "create"
        ? await createOrganizationJob(session.token, { sourceId, targetId, path, mode, allowDiscExpansion, ...batchInput(), fingerprint: plan!.fingerprint, ...(assignments.length ? { assignments } : {}), ...(selectedUnknown ? { unknownRecord: { id: selectedUnknown.id, fingerprint: selectedUnknown.fingerprint } } : {}), ...(selectedHistory ? { historyRecord: { id: selectedHistory.id, fingerprint: selectedHistory.fingerprint } } : {}) })
        : operation === "abandon-unpublished" ? await abandonOrganizationJob(session.token, jobId, discardStaging, oldExecutorsStopped)
        : await controlOrganizationJob(session.token, jobId, operation, !!job && movingMode(job.mode) && (operation === "execute" || operation === "resume-move" || operation === "resume-publication") && sourceRemoval, remoteJob && ["execute","reconcile","resume-move","resume-publication"].includes(operation)?{confirmRemoteExclusive:remoteExclusive,confirmRemoteRecoveryCopy:remoteRecovery,confirmOldExecutorsStopped:remoteStopped}:undefined);
      setJobs((current) => [saved, ...current.filter((item) => item.id !== saved.id)]);
      setJobId(saved.id); setPlan(null); setAssignments([]); setSelectedUnknown(null); setSelectedHistory(null); setManualSource("");
      setUnknown(await getOrganizationUnknown(session.token, unknown.page, unknownKeyword));
    } catch (err) {
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else {
        setError(err instanceof Error ? err.message : "整理任务操作失败");
        // A failed response may follow a successful file write: refresh state,
        // but never automatically retry a mutation or reconciliation.
        try { setJobs(await getOrganizationJobs(session.token)); } catch { /* retain the original error */ }
      }
    } finally { setConfirmed(false); setSourceRemoval(false); setDiscardStaging(false); setOldExecutorsStopped(false);setRemoteExclusive(false);setRemoteRecovery(false);setRemoteStopped(false); setBusy(false); }
  }
  async function closeRemote(){
    if(!job || busy || !closeConfirmed || !closeStopped || !closeReason.trim())return;
    setRemoteExclusive(false);setRemoteRecovery(false);setRemoteStopped(false);setConfirmed(false);setSourceRemoval(false);setSourceReview(null);setRestoreConfirmed(false);setRestoreStopped(false);
    setBusy(true);setError("");
    try{const saved=await closeRemoteOrganizationJob(session.token,job.id,closeReason,closeConfirmed,closeStopped);setJobs(rows=>[saved,...rows.filter(i=>i.id!==saved.id)]);}
    catch(err){if(err instanceof ApiError && err.code===401)onSessionExpired();else setError((err instanceof Error?err.message:"停止回执未核实")+"。请只读刷新任务，不要重发；已有原源隔离凭证时请改用原源恢复通道。");}
    finally{setCloseConfirmed(false);setCloseStopped(false);setBusy(false);}
  }
  return <WorkspaceLayout user={session.user} currentPath={currentPath} section="媒体整理" page="整理预览" onNavigate={onNavigate} onLogout={onLogout}>
    <section className="organization-preview">
      <h1>整理预览</h1>
      <p>此功能仅限管理员使用。各方式均需先保存任务，再单独确认执行，且不覆盖目标。复制和链接保留源名；移动会移除源名及核验后的恢复备份，需额外授权。硬链接共享文件内容，软链接依赖源文件长期存在。</p>
      {error && <p role="alert">{error}</p>}
      {!loading && <UnknownRecordManager token={session.token} records={unknown.items} enabled={!busy && roots.recordManagement === true} onChanged={async () => { invalidate(); setSelectedUnknown(null); await loadUnknown(unknown.page); }} onSessionExpired={onSessionExpired} />}
      {!loading && <RecordRedoForm token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} onChanged={refreshSavedJobs} />}
      {!loading && <RecordRenameForm token={session.token} executionModes={roots.executionModes} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} onChanged={refreshSavedJobs} />}
      {!loading && <SyncConfigManager token={session.token} enabled={!busy} onSessionExpired={onSessionExpired} onChanged={async () => {
        invalidate(); setSelectedUnknown(null); setSelectedHistory(null); setSyncConfigGeneration((value) => value + 1);
        const generation = previewGeneration.current;
        const configured = await getOrganizationRoots(session.token);
        if (generation !== previewGeneration.current) return;
        setRoots(configured); setSourceId(""); setTargetId("");
        await refreshSavedJobs("");
      }} />}
      {!loading && <DirectSyncForm key={syncConfigGeneration} token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} onChanged={refreshSavedJobs} />}
      {!loading && <SyncMonitorManager key={syncConfigGeneration} token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} onChanged={refreshSavedJobs} />}
      {!loading && <DownloadCompletionReviewForm token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} />}
      {!loading && <DownloadOrganizationManager token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} onChanged={refreshSavedJobs} />}
      {!loading && <DownloadMonitorManager token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} />}
      {!loading && <IntakeScraperManager token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} onChanged={invalidate} />}
      {!loading && <LibraryScraperManager token={session.token} enabled={!busy && !!roots.executionModes?.length} onSessionExpired={onSessionExpired} />}
      {!loading && <section aria-label="存量未识别记录"><h2>存量未识别记录</h2><p>列表读取不删除或自动重跑；记录删除／恢复须在独立管理入口审核确认。选择后须重新预览、纠正身份并另行确认执行；不能只处理目录的一部分就标记成功。</p><label>路径关键词<input value={unknownKeyword} disabled={busy} onChange={(event) => setUnknownKeyword(event.target.value)} /></label><button type="button" disabled={busy} onClick={() => void loadUnknown(1)}>查询／刷新记录</button><p>共 {unknown.total} 条，第 {unknown.page}/{unknown.totalPages} 页</p><div className="organization-table"><table><thead><tr><th>原路径／目标</th><th>原方式／核验</th><th>操作</th></tr></thead><tbody>{unknown.items.map((item) => <tr key={item.idText || item.id}><td>#{item.idText || item.id} · {item.path}<small>{item.dest || "未指定目标，需明确选择配置目录"}</small></td><td>{item.mode || "未记录方式，请自行确认"}{item.reason && <small>{item.reason}</small>}</td><td>{item.jobId ? <button type="button" disabled={busy || !roots.executionModes?.length} onClick={() => void loadUnknown(unknown.page, item.jobId)}>查看关联任务</button> : <button type="button" disabled={busy || !Number.isSafeInteger(item.id) || !!item.reason || !roots.executionModes?.length || !!item.nativeMode && !roots.executionModes.includes(item.nativeMode)} onClick={() => selectUnknown(item)}>选择并核验记录</button>}</td></tr>)}</tbody></table></div><button type="button" disabled={busy || unknown.page <= 1} onClick={() => void loadUnknown(unknown.page - 1)}>上一页</button><button type="button" disabled={busy || unknown.page >= unknown.totalPages} onClick={() => void loadUnknown(unknown.page + 1)}>下一页</button></section>}
      {selectedUnknown && <p>关联旧记录 #{selectedUnknown.id}：{selectedUnknown.path}。只有全范围成功才更新此条记录；原路径、方式和其他记录保留。<button type="button" disabled={busy} onClick={() => { invalidate(); setSelectedUnknown(null); }}>取消关联，返回普通规划</button></p>}
      {!loading && <section aria-label="历史记录重新规划"><h2>从历史重新规划</h2><p>保留原历史及原媒体；只从记录中的原来源重新扫描、核验身份并创建新任务。若移动后原来源已不存在，不会改用已发布目标，不直接改名或覆盖。</p><label>标题／源文件名<input value={historyKeyword} disabled={busy} onChange={(event) => setHistoryKeyword(event.target.value)} /></label><button type="button" disabled={busy} onClick={() => void loadHistory(1)}>查询／刷新历史</button><p>共 {history.total} 条，第 {history.page}/{history.totalPages} 页</p><div className="organization-table"><table><thead><tr><th>原身份／来源</th><th>原目标／方式</th><th>操作</th></tr></thead><tbody>{history.items.map((item) => <tr key={item.idText || item.id}><td>#{item.idText || item.id} · {item.title} {item.year} · TMDB {item.tmdbId}<small>{item.sourcePath}/{item.sourceFilename}</small></td><td>{item.destPath}/{item.destFilename}<small>{item.mode} · {item.date}</small>{item.reason && <small>{item.reason}</small>}</td><td><button type="button" disabled={busy || !Number.isSafeInteger(item.id) || !!item.reason || !roots.executionModes?.length || !!item.nativeMode && !roots.executionModes.includes(item.nativeMode)} onClick={() => selectHistory(item)}>定位原来源并重新核验</button></td></tr>)}</tbody></table></div><button type="button" disabled={busy || history.page <= 1} onClick={() => void loadHistory(history.page - 1)}>上一页</button><button type="button" disabled={busy || history.page >= history.totalPages} onClick={() => void loadHistory(history.page + 1)}>下一页</button></section>}
      {selectedHistory && <p>原历史 #{selectedHistory.id}：{selectedHistory.title} · TMDB {selectedHistory.tmdbId}。先重新预览，再逐文件纠正身份；原历史不会修改。<button type="button" disabled={busy} onClick={() => { invalidate(); setSelectedHistory(null); }}>取消历史关联</button></p>}
      {loading ? <p>正在读取配置目录…</p> : <form onSubmit={(event) => void preview(event)}>
        <label>来源目录<select value={sourceId} disabled={busy || !!selectedUnknown || !!selectedHistory} required onChange={(e) => { setSourceId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.sources.map((root) => <option key={root.id} value={root.id}>{root.label} · {root.path}</option>)}</select></label>
        <label>源目录内的相对路径<input value={path} disabled={busy || !!selectedUnknown || !!selectedHistory} required placeholder=". 或 Movies/某个目录" onChange={(e) => { setPath(e.target.value); invalidate(); }} /></label>
        <label>目标目录<select value={targetId} disabled={busy} required onChange={(e) => { setTargetId(e.target.value); invalidate(); }}><option value="">请选择</option>{roots.targets.map((root) => <option key={root.id} value={root.id} disabled={!!selectedDest && selectedDest !== root.path}>{root.label} · {root.path}</option>)}</select></label>
        <label>计划转移方式<select value={mode} disabled={busy || !!selectedUnknown?.nativeMode || !!selectedHistory?.nativeMode} onChange={(e) => { setMode(e.target.value); invalidate(); }}><option value="copy">复制</option><option value="move">移动</option><option value="link">硬链接</option><option value="softlink">软链接</option>{roots.executionModes?.includes("rclonecopy") && <option value="rclonecopy">Rclone 远程复制</option>}{roots.executionModes?.includes("rclone") && <option value="rclone">Rclone 远程移动</option>}</select></label>
        <label><input type="checkbox" checked={allowDiscExpansion} disabled={busy || !!selectedUnknown} onChange={(event)=>{setAllowDiscExpansion(event.target.checked);invalidate();}} />若选择的是蓝光内部文件或子目录，我同意扩大到完整光盘后重新审核。整盘包括菜单、证书、全部片段及其他文件，不拆成独立影片；勾选不会执行。</label>
        <label><input type="checkbox" checked={batchEnabled} disabled={busy} onChange={(event) => { setBatchEnabled(event.target.checked); invalidate(); }} />启用当前范围的批量 TV 集号选择</label>
        {batchEnabled && <fieldset disabled={busy}><legend>批量集号规则</legend>
          <p>只处理当前源范围中完整匹配模板的媒体。筛选依据偏移前的起始集号；多集文件不可拆分，保留完整范围。其他媒体及唯一附件显示为明确筛除，不执行。不能与逐文件身份指定混用。</p>
          <label>TV TMDB ID<input value={batchId} required pattern="[1-9][0-9]*" onChange={(event) => { setBatchId(event.target.value); invalidate(); }} /></label>
          <label>季号<input type="number" min="0" max="99" value={batchSeason} required onChange={(event) => { setBatchSeason(event.target.value); invalidate(); }} /></label>
          <label>完整文件名模板<input value={batchFormat} required maxLength={512} placeholder="Show.第{ep}集.mkv" onChange={(event) => { setBatchFormat(event.target.value); invalidate(); }} /></label>
          <label>筛选起始集号（两项均留空为不筛选）<input type="number" min="1" max="9999" value={batchStart} onChange={(event) => { setBatchStart(event.target.value); invalidate(); }} /></label>
          <label>筛选结束集号<input type="number" min="1" max="9999" value={batchEnd} onChange={(event) => { setBatchEnd(event.target.value); invalidate(); }} /></label>
          <label>集号偏移（留空为 0）<input type="number" min="-99999" max="99999" value={batchOffset} onChange={(event) => { setBatchOffset(event.target.value); invalidate(); }} /></label>
          <p>模板含 {'{ep}'}，其他命名占位符如 {'{a}'} 表示非空可变内容。重新预览仍不执行文件；部分筛选不能将整条未识别记录标记成功。</p>
        </fieldset>}
        <button disabled={busy || !sourceId || !targetId} type="submit">{busy ? "正在核验…" : "生成只读预览"}</button>
        {(!roots.sources.length || !roots.targets.length) && <p>请先在现有配置中设置下载/同步源目录和媒体库目标目录。</p>}
      </form>}
      {plan && <div aria-live="polite"><p>共 {plan.items.length} 个媒体或附件；跳过 {plan.skipped} 个隐藏、非媒体或常见未完成后缀项。此预览不能直接执行，也不证明文件已下载完成。</p>
        {plan.items.filter((item)=>item.disc).map((item)=><p key={`disc-${item.source}`}>蓝光整盘：{item.source} → {item.target || "尚无目标"}；{item.disc!.files} 个文件、{item.disc!.directories} 个子目录、合计 {item.size} 字节。{item.disc!.expanded && "已从内部选择扩展至整盘。"}四种本地方式保留全部目录结构，不逐片段改名；移动将移除这棵已审核的源目录，不移除配置的来源根。</p>)}
        {plan.scraperEnabled && <p>此计划启用了入库刮削：整组整理及移动清理完成后，按原配置补齐缺失 NFO 和图片；已有文件不覆盖，处理结果在刮削回执中独立核查。</p>}
        {plan.remote && <p>远程目标：NASTOOL 下的逻辑目录，不校验或写入同名本机目录。发布非原子，执行前须独立确认停止外部写入器、命名空间独占及恢复副本／元数据暂存的长期空间和流量费用。远端不支持实体空目录时只保留清单；远程 FFmpeg 缺图回退和自动化仍未开放，不能据此替换生产服务。</p>}
        {plan.items.filter((item) => item.kind === "media" && item.partOverride !== undefined).map((item) => <p key={`part-${item.source}`}>分段覆盖：{item.source} → {item.partOverride || "明确清空"}（唯一附件继承）</p>)}
        <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>核验状态</th></tr></thead><tbody>{plan.items.map((item) => <tr key={item.source}><td>{item.source}<small>{item.size} 字节 · {item.kind}{item.part ? ` · 分段 ${item.part}` : ""}</small>{(item.episodeFormat || !!item.episodeOffset) && <small>集号规则：{item.episodeFormat || "明确编号"} · 偏移 {item.episodeOffset || 0}</small>}</td><td>{item.target || "未生成"}{item.tmdbId && <small>{item.title} {item.year} · {item.mediaType || "待核验类型"} · TMDB {item.tmdbId}{item.assigned ? " · 人工指定" : ""}</small>}</td><td>{statusLabels[item.status] || item.status}{item.reason && <small>{item.reason}</small>}</td></tr>)}</tbody></table></div>
        {!batchEnabled && <form onSubmit={(event) => void assignIdentity(event)} aria-label="逐文件指定媒体身份">
          <h2>纠正媒体身份</h2><p>只影响所选媒体文件，同名附件仍须唯一匹配；其他文件继续自动识别。TMDB 标题、年份、分类和偏移后的季集上限由服务器核验，不允许自填目标路径，也不能绕过大小、蓝光或冲突限制。应用只重新生成只读预览，不执行整理。</p>
          <label>当前预览中的媒体文件<select value={manualSource} disabled={busy} required onChange={(event) => setManualSource(event.target.value)}><option value="">请选择</option>{plan.items.filter((item) => item.kind === "media").map((item) => <option key={item.source} value={item.source}>{item.source}</option>)}</select></label>
          <label>媒体查询类型<select value={manualKind} disabled={busy} onChange={(event) => { setManualKind(event.target.value as "movie" | "tv" | "anime"); setManualSeason(""); setManualEpisode(""); setManualEndEpisode(""); setManualEpisodeFormat(""); setManualEpisodeOffset(""); }}><option value="movie">电影</option><option value="tv">电视剧</option><option value="anime">动漫（TV 元数据查询）</option></select></label>
          {manualKind !== "movie" && <p>电视剧和动漫均使用 TV 元数据；最终是否动漫以 TMDB 动画分类为准，不强改非动画作品。未配置动漫目录时使用电视剧目录。</p>}
          <label>TMDB ID<input value={manualId} disabled={busy} required inputMode="numeric" pattern="[1-9][0-9]*" onChange={(event) => setManualId(event.target.value)} /></label>
          <label>分段处理<select value={manualPartMode} disabled={busy} onChange={(event) => setManualPartMode(event.target.value as "auto" | "set" | "clear")}><option value="auto">自动识别</option><option value="set">指定分段</option><option value="clear">明确清空分段</option></select></label>
          {manualPartMode === "set" && <label>分段值<input value={manualPart} maxLength={32} disabled={busy} required placeholder="如 PARTIII、CD01、DISC12" onChange={(event) => setManualPart(event.target.value)} /></label>}
          {manualPartMode !== "auto" && <p>只覆盖所选文件和唯一附件的分段；明确清空可能导致目标重名，服务器仍会阻止冲突，不自动改名或覆盖。</p>}
          {manualKind !== "movie" && <>
            <label>季号（0 为特别篇）<input type="number" min="0" max="99" value={manualSeason} disabled={busy} required onChange={(event) => setManualSeason(event.target.value)} /></label>
            <label>集号定位模板（留空则使用下面的明确集号）<input value={manualEpisodeFormat} maxLength={512} disabled={busy} placeholder="Show.第{ep}集.{a}.mkv" onChange={(event) => setManualEpisodeFormat(event.target.value)} /></label>
            <p>模板完整匹配所选文件名（含扩展名），字面忽略大小写。用 {'{ep}'} 标定数字或 EP11-EP13 范围；支持匿名字段、转义花括号、同值重复字段和文字宽度／对齐格式，如 {'{ep:4}'}。非集号字段支持常见字符、数值和日期／时间类型（如 {'{date:ti}'}）；日期和时间须有效。集号必须保留文字，自定义类型暂不支持。不会应用到目录中的其他文件。</p>
            {manualDisc && <p>蓝光可仅指定季号，将整盘作为该季目录；集号、结束集号、模板和偏移均留空。没有片段到剧集的核实映射时，不生成逐集 NFO 或缩略图。</p>}
            <label>起始集号<input type="number" min="1" max="9999" value={manualEpisode} disabled={busy || !!manualEpisodeFormat} required={!manualEpisodeFormat && !manualDisc} onChange={(event) => setManualEpisode(event.target.value)} /></label>
            <label>结束集号（单集留空）<input type="number" min="1" max="9999" value={manualEndEpisode} disabled={busy || !!manualEpisodeFormat} onChange={(event) => setManualEndEpisode(event.target.value)} /></label>
            <label>集号偏移（留空为 0，如 -10）<input type="number" min="-99999" max="99999" step="1" value={manualEpisodeOffset} disabled={busy} onChange={(event) => setManualEpisodeOffset(event.target.value)} /></label>
          </>}
          <button type="submit" disabled={busy || !manualSource}>应用所选身份并重新预览</button>
        </form>}
        {roots.executionModes?.includes(mode) && <><p>仅将“可规划”的文件保存为待确认{modeLabels[mode] || mode}任务；冲突、未识别和已阻止的文件不会执行。正在编辑的人工身份必须先应用并重新预览。</p><button type="button" disabled={busy || !!manualSource || !plan.items.some((item) => item.status === "available")} onClick={() => void jobOperation("create")}>保存待确认{modeLabels[mode] || mode}任务</button></>}
      </div>}
      {!!assignments.length && <button type="button" disabled={busy} onClick={() => { setAssignments([]); void generatePreview([]); }}>清除所有人工指定并重新预览</button>}
      {!loading && !roots.executionModes?.length && <p>当前运行模式仅支持预览。实际转移只在旧后端已禁用的 Go 模式开放；请勿直接切换未经完整验收的生产环境。</p>}
      {!!roots.executionModes?.length && <section aria-label="持久整理任务">
        <h2>已保存的整理任务</h2>
        <button type="button" disabled={busy} onClick={() => void jobOperation("refresh")}>刷新任务状态</button>
        <label>最近 30 个任务<select value={jobId} disabled={busy} onChange={(event) => { setJobId(event.target.value); setConfirmed(false); setSourceRemoval(false); }}><option value="">请选择</option>{jobs.map((item) => <option key={item.id} value={item.id}>{item.created} · {modeLabels[item.mode] || item.mode} · {item.id.slice(0, 8)} · {jobLabels[item.state] || item.state}</option>)}</select></label>
        {job && <><p>转移方式：{modeLabels[job.mode] || job.mode}。{jobLabels[job.state] || job.state}。中断记录不会自动重跑；“核对已发布文件”只根据保存的凭证确认结果，不会重新转移。</p>
          <p>任务保存的来源：<code>{job.sourceRoot || "未提供，请刷新后确认"}</code><br />任务保存的目标：<code>{job.targetRoot || "未提供，请刷新后确认"}</code></p>
          {remoteJob && <section aria-label="远程转移独立确认"><h3>远程转移约束</h3><p>目标为 NASTOOL 远程下的逻辑路径，不是本机目录。远程发布不是跨服务商的原子操作，期间可能看到不完整文件；本程序不能替你停止外部写入器。完整私有恢复副本和刮削暂存持续保留，会增加云端容量、读取流量及费用，不自动清理。{job.remote?.directories?"当前后端报告可保留空目录。":"空目录只保存在私有清单中，不承诺远端存在实体空目录。"}远程同步／下载完成调度须在独立入口重新审核授权，不继承本次确认；远程 FFmpeg 缺图回退和已有库批量刮削仍未开放。</p>
            {["ready","needs_review"].includes(job.state) && <><label><input type="checkbox" checked={remoteStopped} disabled={busy} onChange={e=>setRemoteStopped(e.target.checked)}/>我已实际停止旧执行器及所有会写本次来源／远程命名空间的外部程序。</label><label><input type="checkbox" checked={remoteExclusive} disabled={busy} onChange={e=>setRemoteExclusive(e.target.checked)}/>我确认本程序独占此远程命名空间，并接受非原子可见性；恢复时按保存清单的完整内容核对，不以同名或存在即认定成功。</label><label><input type="checkbox" checked={remoteRecovery} disabled={busy} onChange={e=>setRemoteRecovery(e.target.checked)}/>我同意保留完整私有恢复副本及元数据暂存并承担空间／流量费用，本次操作不授权删除它们。</label></>}
          </section>}
          {job.remoteClosure && <p>停止回执：{job.remoteClosure.actor} · {job.remoteClosure.created} · {job.remoteClosure.reason}。目标、暂存、预留和既有历史全部保留，不代表转移成功。</p>}
          {remoteJob && job.state==="needs_review" && <section aria-label="停止远程任务"><h3>仅停止远程任务并留存证据</h3><p>不删除或重写任何远程对象，也不释放目标预留。已有源隔离凭证时须使用下方原源恢复流程；不会在此推断原源已恢复。</p><label>处置原因<input value={closeReason} maxLength={128} disabled={busy} onChange={e=>{setCloseReason(e.target.value);setCloseConfirmed(false);setCloseStopped(false);}}/></label><label><input type="checkbox" checked={closeStopped} disabled={busy} onChange={e=>setCloseStopped(e.target.checked)}/>我已实际停止旧执行器和外部写入器。</label><label><input type="checkbox" checked={closeConfirmed} disabled={busy || !closeReason.trim()} onChange={e=>setCloseConfirmed(e.target.checked)}/>我确认仅停止未完成工作，保留所有对象、恢复副本和预留，不记成功、不继续移动。</label><button type="button" disabled={busy || !closeConfirmed || !closeStopped || !closeReason.trim()} onClick={()=>void closeRemote()}>确认仅停止远程任务并保存回执</button></section>}
          {job.legacyOrigin && <p>旧接口 {job.legacyOrigin.action} 请求：{job.legacyOrigin.kind === "custom" ? "自定义路径识别" : `${job.legacyOrigin.kind === "history" ? "原历史" : "未识别记录"} #${job.legacyOrigin.recordId}`} · 操作者 {job.legacyOrigin.actor}。实际结果以本任务的持久回执为准，接口失败不意味着可以重跑。</p>}
          {job.legacySelection && <p>已保存自定义识别规则：{job.legacySelection.kind || "自动类型"} · TMDB {job.legacySelection.tmdbId || "自动识别"} · 季 {job.legacySelection.season ?? "识别值或 TV 默认 1"} · 集号模板 {job.legacySelection.episodeFormat || "无"} · 范围 {job.legacySelection.episodeDetails || "无"} · 偏移 {job.legacySelection.episodeOffset || 0} · 最小大小 {job.legacySelection.minimumMB === undefined ? "配置值" : `${job.legacySelection.minimumMB} MiB`}。只执行已保存项目，不重新扫描扩大范围。</p>}
          {job.legacyRename && <p>关联改名范围：{job.legacyRename.expandedScope ? "已独立授权扩展到原文件父目录" : "原记录范围"}。{job.legacyRename.preserveUnknown ? "仅代表保存条目成功，原未识别记录保持原状，不标记整条成功。" : "兼容状态只根据完整范围成功回执提交。"}</p>}
          {job.legacyTargetOverride && <p>本任务使用请求或原记录明确指定的配置目标根，不按媒体类型自动另选目录；仍不覆盖现有目标。</p>}
          {job.sync && <p>同步配置 #{job.sync.record.id} · {job.sync.record.note || "无备注"} · 操作者 {job.sync.actor} · 审核范围 {job.sync.scope}。{job.sync.record.rename === 1 ? "识别整理：核验后记录媒体转移历史。" : "不识别、不改名：核验后仅写 SYNC_HISTORY，不造媒体识别历史。"}{job.sync.monitorGeneration && `监控批次 ${job.sync.monitorGeneration} · 事件 ${job.sync.monitorEvent}。`}旧记录抑制重做不等于当前文件存在；配置变动需重新审核，任务不因刷新自动执行。</p>}
          {job.download && <p>下载完成整理 · 下载器 #{job.download.downloaderId} · 任务 {job.download.task.id} · 批次 {job.download.batchId} · 操作者 {job.download.actor}。仅处理已保存的选择范围；中断后明确核对，不自动重跑。下载器标签／任务记录回写须在独立批次入口审核。</p>}
          {job.episodeSelection && <p>已保存批量 TV 规则：TMDB {job.episodeSelection.tmdbId} · 季 {job.episodeSelection.season} · {job.episodeSelection.episodeFormat} · 偏移 {job.episodeSelection.episodeOffset || 0} · 偏移前起始集筛选 {job.episodeSelection.startEpisode === undefined ? "不限制" : `${job.episodeSelection.startEpisode}–${job.episodeSelection.endEpisode}`}。只执行已保存项目，不重扫或扩大目录范围。</p>}
          {job.items.filter((item) => item.kind === "media" && item.partOverride !== undefined).map((item) => <p key={`part-${item.index}`}>已保存分段覆盖：{item.source} → {item.partOverride || "明确清空"}</p>)}
          {job.items.filter((item)=>item.disc).map((item)=><p key={`disc-${item.index}`}>已保存蓝光整盘：{item.source} → {item.target}，{item.disc!.files} 个文件、{item.disc!.directories} 个子目录。{item.disc!.expanded && "此任务已明确审核从内部选择扩展至整盘。"}恢复只核对这份保存的目录清单，不重新扫描扩大范围。</p>)}
          {movingMode(job.mode) && ["needs_review","source_restoring","source_restored"].includes(job.state) && <section aria-label="恢复失败移动的原源"><h3>恢复原源并停止此任务</h3><p>这是继续移动的独立替代操作。只将已核验的原文件／整盘恢复到原位置，不覆盖新到同名对象；普通文件可从仍固定原节点的完整见证恢复。目标即使不可用也不会被删除、重写或冒认为已核验。所有暂存、备份、目标预留和既有成功历史保留，占用磁盘；未开始项目停止，不生成新的成功历史／入库事件，不重新授权监控或下载器回写。</p>
            {job.state!=="source_restored" && <><button type="button" disabled={busy} onClick={()=>void sourceRestoreOperation(false)}>只读核对原源恢复条件（可能需要读取完整文件）</button>
              {sourceReview && <><p>{sourceReview.canRestore?"当前原源证据可供本次恢复审核":"当前证据不允许恢复"} · {sourceReview.reason}</p><ul>{sourceReview.items.map(i=><li key={i.index}>{i.source} · {sourceOutcomes[i.outcome]||i.outcome}</li>)}</ul>
                <label>处置原因<input value={restoreReason} maxLength={128} disabled={busy || !!job.sourceRestore} onChange={e=>{setRestoreReason(e.target.value);setRestoreConfirmed(false);setRestoreStopped(false);}}/></label>
                <label><input type="checkbox" checked={restoreStopped} disabled={busy || !sourceReview.canRestore} onChange={e=>setRestoreStopped(e.target.checked)}/>我已实际停止旧整理执行器、其他实例及会写这些源／恢复路径的下载器任务或外部程序；此操作不会替我停止它们。</label>
                <label><input type="checkbox" checked={restoreConfirmed} disabled={busy || !sourceReview.canRestore || !restoreReason.trim()} onChange={e=>setRestoreConfirmed(e.target.checked)}/>我同意按这份新审核恢复原源并停止所有未完成项目，保留目标和全部恢复证据，不继续转移、不授权源移除或暂存删除。</label>
                <button type="button" disabled={busy || !sourceReview.canRestore || !restoreConfirmed || !restoreStopped || !restoreReason.trim()} onClick={()=>void sourceRestoreOperation(true)}>确认{job.state==="source_restoring"?"续办":"进行"}原源恢复并保存回执</button>
              </>}
            </>}
            {job.sourceRestore && <><p>原源恢复回执：{job.sourceRestore.actor} · {job.sourceRestore.created} · {job.sourceRestore.reason} · {jobLabels[job.sourceRestore.state]||job.sourceRestore.state}。这是保存时的历史回执，不保证之后的外部改动；刷新不重跑文件操作。</p><ul>{job.sourceRestore.items.map(i=><li key={i.index}>{i.source} · {sourceOutcomes[i.outcome]||i.outcome}</li>)}</ul></>}
          </section>}
          {job.historyRecordId && <p>由原历史 #{job.historyRecordId} 重新规划。原历史保留，只有新的核验转移成功才追加新历史；不是覆盖原媒体的授权。</p>}
          {job.unknownRecordId && <p>关联旧记录 #{job.unknownRecordId}：{job.unknownState === "resolved" ? "已根据完整成功回执更新为已处理" : job.unknownState === "preserved" ? "选中条目回执已完成，原记录状态保持不变，不能视为整条已处理" : job.unknownState === "cancelled" ? "已取消关联，旧记录仍保留" : job.unknownState === "detached" ? "管理员已明确解除关联，未标记旧记录成功" : "仍待处理；全部项目与清理回执完成后才更新"}。</p>}
          {job.unknownDetach && <p>解除关联回执：{job.unknownDetach.actor} · {job.unknownDetach.created} · {job.unknownDetach.reason}。不代表旧记录已处理。</p>}
          {job.unknownState === "pending" && job.items.every((item) => item.state === "completed" && !item.reason) && <section aria-label="明确解除旧记录关联"><p>记录变更、删除或新文件到来导致兼容回执不能完成时，可在所有原项目已成功且清理完成后另行解除关联。此动作不改变旧行、不标识已处理、不动文件或历史，也不释放媒体目标预留。</p><button type="button" disabled={busy} onClick={() => void unknownDetachOperation(false)}>读取原始及当前记录快照</button>{detachReview && <><p>原记录：{detachReview.original.path} → {detachReview.original.dest || "未指定"} · {detachReview.original.mode} · {detachReview.original.state}<br />当前记录：{detachReview.current ? `${detachReview.current.path} → ${detachReview.current.dest || "未指定"} · ${detachReview.current.mode} · ${detachReview.current.state}` : "此 ID 的记录已不存在"}</p><label>处置原因<input value={detachReason} disabled={busy} maxLength={128} onChange={(event) => { setDetachReason(event.target.value); setDetachConfirmed(false); }} /></label><label className="organization-confirm"><input type="checkbox" checked={detachConfirmed} disabled={busy || !detachReview.eligible || !detachReason.trim()} onChange={(event) => setDetachConfirmed(event.target.checked)} />我已核对这份快照和原任务完整成功回执，同意仅解除记录关联；旧记录保持原状并留待单独处理。</label><button type="button" disabled={busy || !detachConfirmed || !detachReview.eligible || !detachReason.trim()} onClick={() => void unknownDetachOperation(true)}>确认仅解除关联并保存审核回执</button></>}</section>}
          {job.unknownState === "pending" && job.items.every((item) => item.state === "completed" && !item.reason) && <><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />{job.legacyRename?.preserveUnknown ? "我同意核验已选条目的完整账本并确认原记录保持原状，不补标整记录成功。" : "我同意仅核验完整成功账本并补交原未识别记录状态，不重新操作文件。"}</label><button type="button" disabled={busy || !confirmed} onClick={() => void jobOperation("reconcile-unknown")}>{job.legacyRename?.preserveUnknown ? "确认核对保留原记录回执" : "确认核对未识别记录回执"}</button></>}
          <div className="organization-table"><table><thead><tr><th>源文件</th><th>目标相对路径</th><th>任务状态</th></tr></thead><tbody>{job.items.map((item) => <tr key={item.index}><td>{item.source}{item.part && <small>分段 {item.part}</small>}{(item.episodeFormat || !!item.episodeOffset) && <small>集号规则：{item.episodeFormat || "明确编号"} · 偏移 {item.episodeOffset || 0}</small>}</td><td>{item.target}<small>TMDB {item.tmdbId}{item.assigned ? " · 人工指定" : ""}</small></td><td>{jobLabels[item.state] || item.state}{item.reason && <small>{jobReasonLabels[item.reason] || item.reason}</small>}</td></tr>)}</tbody></table></div>
          {movingMode(job.mode) && (job.state === "ready" || job.state === "needs_review") && <label className="organization-confirm"><input type="checkbox" checked={sourceRemoval} disabled={busy || !job.sourceRoot || !job.targetRoot} onChange={(event) => setSourceRemoval(event.target.checked)} />我同意从上述任务保存的来源路径移除原源文件名（蓝光为已审核的整盘目录），并在目标核验及历史提交后清理本任务的源恢复备份；不删除新到的同名对象、其他文件或配置的来源根目录。</label>}
          {job.state === "ready" && <><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已确认源文件下载完成、至少 30 秒未修改，并同意按{modeLabels[job.mode] || job.mode}方式整理以上待执行文件。</label><button type="button" disabled={busy || !remoteApproved || !confirmed || (movingMode(job.mode) && (!sourceRemoval || !job.sourceRoot || !job.targetRoot))} onClick={() => void jobOperation("execute")}>确认执行{modeLabels[job.mode] || job.mode}</button></>}
          {job.state === "needs_review" && (movingMode(job.mode) ? <><p>普通核对不会继续移动。只有保存凭证能证明原文件身份时才可继续；未知状态仍保留，未开始的文件不会由恢复操作执行。</p><label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已检查上述任务路径和恢复记录，同意继续核验移动及清理。</label><button type="button" disabled={busy || !remoteApproved || !confirmed || !sourceRemoval || !job.sourceRoot || !job.targetRoot} onClick={() => void jobOperation("resume-move")}>确认继续移动及恢复清理</button></> : <button type="button" disabled={busy || !remoteApproved} onClick={() => void jobOperation("reconcile")}>核对已发布文件，不重试转移</button>)}
          {job.state === "needs_review" && job.items.some((item) => (item.state === "prepared" || remoteJob && item.state === "publishing")) && <><p>“继续发布”使用已完整准备的暂存对象；本地不重新复制，远程只从保留的私有副本复制缺失文件，并核验所有已有文件完整内容。不是重新上传源或强制覆盖，证据不足时仍保留待核对。移动会在发布核验后按上述授权继续源移除。</p>{!movingMode(job.mode) && <label className="organization-confirm"><input type="checkbox" checked={confirmed} disabled={busy} onChange={(event) => setConfirmed(event.target.checked)} />我已检查上述任务保存的路径，确认源文件已完成，同意继续发布完整暂存对象并提交整理记录。</label>}<button type="button" disabled={busy || !remoteApproved || !confirmed || !job.sourceRoot || !job.targetRoot || (movingMode(job.mode) && !sourceRemoval)} onClick={() => void jobOperation("resume-publication")}>确认继续发布已完整暂存文件{movingMode(job.mode) ? "并完成移动" : ""}</button></>}
          {canAbandonOrganizationJob(job) && <section aria-label="放弃未发布暂存"><p>这是停止整个任务的独立操作，不是继续发布或移动。只清理账本能够证明归属的未发布暂存；包括有写入前凭证、与完整原源核验相符的半写文件。必须重新证明原源文件完整且目标不存在。已发布、已移动、无凭证的执行中状态及未知半成品不会清理，也不会接续复制。所有清理回执保存后才释放目标预留，不写入整理成功历史。</p><label className="organization-confirm"><input type="checkbox" checked={oldExecutorsStopped} disabled={busy} onChange={(event) => setOldExecutorsStopped(event.target.checked)} />我已实际停止旧 Python、未升级的 Go 整理执行器及其他会操作这些路径的任务；勾选此项不会替我停止服务。</label><label className="organization-confirm"><input type="checkbox" checked={discardStaging} disabled={busy || !job.sourceRoot || !job.targetRoot} onChange={(event) => setDiscardStaging(event.target.checked)} />我同意放弃上述整个任务，并删除其已核验的未发布暂存；保留原源文件，不删除媒体库文件或未知对象。</label><button type="button" disabled={busy || !discardStaging || !oldExecutorsStopped || !job.sourceRoot || !job.targetRoot} onClick={() => void jobOperation("abandon-unpublished")}>{job.state === "abandoning" ? "确认续办暂存处置回执" : "确认放弃未发布暂存及整个任务"}</button></section>}
          {job.items.every((item) => item.state === "planned") && <button type="button" disabled={busy} onClick={() => void jobOperation("cancel")}>取消未执行任务</button>}
        </>}
      </section>}
    </section>
  </WorkspaceLayout>;
}
