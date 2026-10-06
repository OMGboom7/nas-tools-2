import { useEffect, useState } from "react";
import { ApiError, deletePluginPageRecord, getPluginPage, type AuthSession, type PluginPageData, type PluginPageDeleteConfirmation, type PluginSummary } from "../api/client";

type PluginPageViewerProps = {
  session: AuthSession;
  plugin: PluginSummary;
  onClose: () => void;
  onSessionExpired: () => void;
};

type ViewerState = { kind: "loading" } | { kind: "ready"; data: PluginPageData } | { kind: "error"; message: string };

export function PluginPageViewer({ session, plugin, onClose, onSessionExpired }: PluginPageViewerProps) {
  const [state, setState] = useState<ViewerState>({ kind: "loading" });
  const [pendingDelete, setPendingDelete] = useState<{ recordId: string; label: string; confirmation: PluginPageDeleteConfirmation }>();
  const [confirmationText, setConfirmationText] = useState("");
  const [deleting, setDeleting] = useState(false);
  const [notice, setNotice] = useState("");

  async function load() {
    setState({ kind: "loading" });
    try {
      setState({ kind: "ready", data: await getPluginPage(session.token, plugin.id) });
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setState({ kind: "error", message: error instanceof Error ? error.message : "插件扩展页加载失败" });
    }
  }

  useEffect(() => { void load(); }, [plugin.id, session.token]);

  async function confirmDelete() {
    if (!pendingDelete) return;
    if (pendingDelete.confirmation === "typeRecordId" && confirmationText !== pendingDelete.recordId) return;
    setDeleting(true);
    setNotice("");
    try {
      const deletesFile = pendingDelete.confirmation === "typeRecordId";
      await deletePluginPageRecord(session.token, plugin.id, pendingDelete.recordId, confirmationText);
      setPendingDelete(undefined);
      setConfirmationText("");
      await load();
      setNotice(deletesFile ? "归档文件已删除。" : "历史记录已删除。");
    } catch (error) {
      if (error instanceof ApiError && error.code === 401) return onSessionExpired();
      setNotice(`错误：${error instanceof Error ? error.message : "历史记录删除失败"}`);
    } finally {
      setDeleting(false);
    }
  }

  const hasSafeActions = state.kind === "ready" && state.data.canDeleteRecords;
  const deletesArchiveFile = state.kind === "ready" && state.data.deleteConfirmation === "typeRecordId";

  return <div className="editor-backdrop" role="presentation"><section className="service-editor plugin-page-viewer" aria-label={`${plugin.name} 扩展页`}>
    <header><div><p className="eyebrow">PLUGIN VIEW</p><h2>{state.kind === "ready" ? state.data.title : plugin.name}</h2></div><button type="button" aria-label="关闭" disabled={deleting} onClick={onClose}>×</button></header>
    {notice && <div className={notice.startsWith("错误：") ? "data-warning plugin-page-warning" : "data-success plugin-page-warning"} role="status">{notice}</div>}
    {state.kind === "loading" && <div className="plugin-page-loading"><span /><span /><span /></div>}
    {state.kind === "error" && <div className="inline-empty"><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></div>}
    {state.kind === "ready" && <>
      <div className="plugin-page-badges"><span>结构化内容</span><span>{hasSafeActions ? "专用安全操作" : "只读视图"}</span></div>
      {state.data.actionsOmitted && <div className="data-warning plugin-page-warning">{hasSafeActions ? (deletesArchiveFile ? "旧扩展页脚本不会载入；归档文件只能通过文件名白名单和完整名称确认后删除。" : "旧扩展页脚本不会载入；本页只提供经过白名单校验的历史删除操作。") : "旧扩展页包含操作按钮或脚本，本页仅展示数据；未迁移操作不会开放。"}</div>}
      {pendingDelete && <div className="plugin-page-confirm" role="alertdialog" aria-label={pendingDelete.confirmation === "typeRecordId" ? "确认删除归档文件" : "确认删除历史记录"}><div><strong>确认删除“{pendingDelete.label}”？</strong>{pendingDelete.confirmation === "typeRecordId" ? <><p>这会从磁盘永久删除该归档记录文件，但不会删除媒体库中的影片。请输入完整文件名确认：</p><input aria-label="输入完整归档文件名" value={confirmationText} onChange={(event) => setConfirmationText(event.target.value)} placeholder={pendingDelete.recordId} autoComplete="off" /></> : <p>这里只会删除插件历史记录，不会删除媒体文件或下载任务。</p>}</div><div><button className="secondary-button" type="button" disabled={deleting} onClick={() => { setPendingDelete(undefined); setConfirmationText(""); }}>取消</button><button className="danger-button" type="button" disabled={deleting || (pendingDelete.confirmation === "typeRecordId" && confirmationText !== pendingDelete.recordId)} onClick={() => void confirmDelete()}>{deleting ? "删除中…" : "确认删除"}</button></div></div>}
      {state.data.sections.length > 0 && <section className="plugin-page-sections" aria-label="扩展页说明">{state.data.sections.map((section, index) => <p key={`${index}-${section}`}>{section}</p>)}</section>}
      {state.data.tables.map((table, tableIndex) => {
        const tableHasActions = table.rowActions.some(Boolean);
        return <section className="plugin-page-table" key={tableIndex} aria-label={`数据表 ${tableIndex + 1}`}><div><table><thead><tr>{table.columns.map((column, index) => <th key={`${index}-${column}`}>{column}</th>)}{tableHasActions && <th>操作</th>}</tr></thead><tbody>{table.rows.length > 0 ? table.rows.map((row, rowIndex) => { const action = table.rowActions[rowIndex]; return <tr key={rowIndex}>{table.columns.map((_, cellIndex) => <td key={cellIndex}>{row[cellIndex] || "—"}</td>)}{tableHasActions && <td className="plugin-page-row-action">{action?.type === "delete" && <button type="button" disabled={deleting} onClick={() => { setNotice(""); setConfirmationText(""); setPendingDelete({ recordId: action.recordId, label: row.find(Boolean) || action.recordId, confirmation: action.confirmation }); }}>删除</button>}</td>}</tr>; }) : <tr><td colSpan={table.columns.length + (tableHasActions ? 1 : 0)}>暂无数据</td></tr>}</tbody></table></div><small>{table.rows.length} 条记录</small></section>;
      })}
      {state.data.sections.length === 0 && state.data.tables.length === 0 && <div className="inline-empty">这个扩展页目前没有可展示的数据。</div>}
    </>}
    <footer><button className="secondary-button" type="button" disabled={deleting} onClick={onClose}>关闭</button></footer>
  </section></div>;
}
