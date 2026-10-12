// Eligibility only; the server still verifies saved proof, physical objects,
// original-source preservation, target absence and executor isolation.
export function canAbandonOrganizationJob(job: { state: string; items: Array<{ state: string }> }): boolean {
  return job.state === "abandoning" || (job.state === "needs_review"
    && job.items.some((item) => item.state === "prepared" || item.state === "staging")
    && job.items.every((item) => item.state === "planned" || item.state === "prepared" || item.state === "staging"));
}

export function buildOrganizationAssignment(file: { source: string; kind: string; identity: string; modified: string; size: number }, kind: "movie" | "tv", id: string, season: string, episode: string, endEpisode: string, episodeFormat = "", episodeOffset = "") {
  const tmdbId = id.trim();
  if (file.kind !== "media" || !file.identity || !file.modified || !Number.isSafeInteger(file.size) || file.size < 0) throw new Error("请从当前预览选择媒体文件；文件快照缺失时请重新预览。");
  if (!/^[1-9]\d{0,18}$/.test(tmdbId) || BigInt(tmdbId) > 9223372036854775807n) throw new Error("请输入有效的 TMDB 数字 ID。");
  const assignment: import("../api/client").OrganizationMediaAssignment = { source: file.source, identity: file.identity, modified: file.modified, size: file.size, kind, tmdbId };
  if (kind === "tv") {
    const parse = (value: string, minimum: number, maximum: number) => {
      if (!/^\d+$/.test(value.trim())) throw new Error("电视剧必须明确指定季号和集号。");
      const number = Number(value);
      if (!Number.isSafeInteger(number) || number < minimum || number > maximum) throw new Error("季集编号超出支持范围。");
      return number;
    };
    assignment.season = parse(season, 0, 99);
    if (episodeFormat) {
      if (episodeFormat.length > 512 || !episodeFormat.includes("{ep}")) throw new Error("文件名模板须包含 {ep}，且不超过 512 字符。");
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
