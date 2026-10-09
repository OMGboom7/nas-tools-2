export type UserInfo = {
  userid: number | string;
  username: string;
  userpris: string[];
};

export type AuthSession = {
  token: string;
  user: UserInfo;
};

export type ResumeItem = {
  id: string | number;
  name: string;
  type: string;
  image?: string;
  link?: string;
  percent?: number;
};

export type DashboardData = {
  statistics: {
    movies: string;
    series: string;
    episodes: string;
    music: string;
    users: string;
  };
  storage: {
    total: string;
    used: string;
    free: string;
    usedPercent: number;
  };
  resume: ResumeItem[];
  latest: ResumeItem[];
  warnings: string[];
};

export type SearchResource = {
  id: string;
  season: string;
  name: string;
  description: string;
  site: string;
  pageUrl: string;
  size: string;
  seeders: number;
  resolution: string;
  medium: string;
  effect: string;
  releaseGroup: string;
  videoCodec: string;
  labels: string[];
  uploadFactor: number;
  downloadFactor: number;
  promotionKnown?: boolean;
  seedersKnown?: boolean;
  minimumSeedTime?: number;
  minimumRatio?: number;
  exists?: boolean;
  existsKnown?: boolean;
};

export type DownloadTask = {
  id: string;
  title: string;
  progress: number;
  speed: string;
  state: string;
  siteUrl: string;
  image: string;
  canControl: boolean;
  canRemove: boolean;
  showProgress: boolean;
};

export type DownloadHistory = {
  id: string;
  title: string;
  type: string;
  year: string;
  image: string;
  torrent: string;
  date: string;
  site: string;
};

export type DownloadsData = {
  active: DownloadTask[];
  history: DownloadHistory[];
  historyPage: number;
  warnings: string[];
};

export type SubscriptionItem = {
  id: string;
  name: string;
  year: string;
  type: "MOV" | "TV";
  season: string;
  tmdbId: string;
  image: string;
  overview: string;
  state: string;
  stateLabel: string;
  total: number;
  remaining: number;
  progress: number;
  rssSites: string[];
  searchSites: string[];
  overEdition: boolean;
  quality: string;
  resolution: string;
  releaseGroup: string;
  include: string;
  exclude: string;
  keyword: string;
  fuzzyMatch: boolean;
  filterRule: string;
  savePath: string;
  downloadSetting: string;
  totalEpisodes: number;
  currentEpisode: number;
  pendingSubmission?: boolean;
};

export type SubscriptionOptions = {
  rssSites: { value: string; label: string }[];
  searchSites: { value: string; label: string }[];
  filterRules: { value: string; label: string }[];
  downloadSettings: { value: string; label: string }[];
  savePaths: string[];
  warnings: string[];
};

export type SubscriptionInput = {
  id?: string; name: string; year: string; type: "MOV" | "TV"; season?: string; mediaId?: string;
  keyword?: string; fuzzyMatch: boolean; overEdition: boolean; rssSites: string[]; searchSites: string[];
  quality?: string; resolution?: string; releaseGroup?: string; filterRule?: string; include?: string;
  exclude?: string; savePath?: string; downloadSetting?: string; totalEpisodes?: number; currentEpisode?: number;
};

export type SubscriptionHistory = {
  id: string;
  name: string;
  year: string;
  type: "MOV" | "TV";
  season: string;
  tmdbId: string;
  image: string;
  overview: string;
  finishTime: string;
  total: number;
  start: number;
};

export type SubscriptionsData = {
  items: SubscriptionItem[];
  history: SubscriptionHistory[];
  warnings: string[];
};

export type DiscoveryMedia = {
  id: string;
  title: string;
  year: string;
  type: "MOV" | "TV";
  mediaType: string;
  vote: string;
  image: string;
  backdrop: string;
  overview: string;
  link: string;
  subscribed: boolean;
};

export type DiscoveryData = {
  category: string;
  page: number;
  items: DiscoveryMedia[];
};

