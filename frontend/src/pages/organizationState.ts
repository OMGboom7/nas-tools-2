// Client-side input checks only; the server matches the actual scan, filters
// before offset, and verifies identity and real season bounds before planning.
function hasEpisodeTemplateField(format: string): boolean {
  for (let i = 0; i < format.length; i++) {
    if ((format[i] === "{" && format[i+1] === "{") || (format[i] === "}" && format[i+1] === "}")) { i++; continue; }
    if (format[i] !== "{") continue;
    const end = format.indexOf("}", i+1);
    if (end < 0) return false;
    if (format.slice(i+1, end).split(":", 1)[0] === "ep") return true;
    i = end;
  }
  return false;
}
export function buildOrganizationEpisodeSelection(id: string, season: string, format: string, start: string, end: string, offset: string): import("../api/client").OrganizationEpisodeSelection {
  const tmdbId = id.trim();
  if (!/^[1-9]\d{0,18}$/.test(tmdbId) || BigInt(tmdbId) > 9223372036854775807n) throw new Error("请输入有效的 TV TMDB ID。");
  const integer = (value: string, low: number, high: number) => {
    if (!/^\d+$/.test(value.trim())) throw new Error("请明确填写整数季号和成对筛选范围。");
    const n = Number(value);
    if (!Number.isSafeInteger(n) || n < low || n > high) throw new Error("批量季集选择超出范围。");
    return n;
  };
  if (!hasEpisodeTemplateField(format) || new TextEncoder().encode(format).length > 512) throw new Error("文件名模板须包含文字型 ep 字段，且不超过 512 字节。");
  const rule: import("../api/client").OrganizationEpisodeSelection = { tmdbId, season: integer(season, 0, 99), episodeFormat: format };
  if (start.trim() || end.trim()) {
    rule.startEpisode = integer(start, 1, 9999);
    rule.endEpisode = integer(end, rule.startEpisode, 9999);
  }
  if (offset.trim()) {
    if (!/^-?\d{1,5}$/.test(offset.trim())) throw new Error("偏移须为 -99999 至 99999 的整数。");
    rule.episodeOffset = Number(offset);
  }
  return rule;
}

// Eligibility only; the server still verifies saved proof, physical objects,
// original-source preservation, target absence and executor isolation.
export function canAbandonOrganizationJob(job: { state: string; items: Array<{ state: string }> }): boolean {
  return job.state === "abandoning" || (job.state === "needs_review"
    && job.items.some((item) => item.state === "prepared" || item.state === "staging")
    && job.items.every((item) => item.state === "planned" || item.state === "prepared" || item.state === "staging"));
}

export function buildOrganizationAssignment(file: { source: string; kind: string; identity: string; modified: string; size: number }, kind: "movie" | "tv" | "anime", id: string, season: string, episode: string, endEpisode: string, episodeFormat = "", episodeOffset = "", partMode: "auto" | "set" | "clear" = "auto", part = "") {
  const tmdbId = id.trim();
  if (file.kind !== "media" || !file.identity || !file.modified || !Number.isSafeInteger(file.size) || file.size < 0) throw new Error("请从当前预览选择媒体文件；文件快照缺失时请重新预览。");
  if (!/^[1-9]\d{0,18}$/.test(tmdbId) || BigInt(tmdbId) > 9223372036854775807n) throw new Error("请输入有效的 TMDB 数字 ID。");
  const assignment: import("../api/client").OrganizationMediaAssignment = { source: file.source, identity: file.identity, modified: file.modified, size: file.size, kind, tmdbId };
  if (partMode === "clear") assignment.partOverride = "";
  else if (partMode === "set") {
    const normalized = part.trim().toUpperCase();
    if (!/^(?:PART(?:[0-9ABI]{0,2}|C|III)|(?:CD|DVD|DISK|DISC)[0-9]{0,2})$/.test(normalized)) throw new Error("请输入有效分段，如 PART1、PARTIII、CD01、DVD2、DISK10 或 DISC12。");
    assignment.partOverride = normalized;
  }
  if (kind !== "movie") {
    const parse = (value: string, minimum: number, maximum: number) => {
      if (!/^\d+$/.test(value.trim())) throw new Error("电视剧必须明确指定季号和集号。");
      const number = Number(value);
      if (!Number.isSafeInteger(number) || number < minimum || number > maximum) throw new Error("季集编号超出支持范围。");
      return number;
    };
    assignment.season = parse(season, 0, 99);
    if (episodeFormat) {
      if (new TextEncoder().encode(episodeFormat).length > 512 || !hasEpisodeTemplateField(episodeFormat)) throw new Error("文件名模板须包含文字型 ep 字段，且不超过 512 字节。");
      assignment.episodeFormat = episodeFormat;
    } else {
      assignment.episode = parse(episode, 1, 9999);
      if (endEpisode.trim()) assignment.endEpisode = parse(endEpisode, assignment.episode, 9999);
    }
    if (episodeOffset.trim()) {
      if (!/^-?\d{1,5}$/.test(episodeOffset.trim())) throw new Error("集号偏移须为 -99999 至 99999 的整数。");
      assignment.episodeOffset = Number(episodeOffset);
    }
  }
  return assignment;
}
