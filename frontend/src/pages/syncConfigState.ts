import type { OrganizationSyncRecord, SyncConfigPatch } from "../api/client";

export type SyncConfigDraft = { source: string; dest: string; unknown: string; mode: string; note: string; enabled: boolean; rename: boolean; compatibility: boolean; locating: boolean };
export function buildSyncConfigPatch(original: OrganizationSyncRecord | undefined, draft: SyncConfigDraft): SyncConfigPatch {
  const patch: SyncConfigPatch = {};
  for (const key of ["source", "dest", "unknown", "mode", "note"] as const) if (!original || draft[key] !== original[key]) patch[key] = draft[key];
  for (const key of ["enabled", "rename", "compatibility", "locating"] as const) if (!original || draft[key] !== (original[key] !== 0)) patch[key] = draft[key];
  return patch;
}