export type SiteSummary = {
  id: string;
  name: string;
  priority: number;
  host: string;
  capabilities: string[];
  rssEnabled: boolean;
  brushEnabled: boolean;
  statisticEnabled: boolean;
  parseEnabled: boolean;
  messageEnabled: boolean;
  browserEnabled: boolean;
  proxyEnabled: boolean;
};

export type SitesData = {
  items: SiteSummary[];
};

export type SiteTestResult = {
  id: string;
  ok: boolean;
  duration: number;
  message: string;
};

export type SiteDetail = {
  id: string;
  name: string;
  priority: number;
  siteUrl: string;
  rssEnabled: boolean;
  brushEnabled: boolean;
  statisticEnabled: boolean;
  parseEnabled: boolean;
  messageEnabled: boolean;
  browserEnabled: boolean;
  proxyEnabled: boolean;
  subtitleEnabled: boolean;
  tags: string;
  filterRule: string;
  downloadSetting: string;
  limitInterval: string;
  limitCount: string;
  limitSeconds: string;
  rssConfigured: boolean;
  cookieConfigured: boolean;
  apiKeyConfigured: boolean;
  userAgentConfigured: boolean;
};

export type SiteInput = Omit<SiteDetail, "id" | "rssConfigured" | "cookieConfigured" | "apiKeyConfigured" | "userAgentConfigured"> & {
  rssUrl: string;
  cookie: string;
  apiKey: string;
  userAgent: string;
  clearRssUrl: boolean;
  clearCookie: boolean;
  clearApiKey: boolean;
  clearUserAgent: boolean;
};

export type SiteOptions = {
  filterRules: { value: string; label: string }[];
  downloadSettings: { value: string; label: string }[];
  warnings: string[];
};

export type ManagedService = {
  id: string;
  kind: "downloader" | "media" | "indexer";
  name: string;
  type: string;
  host: string;
  summary: string;
  configured: boolean;
  enabled: boolean;
  active: boolean;
  default: boolean;
  monitoring: boolean;
  canTest: boolean;
  sourceCount: number;
};

export type ServicesData = {
  downloaders: ManagedService[];
  mediaServers: ManagedService[];
  indexers: ManagedService[];
  warnings: string[];
};

export type ServiceTestResult = {
  kind: ManagedService["kind"];
  id: string;
  ok: boolean;
  duration: number;
  message: string;
};

export type DownloaderConfigDetail = {
  id: string;
  name: string;
  type: string;
  host: string;
  port: string;
  proxy: string;
  torrentManagement: string;
  rmtMode: string;
  enabled: boolean;
  transfer: boolean;
  onlyNastool: boolean;
  matchPath: boolean;
  default: boolean;
  usernameConfigured: boolean;
  passwordConfigured: boolean;
  secretConfigured: boolean;
  cookieConfigured: boolean;
  directoriesConfigured: boolean;
  directories: DownloadDirectory[];
};

export type DownloadDirectory = {
  type: "" | "电影" | "电视剧" | "动漫";
  category: string;
  savePath: string;
  containerPath: string;
  label: string;
};

export type DownloaderOptions = {
  categories: Record<string, string[]>;
  warnings: string[];
};

export type DownloaderConfigInput = Omit<DownloaderConfigDetail,
  "id" | "default" | "usernameConfigured" | "passwordConfigured" | "secretConfigured" | "cookieConfigured" | "directoriesConfigured"> & {
  username: string;
  password: string;
  secret: string;
  cookie: string;
  clearUsername: boolean;
  clearPassword: boolean;
  clearSecret: boolean;
  clearCookie: boolean;
};

export type MediaConfigDetail = {
  id: "emby" | "jellyfin" | "plex";
  name: string;
  host: string;
  playHost: string;
  serverName: string;
  active: boolean;
  apiKeyConfigured: boolean;
  tokenConfigured: boolean;
  usernameConfigured: boolean;
  passwordConfigured: boolean;
};

export type MediaConfigInput = Pick<MediaConfigDetail, "host" | "playHost" | "serverName"> & {
  apiKey: string;
  token: string;
  username: string;
  password: string;
  activate: boolean;
  clearApiKey: boolean;
  clearToken: boolean;
  clearUsername: boolean;
  clearPassword: boolean;
};

