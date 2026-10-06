import type { SearchResource } from "../api/client";

export function seedingRequirements(resource: Pick<SearchResource, "minimumSeedTime" | "minimumRatio">): string[] {
  const { minimumSeedTime: seconds, minimumRatio: ratio } = resource;
  const timeKnown = typeof seconds === "number" && Number.isSafeInteger(seconds) && seconds >= 0;
  const ratioKnown = typeof ratio === "number" && Number.isFinite(ratio) && ratio >= 0;
  return [
    timeKnown ? `最低做种 ${formatSeedTime(seconds)}` : "做种时长未知",
    ratioKnown ? `最低分享率 ${ratio}` : "分享率要求未知",
  ];
}

function formatSeedTime(seconds: number): string {
  if (seconds === 0) return "0 秒";
  const units: [number, string][] = [[86400, "天"], [3600, "小时"], [60, "分"], [1, "秒"]];
  return units.flatMap(([size, label]) => {
    const count = Math.floor(seconds / size);
    seconds %= size;
    return count ? [`${count} ${label}`] : [];
  }).join(" ");
}
