import { useEffect, useRef, useState } from "react";
import {
  ApiError, addIyuuSeed, getIyuuSeeds, bindIyuuSite, getIyuuOverview, getIyuuPartners, matchIyuuTorrents,
  type IyuuCandidate, type IyuuSeed, type IyuuMatches, type IyuuOverview, type IyuuPartner, type IyuuReceipt,
} from "../api/client";

const states: Record<string, string> = {
  bound: "绑定成功", rejected: "绑定被拒绝，请核对账户资料",
  pending: "绑定已登记，结果待核实", unknown: "未收到明确的绑定结果，请先向 IYUU 核实",
};

const seedStates: Record<string, string> = {
  unsent: "尚未发送，可重新匹配并提交", adding: "正在提交", added: "已暂停添加，等待核对", bound: "已记录，等待校验",
  verify_pending: "校验请求待核实", verifying: "等待文件校验完成", verified: "校验通过",
  profile_pending: "做种设置待核实", prepared: "等待开始做种", start_pending: "启动请求待核实",
  started: "已请求启动", seeding: "已确认做种", duplicate: "下载器已有此任务", unknown: "远端结果待核实",
};

export function IyuuPluginPage({ token, onClose, onSessionExpired }: {
  token: string; onClose: () => void; onSessionExpired: () => void;
}) {
  const [overview, setOverview] = useState<IyuuOverview>();
  const [partners, setPartners] = useState<IyuuPartner[]>();
  const [sid, setSid] = useState("");
  const [uid, setUid] = useState("");
  const [passkey, setPasskey] = useState("");
  const [receipt, setReceipt] = useState<IyuuReceipt>();
  const [matches, setMatches] = useState<IyuuMatches>();
  const [seeds, setSeeds] = useState<IyuuSeed[]>([]);
  const [error, setError] = useState("");
  const [working, setWorking] = useState(false);
  const busy = useRef(false);
  const [reload, setReload] = useState(0);

  function report(cause: unknown) {
    if (cause instanceof ApiError && cause.code === 401) onSessionExpired();
    else setError(cause instanceof Error ? cause.message : "请求失败，请重新加载");
  }
  useEffect(() => {
    let active = true;
    setOverview(undefined); setPartners(undefined); setMatches(undefined); setReceipt(undefined);
    setPasskey(""); setUid(""); setError("");
    getIyuuOverview(token).then(data => {
      if (active) { setOverview(data); setReceipt(data.latest); }
    }).catch(cause => { if (active) report(cause); });
    return () => { active = false; };
  }, [token, reload]);

  useEffect(() => {
    if (!overview) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    async function poll() {
      try {
        const rows = await getIyuuSeeds(token);
        if (active) setSeeds(rows);
      } catch (cause) { if (active) report(cause); }
      if (active) timer = setTimeout(poll, 5000);
    }
    void poll();
    return () => { active = false; clearTimeout(timer); };
  }, [token, overview]);

  async function submit(candidate: IyuuCandidate) {
    if (!overview) return;
    const result = await addIyuuSeed(token, overview.matchFingerprint, candidate);
    setSeeds(rows => [result, ...rows.filter(v => v.id !== result.id)]);
    setMatches(current => current && ({ ...current, items: current.items.filter(v =>
      v.downloaderId !== candidate.downloaderId || v.hash !== candidate.hash) }));
  }
  async function run(action: () => Promise<void>) {
    if (busy.current) return;
    busy.current = true; setWorking(true); setError("");
    try { await action(); } catch (cause) { report(cause); }
    finally { setWorking(false); busy.current = false; }
  }
  async function loadPartners() {
    const result = await getIyuuPartners(token);
    if (result.fingerprint !== overview?.fingerprint) throw new Error("配置已变化，请刷新");
    setPartners(result.items); setSid(result.items[0]?.sid || "");
  }
  async function bind() {
    const partner = partners?.find(p => p.sid === sid);
    if (!overview || !partner) return;
    const input = { fingerprint: overview.fingerprint, requestId: overview.requestId, sid, site: partner.site, uid, passkey };
    setPasskey(""); setPartners(undefined);
    setReceipt({ id: overview.requestId, site: partner.site, state: "pending" });
    setReceipt(await bindIyuuSite(token, input));
  }
  const used = !!overview && receipt?.id === overview.requestId;
  return <div className="editor-backdrop"><section className="plugin-page-dialog" role="dialog" aria-modal="true" aria-label="IYUU 自动辅种">
    <header><h2>IYUU 自动辅种</h2><button type="button" onClick={onClose} aria-label="关闭">×</button></header>
    {error && <p role="alert">{error}</p>}
    {overview ? <>
      <section><h3>站点绑定</h3>
        {receipt?.id && <p role="status">{receipt.site} · {states[receipt.state] || "结果待核实"}</p>}
        {!partners && !used && <button type="button" disabled={working} onClick={() => void run(loadPartners)}>读取合作站点</button>}
        {partners && (partners.length === 0 ? <p>当前没有可绑定的合作站点。</p> :
          <form onSubmit={event => { event.preventDefault(); void run(bind); }}>
            <fieldset disabled={working} className="plugin-config-grid">
              <label>合作站点<select value={sid} onChange={event => setSid(event.target.value)}>{partners.map(p => <option key={p.sid} value={p.sid}>{p.name || p.site}</option>)}</select></label>
              <label>站点用户 ID<input required inputMode="numeric" pattern="[1-9][0-9]*" autoComplete="off" value={uid} onChange={event => setUid(event.target.value)} /></label>
              <label>Passkey<input required type="password" autoComplete="off" value={passkey} onChange={event => setPasskey(event.target.value)} /></label>
              <button type="submit">绑定站点</button>
            </fieldset>
          </form>)}
      </section>
      <section><h3>匹配已完成的种子</h3>
        <p>{overview.downloaders.length ? `将向 IYUU 提交以下下载器中符合条件的种子哈希：${overview.downloaders.join("、")}。` : "请先在插件配置中选择辅种下载器。"}</p>
        <button type="button" disabled={working || !overview.downloaders.length} onClick={() => void run(async () => {
          setMatches(undefined); setMatches(await matchIyuuTorrents(token, overview.matchFingerprint));
        })}>开始匹配</button>
        {matches && <><p role="status">已查询 {matches.scanned} 个种子，找到 {matches.items.length} 个辅种候选。</p>
          {matches.items.length > 0 && <div className="plugin-page-table"><table><thead><tr><th>下载器</th><th>源任务</th><th>目标站点</th><th>种子编号</th><th>操作</th></tr></thead><tbody>
            {matches.items.map(v => <tr key={[v.downloaderId, v.source, v.siteId, v.torrentId, v.hash].join(":")}><td>{v.downloader}</td><td>{v.name}</td><td>{v.site}</td><td>{v.torrentId}</td><td><button type="button" disabled={working} onClick={() => void run(() => submit(v))}>暂停添加，校验后做种</button></td></tr>)}
          </tbody></table></div>}</>}
      </section>
      {seeds.length > 0 && <section><h3>最近辅种记录</h3><div className="plugin-page-table"><table>
        <thead><tr><th>源任务</th><th>下载器</th><th>站点</th><th>进度</th></tr></thead>
        <tbody>{seeds.map(v => <tr key={v.id}><td>{v.name}</td><td>{v.downloader}</td><td>{v.site}</td><td>{seedStates[v.state] || v.state}</td></tr>)}</tbody>
      </table></div></section>}
    </> : !error && <p role="status">正在读取插件记录…</p>}
    <footer><button type="button" disabled={working} onClick={() => setReload(v => v + 1)}>刷新</button><button type="button" onClick={onClose}>关闭</button></footer>
  </section></div>;
}