export type NotificationChannel = {
  id: string;
  name: string;
  type: string;
  typeLabel: string;
  enabled: boolean;
  interactive: boolean;
  canInteract: boolean;
  configured: boolean;
  switches: string[];
  switchLabels: string[];
};

export type NotificationEvent = { id: string; label: string };

export type NotificationsData = {
  items: NotificationChannel[];
  events: NotificationEvent[];
};

export type NotificationTestResult = {
  id: string;
  ok: boolean;
  duration: number;
  message: string;
};

export type NotificationFieldChoice = { value: string; label: string };

export type NotificationFieldOption = {
  key: string;
  title: string;
  type: "text" | "textarea" | "select" | "switch";
  required: boolean;
  tooltip: string;
  placeholder: string;
  default: string | number | boolean | null;
  options: NotificationFieldChoice[];
  writeOnly: boolean;
};

export type NotificationChannelOption = {
  type: string;
  name: string;
  canInteract: boolean;
  fields: NotificationFieldOption[];
};

export type NotificationOptions = {
  channels: NotificationChannelOption[];
  events: NotificationEvent[];
};

export type NotificationDetail = {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  interactive: boolean;
  canInteract: boolean;
  events: string[];
  config: Record<string, string | number | boolean>;
  configuredFields: string[];
};

export type NotificationInput = {
  name: string;
  type: string;
  enabled: boolean;
  interactive: boolean;
  events: string[];
  config: Record<string, string | boolean>;
  clearConfig: string[];
};

export type CustomMessageInput = {
  title: string;
  text: string;
  image: string;
  channelIds: string[];
};

export type CustomMessageResult = { channelCount: number };

export type PluginSummary = {
  id: string;
  name: string;
  description: string;
  version: string;
  author: string;
  authorUrl: string;
  installed: boolean;
  running: boolean;
  configurable: boolean;
	 hasPage: boolean;
  stateKnown?: boolean;
  native?: boolean;
  actionsAvailable?: boolean;
};

export type PluginsData = {
  items: PluginSummary[];
  installedCount: number;
  runningCount: number;
  unknownStateCount?: number;
};

export type PluginConfigChoice = { value: string; label: string };

export type PluginConfigField = {
  key: string;
  title: string;
  section: string;
  type: "text" | "textarea" | "select" | "switch" | "multiselect";
  required: boolean;
  tooltip: string;
  placeholder: string;
  options: PluginConfigChoice[];
  default: string | boolean | null;
  writeOnly: boolean;
  configured: boolean;
  readOnly: boolean;
};

export type PluginConfigDetail = {
  id: string;
  name: string;
  fields: PluginConfigField[];
  values: Record<string, string | boolean | string[]>;
  meta: { hasPage: boolean };
};

export type PluginConfigInput = {
  values: Record<string, string | boolean | string[]>;
  clearConfig: string[];
};

export type PluginPageDeleteConfirmation = "confirm" | "typeRecordId";

export type PluginPageRowAction = { type: "delete"; recordId: string; confirmation: PluginPageDeleteConfirmation };

export type PluginPageTable = {
  columns: string[];
  rows: string[][];
  rowActions: Array<PluginPageRowAction | null>;
};

export type PluginPageData = {
  title: string;
  sections: string[];
  tables: PluginPageTable[];
  readOnly: boolean;
  actionsOmitted: boolean;
  canDeleteRecords: boolean;
  deleteConfirmation: PluginPageDeleteConfirmation | "";
};

export type SearchMedia = {
  key: string;
  title: string;
  year: string;
  type: string;
  vote: string;
  tmdbId: string;
  poster: string;
  overview: string;
  exists: boolean;
  existsKnown?: boolean;
  resources: SearchResource[];
};

export type SearchData = {
  keyword: string;
  total: number;
  items: SearchMedia[];
  warnings?: string[];
};

type ApiResponse<T> = {
  code: number;
  success: boolean;
  data?: T;
  message?: string;
};

