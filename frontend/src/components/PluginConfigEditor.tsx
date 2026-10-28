import { useEffect, useMemo, useRef, useState } from "react";
import { ApiError, getPluginConfig, savePluginConfig, type AuthSession, type PluginConfigDetail, type PluginConfigField, type PluginSummary } from "../api/client";

type PluginConfigEditorProps = {
  session: AuthSession;
  plugin: PluginSummary;
  onClose: () => void;
  onSaved: () => void;
  onSessionExpired: () => void;
};

type EditorState = { kind: "loading" } | { kind: "ready"; detail: PluginConfigDetail } | { kind: "error"; message: string };
type FieldValue = string | boolean | string[];

export function PluginConfigEditor({ session, plugin, onClose, onSaved, onSessionExpired }: PluginConfigEditorProps) {
  const [state, setState] = useState<EditorState>({ kind: "loading" });
  const [values, setValues] = useState<Record<string, FieldValue>>({});
  const [secrets, setSecrets] = useState<Record<string, string>>({});
  const [clearConfig, setClearConfig] = useState<Set<string>>(new Set());
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const generation=useRef(0),inFlight=useRef(false);

  async function load() {
    const version=++generation.current;inFlight.current=false;setSaving(false);setValues({});setSecrets({});setClearConfig(new Set());setError("");
    setState({ kind: "loading" });
    try {
      const detail = await getPluginConfig(session.token, plugin.id);
      if(version!==generation.current)return;
      setValues(detail.values);
      setState({ kind: "ready", detail });
    } catch (loadError) {
      if(version!==generation.current)return;
      if (loadError instanceof ApiError && loadError.code === 401) return onSessionExpired();
      setState({ kind: "error", message: loadError instanceof Error ? loadError.message : "插件配置加载失败" });
    }
  }

  useEffect(() => { void load();return()=>{generation.current++;}; }, [plugin.id, session.token]);

  const sections = useMemo(() => {
    if (state.kind !== "ready") return [];
    const groups = new Map<string, PluginConfigField[]>();
    for (const field of state.detail.fields) {
      const section = field.section || "基础设置";
      groups.set(section, [...(groups.get(section) || []), field]);
    }
    return Array.from(groups.entries());
  }, [state]);

  function setValue(key: string, value: FieldValue) {
    setValues((current) => ({ ...current, [key]: value }));
  }

  function setSecret(key: string, value: string) {
    setSecrets((current) => ({ ...current, [key]: value }));
    if (value) setClearConfig((current) => { const next = new Set(current); next.delete(key); return next; });
  }

  function toggleClear(key: string, checked: boolean) {
    setClearConfig((current) => { const next = new Set(current); if (checked) next.add(key); else next.delete(key); return next; });
    if (checked) setSecrets((current) => ({ ...current, [key]: "" }));
  }

  function toggleMulti(key: string, option: string, checked: boolean) {
    const current = Array.isArray(values[key]) ? values[key] as string[] : [];
    setValue(key, checked ? [...current, option] : current.filter((item) => item !== option));
  }

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (state.kind !== "ready" || inFlight.current) return;
    setError("");
    const nextValues: Record<string, FieldValue> = { ...values };
    for (const field of state.detail.fields) {
      if (!field.writeOnly || field.readOnly) continue;
      const replacement = secrets[field.key] || "";
      if (replacement) nextValues[field.key] = replacement;
      if (field.required && ((!field.configured && !replacement) || clearConfig.has(field.key))) {
        setError(`“${field.title}”为必填项，请输入新值。`);
        return;
      }
    }
    setSaving(true);
    inFlight.current=true;const version=generation.current;
    try {
      await savePluginConfig(session.token, plugin.id, { values: nextValues, clearConfig: Array.from(clearConfig), ...(state.detail.meta.fingerprint?{fingerprint:state.detail.meta.fingerprint}:{}) });
      if(version!==generation.current)return;
      onSaved();
    } catch (saveError) {
      if(version!==generation.current)return;
      if (saveError instanceof ApiError && saveError.code === 401) return onSessionExpired();
      setError(saveError instanceof Error ? saveError.message : "插件配置保存失败");
      if(plugin.id==="LibraryScraper")setValues(current=>({...current,go_confirm:false,go_old_stopped:false,go_overwrite:false,go_stop:false,onlyonce:false}));
      if(plugin.id==="LibraryRefresh")setValues(current=>({...current,go_confirm:false,go_old_stopped:false,go_scan:false,...("go_plex_login" in current?{go_plex_login:false,go_plex_http:false}:{})}));
      if(plugin.id==="Webhook")setValues(current=>({...current,go_confirm:false,go_payload:false,go_old_stopped:false,go_http:false}));
    } finally {
      if(version===generation.current){inFlight.current=false;setSaving(false);}
    }
  }

  return <div className="editor-backdrop" role="presentation"><form className="service-editor plugin-config-editor" aria-label={`配置插件 ${plugin.name}`} onSubmit={submit}>
    <header><div><p className="eyebrow">PLUGIN CONFIGURATION</p><h2>{plugin.name}</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    {state.kind === "loading" && <div className="plugin-config-loading"><span /><span /><span /></div>}
    {state.kind === "error" && <div className="inline-empty"><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></div>}
    {state.kind === "ready" && state.detail.fields.length === 0 && <div className="inline-empty">这个插件不需要配置。</div>}
    {state.kind === "ready" && sections.map(([section, fields]) => <fieldset disabled={saving} className="plugin-config-section" key={section}><legend>{section}</legend><div className="plugin-config-grid">{fields.map((field) => <PluginField key={field.key} field={field} value={values[field.key]} secret={secrets[field.key] || ""} clearing={clearConfig.has(field.key)} onValue={(value) => setValue(field.key, value)} onSecret={(value) => setSecret(field.key, value)} onClear={(checked) => toggleClear(field.key, checked)} onMulti={(option, checked) => toggleMulti(field.key, option, checked)} />)}</div></fieldset>)}
    {state.kind==="ready" && plugin.id==="LibraryScraper" && <p className="editor-note">周期授权会扫描所选媒体库未来新增的文件；强制模式会保留占磁盘的原文件备份，备份不自动删除。保存或停用将停止旧任务后续写入。响应丢失不自动重试，请先只读刷新核查。<button type="button" disabled={saving} onClick={()=>void load()}>只读刷新并重置确认</button></p>}
    {state.kind==="ready"&&plugin.id==="LibraryRefresh"&&<p className="editor-note">启用会授权当前媒体服务器接收未来的新原生入库事件，可能回退整库扫描并改变服务器元数据；不接管历史待办。所有确认每次重新勾选，停用不能撤回已经接收的扫描。响应丢失请先只读刷新，不要重新提交。<button type="button" disabled={saving} onClick={()=>void load()}>只读刷新并重置确认</button></p>}
    {state.kind === "ready" && <p className="editor-note">文本、路径、地址和凭据均按只写字段处理：已保存内容不会回显；留空表示保留原值。插件提供的动态脚本和事件处理器不会在此页面执行。</p>}
    {state.kind==="ready"&&plugin.id==="Webhook"&&<p className="editor-note">此插件会向指定地址发送事件数据；原始媒体回调可能含路径、站点链接、用户和私有字段。每次启用需重新确认；关闭或卸载阻止尚未发送的待办，不能撤回已接收的操作。响应丢失请先只读核对配置与原回执。<button type="button" disabled={saving} onClick={()=>void load()}>只读刷新并重置确认</button></p>}
    {state.kind==="ready"&&plugin.id==="SpeedLimiter"&&<p className="editor-note">保存配置会停止旧原生限速授权；原全局速度和回执保留。请在插件列表下方“播放联动下载器限速”中先只读核对当前会话和速度，再单独启用。旧安装或旧设置不会自动启动后台。<button type="button" disabled={saving} onClick={()=>void load()}>只读刷新原设置</button></p>}
    {state.kind==="ready"&&plugin.id==="MediaSyncDel"&&<p className="editor-note">保存会停止旧原生同步删除授权，保留已发生的操作与回执。在列表下方“Emby 同步删除管理”独立审核未来历史、去重与源范围；原开关不会自动启动删除。缺少虚拟媒体标志会停用原开关，可只读刷新后核对。<button type="button" disabled={saving} onClick={()=>void load()}>只读刷新原同步删除设置</button></p>}
    {error && <p className="form-error plugin-config-error" role="alert">{error}</p>}
    <footer><button type="button" className="secondary-button" onClick={onClose}>取消</button><button type="submit" className="primary-button" disabled={saving || state.kind !== "ready" || state.detail.fields.length === 0}>{saving ? "保存并重载中…" : "保存并重载插件"}</button></footer>
  </form></div>;
}

