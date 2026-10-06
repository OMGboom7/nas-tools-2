import type { AuthSession } from "../api/client";

const storageKey = "nas-tools.auth";

export function loadSession(): AuthSession | null {
  return readStorage(localStorage) ?? readStorage(sessionStorage);
}

export function saveSession(session: AuthSession, remember: boolean): void {
  clearSession();
  const storage = remember ? localStorage : sessionStorage;
  storage.setItem(storageKey, JSON.stringify(session));
}

export function clearSession(): void {
  localStorage.removeItem(storageKey);
  sessionStorage.removeItem(storageKey);
}

function readStorage(storage: Storage): AuthSession | null {
  const value = storage.getItem(storageKey);
  if (!value) return null;
  try {
    const session = JSON.parse(value) as Partial<AuthSession>;
    if (typeof session.token !== "string" || !session.token || !session.user || typeof session.user.username !== "string") {
      storage.removeItem(storageKey);
      return null;
    }
    return session as AuthSession;
  } catch {
    storage.removeItem(storageKey);
    return null;
  }
}