type LoginData = {
  token: string;
  userinfo: UserInfo;
};

export class ApiError extends Error {
  constructor(message: string, readonly code?: number) {
    super(message);
    this.name = "ApiError";
  }
}

export async function login(username: string, password: string): Promise<AuthSession> {
  const body = new URLSearchParams({ username, password });
  const response = await fetch("/api/v1/user/login", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body,
  });
  const result = await readResponse<LoginData>(response);

  if (!result.success || result.code !== 0 || !result.data?.token) {
    throw new ApiError(result.message || "用户名或密码错误", result.code);
  }

  return { token: result.data.token, user: result.data.userinfo };
}

export async function logout(token: string): Promise<void> {
  const response = await fetch("/api/v1/system/logout", {
    method: "POST",
    headers: { Authorization: token },
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) {
    throw new ApiError(result.message || "退出失败", result.code);
  }
}

export async function getDashboard(token: string): Promise<DashboardData> {
  const response = await fetch("/api/v1/dashboard", {
    headers: { Authorization: token },
  });
  const result = await readResponse<DashboardData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "媒体库数据加载失败", result.code);
  }
  return result.data;
}

export async function searchResources(token: string, keyword: string, quick = false): Promise<SearchData> {
  const response = await fetch("/api/v1/search/resources", {
    method: "POST",
    headers: {
      Authorization: token,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ keyword, quick }),
  });
  const result = await readResponse<SearchData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "资源搜索失败", result.code);
  }
  return result.data;
}

export async function addSearchResource(token: string, resourceId: string): Promise<string> {
  const response = await fetch("/api/v1/downloads/resource", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ resourceId }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "添加下载失败", result.code);
  return result.message || "已添加到下载器";
}

export async function getDownloads(token: string, historyPage = 1): Promise<DownloadsData> {
  const response = await fetch(`/api/v1/downloads?historyPage=${historyPage}`, { headers: { Authorization: token } });
  const result = await readResponse<DownloadsData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "下载任务加载失败", result.code);
  }
  return result.data;
}

export async function controlDownload(token: string, id: string, action: "start" | "stop" | "remove"): Promise<void> {
  const response = await fetch(`/api/v1/downloads/${encodeURIComponent(id)}/${action}`, {
    method: "POST",
    headers: { Authorization: token },
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "下载任务操作失败", result.code);
}

export async function addMagnetDownload(token: string, magnet: string): Promise<void> {
  const response = await fetch("/api/v1/downloads/magnet", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ magnet }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "磁力任务添加失败", result.code);
}

export async function addTorrentDownload(token: string, file: File): Promise<void> {
  const body = new FormData();
  body.append("torrent", file);
  const response = await fetch("/api/v1/downloads/torrent", {
    method: "POST",
    headers: { Authorization: token },
    body,
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "种子文件添加失败", result.code);
}

export async function addSiteTorrentLink(token: string, siteId: string, url: string): Promise<void> {
  const response = await fetch("/api/v1/downloads/link", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ siteId, url }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "站点种子链接添加失败", result.code);
}

export async function getSubscriptions(token: string): Promise<SubscriptionsData> {
  const response = await fetch("/api/v1/subscriptions", { headers: { Authorization: token } });
  const result = await readResponse<SubscriptionsData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "订阅数据加载失败", result.code);
  }
  return result.data;
}

