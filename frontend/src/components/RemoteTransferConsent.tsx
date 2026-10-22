import type { OrganizationRemoteTarget } from "../api/client";

type Props={targets?:OrganizationRemoteTarget[];busy:boolean;exclusive:boolean;recovery:boolean;onExclusive:(value:boolean)=>void;onRecovery:(value:boolean)=>void;automatic?:boolean};
export function RemoteTransferConsent({targets,busy,exclusive,recovery,onExclusive,onRecovery,automatic=false}:Props){
  return <fieldset disabled={busy} aria-label="远程整理独立授权"><legend>{automatic?"本轮远程后台授权":"本次远程整理授权"}</legend>
    <p>NASTOOL 下的目标是远程逻辑路径，不是同名本机目录。发布期间可能看到不完整文件；本程序无法约束其他实例或外部写入器。完整私有恢复副本和元数据暂存长期保留，增加空间、读取流量和费用，不自动清理。停止旧执行器的确认须包含所有会修改本轮来源及远程命名空间的外部程序。</p>
    {!!targets?.length && <ul>{targets.map(target=><li key={`${target.remote.driver}:${target.targetId}`}>{target.remote.driver==="minio"?"MinIO":"Rclone"} · {target.path} · {target.remote.directories?"可保留空目录（对象存储使用目录标记）":"空目录只保存在私有清单中"}</li>)}</ul>}
    <label><input type="checkbox" checked={exclusive} onChange={e=>onExclusive(e.target.checked)}/>我确认本程序独占审核范围的远程命名空间，接受非原子可见性；这不授权覆盖已有或未知对象。</label>
    <label><input type="checkbox" checked={recovery} onChange={e=>onRecovery(e.target.checked)}/>我同意{automatic?"本轮授权产生的每个任务":"本次任务"}保留完整恢复副本与元数据暂存并承担费用；不授权删除它们，不将不确定响应当成功。</label>
  </fieldset>;
}