function PluginField({ field, value, secret, clearing, onValue, onSecret, onClear, onMulti }: { field: PluginConfigField; value?: FieldValue; secret: string; clearing: boolean; onValue: (value: FieldValue) => void; onSecret: (value: string) => void; onClear: (checked: boolean) => void; onMulti: (option: string, checked: boolean) => void }) {
  const heading = <span>{field.title}{field.required && <b> *</b>}{field.writeOnly && field.configured && <em>已配置</em>}{field.readOnly && <em>只读</em>}</span>;
  if (field.readOnly) return <div className="plugin-config-field is-readonly"><label>{heading}<span className="plugin-readonly-value">服务器保留的只读内容不会显示</span></label>{field.tooltip && <small>{field.tooltip}</small>}</div>;
  if (field.type === "switch") return <div className="plugin-config-field plugin-switch-field"><label>{heading}<input type="checkbox" checked={Boolean(value)} onChange={(event) => onValue(event.target.checked)} /></label>{field.tooltip && <small>{field.tooltip}</small>}</div>;
  if (field.type === "select") return <div className="plugin-config-field"><label>{heading}<select value={typeof value === "string" ? value : ""} onChange={(event) => onValue(event.target.value)}>{field.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>{field.tooltip && <small>{field.tooltip}</small>}</div>;
  if (field.type === "multiselect") {
    const selected = Array.isArray(value) ? value : [];
    return <div className="plugin-config-field plugin-multiselect"><span>{heading}</span><div>{field.options.map((option) => <label key={option.value}><input type="checkbox" checked={selected.includes(option.value)} onChange={(event) => onMulti(option.value, event.target.checked)} /><span>{option.label}</span></label>)}</div>{field.tooltip && <small>{field.tooltip}</small>}</div>;
  }
  const input = field.type === "textarea" ? <textarea rows={4} value={secret} disabled={clearing} placeholder={field.configured ? "留空以保留已保存内容" : field.placeholder} onChange={(event) => onSecret(event.target.value)} /> : <input type="password" value={secret} disabled={clearing} autoComplete="new-password" placeholder={field.configured ? "留空以保留已保存内容" : field.placeholder} onChange={(event) => onSecret(event.target.value)} />;
  return <div className="plugin-config-field"><label>{heading}{input}</label>{field.tooltip && <small>{field.tooltip}</small>}{field.configured && <label className="plugin-clear-field"><input type="checkbox" checked={clearing} onChange={(event) => onClear(event.target.checked)} />清除服务器上保存的内容</label>}</div>;
}