export type OrganizationRoot = { id: string; path: string; label: string; type: string };
export type OrganizationRoots = { sources: OrganizationRoot[]; targets: OrganizationRoot[]; executionModes?: string[] };
export type OrganizationPlan = { previewOnly: boolean; fingerprint: string; mode: string; skipped: number; items: Array<{ source: string; target?: string; kind: string; size: number; modified: string; identity: string; status: string; reason?: string; tmdbId?: string }> };
export type OrganizationJob = { id: string; fingerprint: string; created: string; state: string; mode: string; sourceRoot: string; targetRoot: string; items: Array<{ index: number; source: string; target: string; kind: string; size: number; modified: string; state: string; reason?: string; tmdbId: string }> };
export async function getOrganizationRoots(token: string): Promise<OrganizationRoots> {
  const result = await readResponse<OrganizationRoots>(await fetch("/api/v1/organization/roots", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理目录加载失败", result.code);
  return result.data;
}
export async function previewOrganization(token: string, input: { sourceId: string; targetId: string; path: string; mode: string }): Promise<OrganizationPlan> {
  const result = await readResponse<OrganizationPlan>(await fetch("/api/v1/organization/plan", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data || result.data.previewOnly !== true || typeof result.data.fingerprint !== "string" || result.data.fingerprint.length !== 64) throw new ApiError(result.message || "整理预览失败", result.code);
  return result.data;
}
export async function getOrganizationJobs(token: string): Promise<OrganizationJob[]> {
  const result = await readResponse<OrganizationJob[]>(await fetch("/api/v1/organization/jobs", { headers: { Authorization: token } }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务加载失败", result.code);
  return result.data;
}
export async function createOrganizationJob(token: string, input: { sourceId: string; targetId: string; path: string; mode: string; fingerprint: string }): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch("/api/v1/organization/jobs", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务创建失败", result.code);
  return result.data;
}
export async function controlOrganizationJob(token: string, id: string, action: "execute" | "reconcile" | "cancel" | "resume-move" | "resume-publication", confirmSourceRemoval = false): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}/${action}`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(confirmSourceRemoval ? { confirm: true, confirmSourceRemoval: true } : { confirm: true }) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "整理任务操作失败，请刷新查看已保存状态", result.code);
  return result.data;
}

export async function abandonOrganizationJob(token: string, id: string, confirmDiscardStaging = false, confirmOldExecutorsStopped = false): Promise<OrganizationJob> {
  const result = await readResponse<OrganizationJob>(await fetch(`/api/v1/organization/jobs/${encodeURIComponent(id)}/abandon-unpublished`, { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify({ confirm: true, confirmDiscardStaging, confirmOldExecutorsStopped }) }));
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "暂存处置未确认完成，请刷新查看保存状态", result.code);
  return result.data;
}

export type SubscriptionSearchRunResult = {
  submitted: number;
  remaining: number[];
  completed: boolean;
  uncertain: boolean;
};

export async function controlSubscription(token: string, type: "MOV" | "TV", id: string, action: "refresh" | "remove" | "reconcile"): Promise<SubscriptionSearchRunResult | undefined> {
  return subscriptionMutation<SubscriptionSearchRunResult>(token, `/api/v1/subscriptions/${type}/${encodeURIComponent(id)}/${action}`);
}

export async function controlSubscriptionHistory(token: string, type: "MOV" | "TV", id: string, action: "redo" | "remove"): Promise<void> {
  await subscriptionMutation(token, `/api/v1/subscriptions/history/${type}/${encodeURIComponent(id)}/${action}`);
}

async function subscriptionMutation<T = Record<string, never>>(token: string, path: string): Promise<T | undefined> {
  const response = await fetch(path, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<T>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "订阅操作失败", result.code);
  return result.data;
}

export async function getDiscovery(token: string, category: string, page = 1): Promise<DiscoveryData> {
  const response = await fetch("/api/v1/discovery", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ category, page }),
  });
  const result = await readResponse<DiscoveryData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "探索内容加载失败", result.code);
  return result.data;
}

export async function addDefaultSubscription(token: string, media: Pick<DiscoveryMedia, "id" | "title" | "year" | "type">): Promise<void> {
  const response = await fetch("/api/v1/subscriptions", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ name: media.title, year: media.year, type: media.type, mediaId: media.id,
      fuzzyMatch: false, overEdition: false, rssSites: [], searchSites: [] }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "添加订阅失败", result.code);
}

export async function getSubscriptionOptions(token: string): Promise<SubscriptionOptions> {
  const response = await fetch("/api/v1/subscriptions/options", { headers: { Authorization: token } });
  const result = await readResponse<SubscriptionOptions>(response);
  if (!result.success || !result.data) throw new ApiError(result.message || "订阅选项加载失败", result.code);
  return result.data;
}

export async function saveSubscription(token: string, input: SubscriptionInput): Promise<void> {
  const response = await fetch("/api/v1/subscriptions", { method: "POST", headers: { Authorization: token, "Content-Type": "application/json" }, body: JSON.stringify(input) });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "保存订阅失败", result.code);
}

export async function getSites(token: string): Promise<SitesData> {
  const response = await fetch("/api/v1/sites", { headers: { Authorization: token } });
  const result = await readResponse<SitesData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点列表加载失败", result.code);
  }
  return result.data;
}

export async function testSiteConnection(token: string, id: string): Promise<SiteTestResult> {
  const response = await fetch(`/api/v1/sites/${encodeURIComponent(id)}/test`, {
    method: "POST",
    headers: { Authorization: token },
  });
  const result = await readResponse<SiteTestResult>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点连接测试失败", result.code);
  }
  return result.data;
}

export async function getSite(token: string, id: string): Promise<SiteDetail> {
  const response = await fetch(`/api/v1/sites/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<SiteDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点详情加载失败", result.code);
  }
  return result.data;
}

export async function saveSite(token: string, input: SiteInput, id?: string): Promise<void> {
  const response = await fetch(id ? `/api/v1/sites/${encodeURIComponent(id)}` : "/api/v1/sites", {
    method: id ? "PUT" : "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "站点保存失败", result.code);
}

export async function getSiteOptions(token: string): Promise<SiteOptions> {
  const response = await fetch("/api/v1/sites/options", { headers: { Authorization: token } });
  const result = await readResponse<SiteOptions>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "站点选项加载失败", result.code);
  }
  return result.data;
}

export async function deleteSite(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/sites/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "站点删除失败", result.code);
}

export async function getServices(token: string): Promise<ServicesData> {
  const response = await fetch("/api/v1/services", { headers: { Authorization: token } });
  const result = await readResponse<ServicesData>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "服务状态加载失败", result.code);
  }
  return result.data;
}

