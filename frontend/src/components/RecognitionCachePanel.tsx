import { useState } from "react";
import {
  ApiError, changeRecognitionCache, clearRecognitionCache, listRecognitionCache,
  type RecognitionCachePage, type RecognitionCacheRow,
} from "../api/client";

type Action = { kind: "rename" | "delete"; row: RecognitionCacheRow } | { kind: "clear" };

export function RecognitionCachePanel({ token, onSessionExpired }: { token: string; onSessionExpired: () => void }) {
  const [data, setData] = useState<RecognitionCachePage>();
  const [input, setInput] = useState("");
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [action, setAction] = useState<Action>();
  const [title, setTitle] = useState("");

  async function load(term: string, next: number) {
    let result = await listRecognitionCache(token, term, next);
    const last = Math.max(1, Math.ceil(result.total / 25));
    if (next > last) {
      next = last;
      result = await listRecognitionCache(token, term, next);
    }
    setData(result);
    setPage(next);
    setSearch(term);
  }

  async function run(work: () => Promise<void>) {
    setBusy(true);
    setNotice("");
    try { await work(); }
    catch (error) {
      if (error instanceof ApiError && error.code === 401) onSessionExpired();
      else setNotice(error instanceof Error ? error.message : "识别缓存请求失败");
    } finally { setBusy(false); }
  }

  async function save() {
    if (!action) return;
    if (action.kind === "clear") await clearRecognitionCache(token);
    else await changeRecognitionCache(token, action.kind, action.row, title);
    setAction(undefined);
    await load(search, page);
    setNotice("识别缓存已更新");
  }

  return <section className="service-card" aria-label="媒体识别缓存">
    <h2>媒体识别缓存</h2>
    <fieldset disabled={busy}>
      <form onSubmit={(event) => { event.preventDefault(); setAction(undefined); void run(() => load(input, 1)); }}>
        <label>原始名称 <input value={input} onChange={(event) => setInput(event.target.value)} /></label>{" "}
        <button type="submit" className="secondary-button">{data ? "查询" : "查看缓存"}</button>
      </form>
      {data && <>
        <p>共 {data.total} 条识别记录</p>
        {data.items.map((row) => <div className="service-toolbar" key={row.key}>
          <span>{row.key}</span><strong>{row.entry.title}</strong>
          <span>{[row.entry.type, row.entry.year, `TMDB ${row.entry.id}`].filter(Boolean).join(" · ")}</span>
          <button onClick={() => { setAction({ kind: "rename", row }); setTitle(row.entry.title ?? ""); }}>改名</button>
          <button onClick={() => setAction({ kind: "delete", row })}>删除</button>
        </div>)}
        <div className="service-toolbar">
          <button disabled={page <= 1} onClick={() => { setAction(undefined); void run(() => load(search, page - 1)); }}>上一页</button>
          <span>第 {page} 页</span>
          <button disabled={page * 25 >= data.total} onClick={() => { setAction(undefined); void run(() => load(search, page + 1)); }}>下一页</button>
          <button onClick={() => setAction({ kind: "clear" })}>清空全部缓存</button>
        </div>
      </>}
      {action && <form onSubmit={(event) => { event.preventDefault(); void run(save); }}>
        {action.kind === "clear" ? <p>清空所有识别记录？已保存的人工改名也会删除。</p> :
          <p>{action.kind === "delete" ? "删除这条识别记录？" : "修改识别标题"} {action.row.key}</p>}
        {action.kind === "rename" && <label>新标题 <input required value={title} onChange={(event) => setTitle(event.target.value)} /></label>}
        <button type="submit" disabled={action.kind === "rename" && !title.trim()}>确认</button>
        <button type="button" onClick={() => setAction(undefined)}>取消</button>
      </form>}
    </fieldset>
    {notice && <p role="status">{notice}</p>}
  </section>;
}
