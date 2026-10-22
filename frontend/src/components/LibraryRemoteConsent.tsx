type Props={busy:boolean;exclusive:boolean;recovery:boolean;onExclusive:(value:boolean)=>void;onRecovery:(value:boolean)=>void};

export function LibraryRemoteConsent({busy,exclusive,recovery,onExclusive,onRecovery}:Props){
  return <fieldset disabled={busy} aria-label="远程已有库刮削独立授权"><legend>本轮远程读取与元数据写入授权</legend>
    <p>这是原任务账户下的远程逻辑路径，不是同名本机目录。审核固定对象清单，后台先完整读取媒体并持久留证，再处理 NFO／图片；不会下载第二份视频到本机。远程发布／恢复不具本地交换的原子性，发布期间可能看到不完整内容。原件、新件与清单长期保留，会增加云端空间和读取流量，不自动清理。此确认不替代所选模式的覆盖授权，不授权删除视频或恢复副本。</p>
    <label><input type="checkbox" checked={exclusive} onChange={e=>onExclusive(e.target.checked)}/>我确认独占本轮远程命名空间，已实际停止其他实例及全部外部写入器，接受非原子可见性</label>
    <label><input type="checkbox" checked={recovery} onChange={e=>onRecovery(e.target.checked)}/>我同意完整内容读取及长期保留原件／新件副本的费用；不确定结果须先核验，不自动重放覆盖</label>
  </fieldset>;
}