export async function testManagedService(token: string, service: Pick<ManagedService, "kind" | "id">): Promise<ServiceTestResult> {
  const response = await fetch(`/api/v1/services/${service.kind}/${encodeURIComponent(service.id)}/test`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<ServiceTestResult>(response);
  if (!result.success || result.code !== 0 || !result.data) {
    throw new ApiError(result.message || "服务测试失败", result.code);
  }
  return result.data;
}

export async function getDownloaderConfig(token: string, id: string): Promise<DownloaderConfigDetail> {
  const response = await fetch(`/api/v1/services/downloader/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<DownloaderConfigDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "下载器配置加载失败", result.code);
  return result.data;
}

export async function getDownloaderOptions(token: string): Promise<DownloaderOptions> {
  const response = await fetch("/api/v1/services/downloader-options", { headers: { Authorization: token } });
  const result = await readResponse<DownloaderOptions>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "下载目录选项加载失败", result.code);
  return result.data;
}

export async function saveDownloaderConfig(token: string, input: DownloaderConfigInput, id?: string): Promise<void> {
  const response = await fetch(id ? `/api/v1/services/downloader/${encodeURIComponent(id)}` : "/api/v1/services/downloader", {
    method: id ? "PUT" : "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "下载器保存失败", result.code);
}

export async function deleteDownloaderConfig(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/services/downloader/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "下载器删除失败", result.code);
}

export async function setDefaultDownloader(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/services/downloader/${encodeURIComponent(id)}/default`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "默认下载器设置失败", result.code);
}

export async function getMediaConfig(token: string, id: MediaConfigDetail["id"]): Promise<MediaConfigDetail> {
  const response = await fetch(`/api/v1/services/media/${id}`, { headers: { Authorization: token } });
  const result = await readResponse<MediaConfigDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "媒体服务器配置加载失败", result.code);
  return result.data;
}

