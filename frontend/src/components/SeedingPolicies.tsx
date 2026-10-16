import { useEffect, useRef, useState } from "react";
import { ApiError, bindSeedingPolicy, getSeedingEvidence, reviewSeedingPolicy, type SeedingPolicyReview, type SeedingPolicyStatus } from "../api/client";

export function SeedingPolicies({ token, resourceId, onSessionExpired }: { token: string; resourceId?: string; onSessionExpired: () => void }) {
  const [downloader, setDownloader] = useState("");
  const [hash, setHash] = useState("");
  const [status, setStatus] = useState<SeedingPolicyStatus | null>(null);
  const [review, setReview] = useState<SeedingPolicyReview | null>(null);
  const [confirm, setConfirm] = useState(false);
  const [oldStopped, setOldStopped] = useState(false);
  const [hold, setHold] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const generation = useRef(0);
  const inFlight = useRef(false);
  function resetReview() { setReview(null); setConfirm(false); setOldStopped(false); setHold(false); }
  useEffect(() => {
    generation.current++; inFlight.current = false; setBusy(false); setStatus(null); resetReview(); setMessage("");
    return () => { generation.current++; };
  }, [token, resourceId]);

  async function operate(action: "read" | "review" | "bind") {
    if (inFlight.current || action === "bind" && (!review || !confirm || !oldStopped || review.needsUnknownHold && !hold)) return;
    const current = ++generation.current;
    inFlight.current = true; setBusy(true); setMessage("");
    const scope = { downloaderId: Number(downloader), taskId: hash.trim().toLowerCase() };
    try {
      if (action === "review") {
        const result = await reviewSeedingPolicy(token, { ...scope, resourceId: resourceId! });
        if (current === generation.current) { resetReview(); setStatus(null); setReview(result); }
      } else {
        if (action === "bind") await bindSeedingPolicy(token, { ...scope, resourceId: resourceId! }, review!, confirm, oldStopped, hold);
        const result = await getSeedingEvidence(token, scope);
        if (current === generation.current) { resetReview(); setStatus(result); if (action === "bind") setMessage("已登记约束，仅影响后续受保护操作；没有添加、停止或删除下载任务。"); }
      }
    } catch (err) {
      if (current !== generation.current) return;
      resetReview(); setStatus(null);
      if (err instanceof ApiError && err.code === 401) onSessionExpired();
      else setMessage(err instanceof Error ? err.message : "无法核实做种约束，请只读刷新原任务");
      if (action === "bind") {
        // A lost response is not permission to append again. Read only.
        try { const result = await getSeedingEvidence(token, scope); if (current === generation.current) setStatus(result); } catch { /* no mutation replay */ }
      }
    } finally { if (current === generation.current) { inFlight.current = false; setBusy(false); } }
  }

  const evidence = review?.evidence ?? status?.evidence;
  const policies = review?.policies ?? status?.policies ?? [];
  const decision = review?.decision ?? status?.decision;
  return <section aria-label="PT 做种约束"><h2>PT 做种约束（管理员）</h2>
    <p>只保护本版本 Go 的停止、删除和下载任务移动整理，不控制下载器自动停止或其他客户端。未登记不代表没有 PT 义务。HR 未核实会持续拦截，原约束不删改；可另行审核限时人工核实声明。</p>
    <fieldset disabled={busy}><label>准确的下载器 ID<input inputMode="numeric" value={downloader} onChange={(e) => { setDownloader(e.target.value); setStatus(null); resetReview(); }} /></label>
      <label>现有任务完整 hash<input maxLength={64} value={hash} onChange={(e) => { setHash(e.target.value); setStatus(null); resetReview(); }} /></label>
      <button type="button" onClick={() => void operate("read")}>只读核对原任务及已登记约束</button>
      {resourceId && <button type="button" onClick={() => void operate("review")}>只读审核此搜索资源的约束接管</button>}
    </fieldset>
    {message && <p role="status">{message}</p>}
    {status && !status.registered && <p>此任务未登记约束，这不是“无需做种”的证明。</p>}
    {evidence && <p>下载器当前报告：{evidence.state}；完整下载：{evidence.complete === null ? "未知" : evidence.complete ? "是" : "否"}；累计做种 {evidence.seconds ?? "未知"} 秒；分享率 {evidence.ratio ?? "未知"}。不使用下载完成后的墙上时间代替累计做种时间。</p>}
    <ul>{policies.map((p) => <li key={p.id}>{p.title} · {p.source} · 最低 {p.requirements.seconds ?? "未知"} 秒 / 分享率 {p.requirements.ratio ?? "未知"} · HR {p.requirements.hr === null ? "未知" : p.requirements.hr ? "已标记，解除未核实" : "无标记证据"} · {p.created}</li>)}</ul>
    {decision && <p>{decision.allowed ? "当前已登记约束检查未拦截（执行时将重新检查）" : `当前拦截：${decision.reasons.join("；")}`}</p>}
    {status?.actions && <ul>{(["stop", "remove", "move"] as const).map((action) => <li key={action}>{({ stop: "停止", remove: "删除", move: "移动" })[action]}：{status.actions![action].allowed ? "当前范围检查未拦截，执行时重查" : "当前仍拦截"}{status.actions![action].basis === "operator-attestation" ? "（依据管理员核实，不是站点自动验证）" : ""}</li>)}</ul>}
    {review && <fieldset disabled={busy}><legend>登记前审核，不会向下载器发送写入</legend><p>{review.policy.downloaderName} · {review.policy.infoHash} · {review.policy.title} · 最低 {review.policy.requirements.seconds ?? "未知"} 秒 / 分享率 {review.policy.requirements.ratio ?? "未知"}。{review.warning}</p>
      <label><input type="checkbox" checked={confirm} onChange={(e) => setConfirm(e.target.checked)} />确认将此源资源的不可降低约束绑定到该准确任务</label>
      <label><input type="checkbox" checked={oldStopped} onChange={(e) => setOldStopped(e.target.checked)} />已实际停止旧 Go / Python 执行器，知晓外部客户端不受此锁保护</label>
      {review.needsUnknownHold && <label><input type="checkbox" checked={hold} onChange={(e) => setHold(e.target.checked)} />另行确认未知或 HR 约束会持续阻止停止、删除、移动；限时人工核实须另行审核，不会删除原约束</label>}
      <button type="button" disabled={!confirm || !oldStopped || review.needsUnknownHold && !hold} onClick={() => void operate("bind")}>确认仅登记不可降低的约束</button>
    </fieldset>}
  </section>;
}
