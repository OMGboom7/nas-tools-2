import type { MediaCachePresence, MediaCacheSummary } from "../api/client";

export function mediaCacheLabel(cache: MediaCacheSummary | undefined, presence: MediaCachePresence | undefined, tv: boolean): string {
  if (!cache || cache.displayOnly !== true || cache.state !== "ready" || !/^[a-f0-9]{32}$/.test(cache.generation) || !presence || presence.state !== "present" && presence.state !== "not-recorded" || tv && (!Number.isInteger(presence.episodes) || presence.episodes < 0)) return "展示快照不可用（不代表缺失）";
  if (presence.state === "not-recorded") return "所选快照未记录（不代表当前缺失）";
  return tv ? `快照有剧集记录 · ${presence.episodes} 集（不代表全集）` : "快照有入库记录（非实时）";
}
