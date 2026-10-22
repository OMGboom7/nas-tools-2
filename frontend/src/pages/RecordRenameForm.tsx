import { useEffect, useRef, useState } from "react";
import { ApiError, OrganizationRenameError, previewRecordRename, executeRecordRename, previewCustomIdentification, executeCustomIdentification, organizationMovesSource, type OrganizationRenameInput, type OrganizationRenameReview, type OrganizationCustomInput } from "../api/client";
import { RemoteTransferConsent } from "../components/RemoteTransferConsent";

type Props = { token: string; enabled: boolean; executionModes?:string[]; onChanged: (jobId: string) => Promise<void>; onSessionExpired: () => void };

// This is an explicit preview/execute workflow. Loading, edits and errors never
// execute, expand, retry or reconcile a filesystem job in the background.
export function RecordRenameForm({ token, enabled, executionModes=[], onChanged, onSessionExpired }: Props) {
  const [kind, setKind] = useState("unknown");
  const [recordID, setRecordID] = useState("");
  const [mode, setMode] = useState("copy");
  const [source,setSource]=useState(""),[target,setTarget]=useState("");
  const [remoteExclusive,setRemoteExclusive]=useState(false),[remoteRecovery,setRemoteRecovery]=useState(false),[oldStopped,setOldStopped]=useState(false);
  const [mediaType, setMediaType] = useState<"" | "MOV" | "TV" | "ANIME">("");
  const [tmdb, setTMDB] = useState("");
  const [season, setSeason] = useState("");
  const [format, setFormat] = useState("");
  const [details, setDetails] = useState("");
  const [offset, setOffset] = useState("");
  const [minimum, setMinimum] = useState("");
  const [partMode, setPartMode] = useState("auto");
  const [part, setPart] = useState("");
  const [parentConsent, setParentConsent] = useState(false);
  const [confirmed, setConfirmed] = useState(false);
  const [sourceRemoval, setSourceRemoval] = useState(false);
  const [review, setReview] = useState<OrganizationRenameReview | null>(null);
  const [savedID, setSavedID] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const inFlight = useRef(false);
  const remoteRequired=!!review?.plan.remote;
  const multi=(review?.plan.partitions?.length || 0)>1;
  const stoppedRequired=remoteRequired||multi;
  const moves=organizationMovesSource(mode);
  function clearRemote(){setRemoteExclusive(false);setRemoteRecovery(false);setOldStopped(false);}
  useEffect(() => { generation.current++; inFlight.current = false; setReview(null); setRecordID(""); setTMDB(""); setMediaType(""); setSeason(""); setFormat(""); setDetails(""); setOffset(""); setMinimum(""); setPartMode("auto"); setPart(""); setParentConsent(false); setConfirmed(false); setSourceRemoval(false); setSavedID(""); setError(""); setBusy(false); return () => { generation.current++; }; }, [token]);
  useEffect(()=>{clearRemote();setSource("");setTarget("");},[token]);
  function invalidate() { generation.current++; setReview(null); setConfirmed(false); setSourceRemoval(false); setParentConsent(false); setError("");clearRemote(); }
  function input(): OrganizationRenameInput {
    if (kind!=="custom" && (!/^[1-9][0-9]{0,18}$/.test(recordID) || BigInt(recordID) > 9223372036854775807n)) throw new Error("请输入完整、精确的正整数记录 ID");
    return { ...(kind === "custom" ? {} : kind === "unknown" ? { unknown_id: recordID } : { logid: recordID }), syncmod: mode, ...(mediaType ? { type: mediaType } : {}), ...(tmdb ? { tmdb } : {}), ...(season ? { season } : {}), ...(format ? { episode_format: format } : {}), ...(details ? { episode_details: details } : {}), ...(offset ? { episode_offset: offset } : {}), ...(minimum ? { min_filesize: minimum } : {}), ...(partMode === "auto" ? {} : { episode_part: partMode === "clear" ? "" : part }), ...(kind==="custom"?{}:{confirmParentScope: parentConsent}) };
  }
  async function operate(execute: boolean) {
    if (!enabled || inFlight.current || execute && (!review || !confirmed || moves && !sourceRemoval || remoteRequired && (!remoteExclusive || !remoteRecovery) || stoppedRequired&&!oldStopped)) return;
    inFlight.current = true;
    const version = ++generation.current;
    setBusy(true); setError(""); setSavedID("");
    try {
      const request = input();
      const custom:OrganizationCustomInput={...request,inpath:source,outpath:target};
      if (!execute) {
        setReview(null); setConfirmed(false); setSourceRemoval(false);clearRemote();
        const next = kind==="custom"?await previewCustomIdentification(token,custom):await previewRecordRename(token, request);
        if (version === generation.current) setReview(next);
      } else {
        const remote=stoppedRequired?{confirmRemoteExclusive:remoteExclusive,confirmRemoteRecoveryCopy:remoteRecovery,confirmOldExecutorsStopped:oldStopped}:undefined;
        const result = kind==="custom"?await executeCustomIdentification(token,custom,review!.plan.fingerprint,true,sourceRemoval,remote):await executeRecordRename(token, request, review!.plan.fingerprint, true, sourceRemoval,remote);
        if (version !== generation.current) return;
        setSavedID(result.jobId); setReview(null); setConfirmed(false); setSourceRemoval(false); setParentConsent(false);clearRemote();
        await onChanged(result.jobId);
      }
    } catch (err) {
      if (version !== generation.current) return;
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setError(err instanceof Error ? err.message : "改名请求未完成");
      setConfirmed(false); setSourceRemoval(false);clearRemote();
      if (execute) {
        setReview(null); setParentConsent(false);
        if (err instanceof OrganizationRenameError && err.jobId) setSavedID(err.jobId);
        // A request can commit before its response is lost. Only read saved
        // jobs on failure, never send the mutation again automatically.
        try { await onChanged(err instanceof OrganizationRenameError ? err.jobId || "" : ""); } catch { /* retain original failure */ }
      }
    } finally { if (version === generation.current) { inFlight.current = false; setBusy(false); } }
  }
  return <section aria-label="关联记录手动识别与改名">
    <h2>关联记录手动识别与改名</h2>
    <p>使用上方列表中的记录 ID。保留旧历史和已发布媒体，仅从原来源按本次方式整理，不覆盖目标；模板作用于单文件记录时必须另行确认扩展到父目录。预览和刷新不执行。</p>
    {error && <p role="alert">{error}</p>}
    {savedID && <p>已保存任务：<code>{savedID}</code>。请在任务列表查看持久回执，错误不代表可以重跑。</p>}
    <fieldset disabled={!enabled || busy}>
      <label>识别来源<select value={kind} onChange={(e) => { setKind(e.target.value); invalidate(); }}><option value="unknown">未识别记录</option><option value="history">原历史</option><option value="custom">自定义配置内路径（不关联旧记录）</option></select></label>
      {kind!=="custom"?<label>精确记录 ID<input value={recordID} inputMode="numeric" maxLength={19} onChange={(e) => { setRecordID(e.target.value); invalidate(); }} /></label>:<><label>配置内原来源绝对路径<input value={source} maxLength={4096} onChange={e=>{setSource(e.target.value);invalidate();}}/></label><label>明确配置目标根（远程为逻辑根）<input value={target} maxLength={4096} onChange={e=>{setTarget(e.target.value);invalidate();}}/></label></>}
      <label>本次转移方式<select value={mode} onChange={(e) => { setMode(e.target.value); invalidate(); }}><option value="copy">复制</option><option value="link">硬链接</option><option value="softlink">软链接</option><option value="move">移动</option>{executionModes.includes("rclonecopy")&&<option value="rclonecopy">Rclone 远程复制</option>}{executionModes.includes("rclone")&&<option value="rclone">Rclone 远程移动</option>}{executionModes.includes("miniocopy")&&<option value="miniocopy">MinIO 远程复制</option>}{executionModes.includes("minio")&&<option value="minio">MinIO 远程移动</option>}</select></label>
      <label>媒体查询类型<select value={mediaType} onChange={(e) => { setMediaType(e.target.value as typeof mediaType); invalidate(); }}><option value="">自动识别</option><option value="MOV">电影</option><option value="TV">电视剧</option><option value="ANIME">动漫（TV 元数据查询）</option></select></label>
      <p>动漫身份由 TMDB 动画分类确定，不将非动画作品强改为动漫。未指定目的的旧记录按配置类型选库；原记录明确指定目的时仍使用该目录。</p>
      <label>TMDB ID（留空为自动）<input value={tmdb} maxLength={19} onChange={(e) => { setTMDB(e.target.value); invalidate(); }} /></label>
      <label>季号（留空沿用识别；TV 无值时为 1）<input type="number" value={season} min="0" max="99" onChange={(e) => { setSeason(e.target.value); invalidate(); }} /></label>
      <label>完整文件名集号模板<input value={format} maxLength={512} placeholder="Show.{ep}.mkv" onChange={(e) => { setFormat(e.target.value); invalidate(); }} /></label>
      <label>集号规则<input value={details} placeholder="n 或 n-m 指定；n,m 筛选原始集号" maxLength={20} onChange={(e) => { setDetails(e.target.value); invalidate(); }} /></label>
      <label>集号偏移<input type="number" min="-99999" max="99999" value={offset} onChange={(e) => { setOffset(e.target.value); invalidate(); }} /></label>
      <label>本次最小 MiB（留空沿用配置）<input type="number" min="0" max="1000000" value={minimum} onChange={(e) => { setMinimum(e.target.value); invalidate(); }} /></label>
      <label>分段<select value={partMode} onChange={(e) => { setPartMode(e.target.value); invalidate(); }}><option value="auto">自动</option><option value="set">明确指定</option><option value="clear">明确清空</option></select></label>
      {partMode === "set" && <label>分段值<input value={part} maxLength={32} onChange={(e) => { setPart(e.target.value); invalidate(); }} /></label>}
      {format && kind!=="custom" && <label><input type="checkbox" checked={parentConsent} onChange={(e) => { invalidate(); setParentConsent(e.target.checked); }} />若此记录是单个媒体文件，我明确同意将模板范围扩展到它的父目录；若是目录记录请勿勾选。</label>}
      <button type="button" onClick={() => void operate(false)}>生成关联记录只读预览</button>
    </fieldset>
    {review && <div>
      <p>{kind==="custom"?`自定义原来源：${source}`:`审核记录 #${review.origin.recordId}：${review.unknown?.path || `${review.history?.sourcePath}/${review.history?.sourceFilename}`}`} · 原方式 {review.unknown?.mode || review.history?.mode || "未指定"} → 本次 {review.plan.mode}</p>
      <p>来源根：<code>{review.sourceRoot}</code><br />目标根：<code>{review.targetRoot}</code>。{review.options?.expandedScope ? "本次已扩展到原文件的父目录。" : "未扩展来源范围。"}{review.options?.preserveUnknown ? "仅选中范围执行；旧未识别记录保持原状，不标记整条成功。" : review.history ? "原历史和已发布媒体不变；新的转移成功后另记历史。" : kind==="custom"?"自定义本次范围不关联或修改旧记录。":"关联未识别记录仅在全范围成功和完整回执后更新。"}</p>
      <p>{review.explicitTarget ? "本次使用原记录明确指定的配置目标根，不按媒体类型另选。" : "本次按已核验媒体类型选择配置媒体库。"}</p>
      {!!review.plan.partitions?.length&&<div><p>多目标批次：全部子任务及完整来源核验后才提交旧记录；部分完成不会标整条成功。</p><ul>{review.plan.partitions.map(part=><li key={part.targetId}>{part.targetRoot}<ul>{part.items.map(item=><li key={item.source}>{item.source} → {item.target}</li>)}</ul></li>)}</ul></div>}
      <ul>{review.plan.items.map((item) => <li key={item.source}>{item.source} → {item.target || "不执行"} · {item.mediaType || "未核验类型"} · {item.category || "无分类"} · {item.status}</li>)}</ul>
      <label><input type="checkbox" checked={confirmed} disabled={!enabled || busy} onChange={(e) => setConfirmed(e.target.checked)} />我已审核本次范围、身份规则和路径，确认来源已下载完成且至少 30 秒未修改，同意执行以上可规划项。</label>
      {stoppedRequired&&<><label><input type="checkbox" checked={oldStopped} disabled={!enabled||busy} onChange={e=>setOldStopped(e.target.checked)}/>我已实际停止旧执行器及会写本轮来源／远程范围的外部程序。</label>{remoteRequired&&<RemoteTransferConsent busy={!enabled||busy} exclusive={remoteExclusive} recovery={remoteRecovery} onExclusive={setRemoteExclusive} onRecovery={setRemoteRecovery}/>}</>}
      {moves && <label><input type="checkbox" checked={sourceRemoval} disabled={!enabled || busy} onChange={(e) => setSourceRemoval(e.target.checked)} />我另外同意移除上述来源中的原文件名，并在目标核验及历史提交后清理本任务源恢复备份；不删除源目录或其他文件。</label>}
      <button type="button" disabled={!enabled || busy || !confirmed || moves && !sourceRemoval || remoteRequired&&(!remoteExclusive||!remoteRecovery)||stoppedRequired&&!oldStopped} onClick={() => void operate(true)}>按已审核范围确认执行</button>
    </div>}
  </section>;
}