export async function saveMediaConfig(token: string, id: MediaConfigDetail["id"], input: MediaConfigInput): Promise<void> {
  const response = await fetch(`/api/v1/services/media/${id}`, {
    method: "PUT",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "媒体服务器保存失败", result.code);
}

export async function getNotifications(token: string): Promise<NotificationsData> {
  const response = await fetch("/api/v1/notifications", { headers: { Authorization: token } });
  const result = await readResponse<NotificationsData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知渠道加载失败", result.code);
  return result.data;
}

export async function updateNotificationStatus(token: string, id: string, status: { enabled: boolean } | { interactive: boolean }): Promise<void> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}/status`, {
    method: "PUT",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(status),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "通知状态更新失败", result.code);
}

export async function testNotification(token: string, id: string): Promise<NotificationTestResult> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}/test`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<NotificationTestResult>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知测试失败", result.code);
  return result.data;
}

export async function deleteNotification(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "通知渠道删除失败", result.code);
}

export async function getNotificationOptions(token: string): Promise<NotificationOptions> {
  const response = await fetch("/api/v1/notifications/options", { headers: { Authorization: token } });
  const result = await readResponse<NotificationOptions>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知渠道选项加载失败", result.code);
  return result.data;
}

export async function getNotification(token: string, id: string): Promise<NotificationDetail> {
  const response = await fetch(`/api/v1/notifications/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<NotificationDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "通知渠道详情加载失败", result.code);
  return result.data;
}

export async function saveNotification(token: string, input: NotificationInput, id?: string): Promise<void> {
  const response = await fetch(id ? `/api/v1/notifications/${encodeURIComponent(id)}` : "/api/v1/notifications", {
    method: id ? "PUT" : "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "通知渠道保存失败", result.code);
}

export async function sendCustomMessage(token: string, input: CustomMessageInput): Promise<CustomMessageResult> {
  const response = await fetch("/api/v1/notifications/custom-message", {
    method: "POST",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<CustomMessageResult>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "自定义消息发送失败", result.code);
  return result.data;
}

export async function getPlugins(token: string): Promise<PluginsData> {
  const response = await fetch("/api/v1/plugins", { headers: { Authorization: token } });
  const result = await readResponse<PluginsData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "插件列表加载失败", result.code);
  return result.data;
}

export async function installPlugin(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}/install`, { method: "POST", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件安装失败", result.code);
}

export async function uninstallPlugin(token: string, id: string): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}`, { method: "DELETE", headers: { Authorization: token } });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件卸载失败", result.code);
}

export async function getPluginConfig(token: string, id: string): Promise<PluginConfigDetail> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}`, { headers: { Authorization: token } });
  const result = await readResponse<PluginConfigDetail>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "插件配置加载失败", result.code);
  return result.data;
}

export async function savePluginConfig(token: string, id: string, input: PluginConfigInput): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件配置保存失败", result.code);
}

export async function getPluginPage(token: string, id: string): Promise<PluginPageData> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}/page`, { headers: { Authorization: token } });
  const result = await readResponse<PluginPageData>(response);
  if (!result.success || result.code !== 0 || !result.data) throw new ApiError(result.message || "插件扩展页加载失败", result.code);
  return result.data;
}

export async function deletePluginPageRecord(token: string, id: string, recordId: string, confirmation = ""): Promise<void> {
  const response = await fetch(`/api/v1/plugins/${encodeURIComponent(id)}/page/records`, {
    method: "DELETE",
    headers: { Authorization: token, "Content-Type": "application/json" },
    body: JSON.stringify({ recordId, confirmation }),
  });
  const result = await readResponse<Record<string, never>>(response);
  if (!result.success || result.code !== 0) throw new ApiError(result.message || "插件扩展页记录删除失败", result.code);
}

async function readResponse<T>(response: Response): Promise<ApiResponse<T>> {
  let result: ApiResponse<T>;
  try {
    result = (await response.json()) as ApiResponse<T>;
  } catch {
    if (!response.ok) throw new ApiError(`服务请求失败（HTTP ${response.status}）`, response.status);
    throw new ApiError("服务返回了无法识别的数据");
  }
  if (!response.ok) throw new ApiError(result.message || `服务请求失败（HTTP ${response.status}）`, result.code || response.status);
  return result;
}
