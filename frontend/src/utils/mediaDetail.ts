import type { MediaDetailKind, MediaDisplayDetail } from "../api/client";
export function mediaDetailPath(type: string, id: string): string | null {
  const label = type.trim().toLowerCase(); const kind: MediaDetailKind | null = ["movie", "mov", "电影"].includes(label) ? "movie" : ["tv", "电视剧", "剧集", "anime", "动漫"].includes(label) ? "tv" : null;
  if (!kind || !/^(?:[1-9]\d{0,18}|DB:[1-9]\d{0,18}|BG:[1-9]\d{0,18})$/.test(id) || kind === "movie" && id.startsWith("BG:")) return null;
  return `/media/${kind}/${encodeURIComponent(id)}`;
}
export function parseMediaDetailPath(path: string): { kind: MediaDetailKind; id: string } | null {
  const match = /^\/media\/(movie|tv)\/([^/]+)$/.exec(path); if (!match) return null;
  try { const kind = match[1] as MediaDetailKind; const id = decodeURIComponent(match[2]); return mediaDetailPath(kind, id) ? { kind, id } : null; } catch { return null; }
}
export function isMediaSeasonSubscribed(detail: MediaDisplayDetail, season?: number): boolean {
  if (!detail.subscriptionsKnown) return false;
  if (detail.kind === "movie") return detail.subscribed;
  if (season === undefined) return false;
  return detail.subscriptions.some((ref) => { const raw = ref.season.trim().replace(/^S/i, ""); return /^\d+$/.test(raw) && Number(raw) === season; });
}
