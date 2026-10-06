import { useEffect, useMemo, useState } from "react";
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

  async function load() {
    setState({ kind: "loading" });
    try {
      const detail = await getPluginConfig(session.token, plugin.id);
      setValues(detail.values);
      setState({ kind: "ready", detail });
    } catch (loadError) {
      if (loadError instanceof ApiError && loadError.code === 401) return onSessionExpired();
      setState({ kind: "error", message: loadError instanceof Error ? loadError.message : "插件配置加载失败" });
    }
  }

  useEffect(() => { void load(); }, [plugin.id, session.token]);

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
    if (state.kind !== "ready") return;
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
    try {
      await savePluginConfig(session.token, plugin.id, { values: nextValues, clearConfig: Array.from(clearConfig) });
      onSaved();
    } catch (saveError) {
      if (saveError instanceof ApiError && saveError.code === 401) return onSessionExpired();
      setError(saveError instanceof Error ? saveError.message : "插件配置保存失败");
    } finally {
      setSaving(false);
    }
  }

  return <div className="editor-backdrop" role="presentation"><form className="service-editor plugin-config-editor" aria-label={`配置插件 ${plugin.name}`} onSubmit={submit}>
    <header><div><p className="eyebrow">PLUGIN CONFIGURATION</p><h2>{plugin.name}</h2></div><button type="button" aria-label="关闭" onClick={onClose}>×</button></header>
    {state.kind === "loading" && <div className="plugin-config-loading"><span /><span /><span /></div>}
    {state.kind === "error" && <div className="inline-empty"><p>{state.message}</p><button className="secondary-button" type="button" onClick={() => void load()}>重新加载</button></div>}
    {state.kind === "ready" && state.detail.fields.length === 0 && <div className="inline-empty">这个插件不需要配置。</div>}
    {state.kind === "ready" && sections.map(([section, fields]) => <fieldset className="plugin-config-section" key={section}><legend>{section}</legend><div className="plugin-config-grid">{fields.map((field) => <PluginField key={field.key} field={field} value={values[field.key]} secret={secrets[field.key] || ""} clearing={clearConfig.has(field.key)} onValue={(value) => setValue(field.key, value)} onSecret={(value) => setSecret(field.key, value)} onClear={(checked) => toggleClear(field.key, checked)} onMulti={(option, checked) => toggleMulti(field.key, option, checked)} />)}</div></fieldset>)}
    {state.kind === "ready" && <p className="editor-note">文本、路径、地址和凭据均按只写字段处理：已保存内容不会回显；留空表示保留原值。插件提供的动态脚本和事件处理器不会在此页面执行。</p>}
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
