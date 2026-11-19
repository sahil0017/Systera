import { exchangeForLensToken } from "../api/client";
import { AuthSession, decodeClaims, isLensScoped, normalizeApiBase, saveAuthSession } from "./session";
import { saveWorkspaceOrigin } from "../shared/workspace";

const LAST_API_KEY = "systera_lens_last_api";
const PAIRING_KEY = "systera_workspace_pairing";
const DEFAULT_ENDPOINTS = ["http://13.201.7.204:8080", "http://localhost:8080"];

interface PairingRecord {
  apiBaseUrl?: string;
  token?: string;
  workspaceOrigin?: string; // the tab the session came from
}

async function readLocalPairing(): Promise<PairingRecord> {
  try {
    const local = await chrome.storage?.local?.get?.([LAST_API_KEY, PAIRING_KEY]);
    const managed = await (chrome.storage as unknown as { managed?: chrome.storage.StorageArea })?.managed
      ?.get?.(["apiBaseUrl", "token"])
      .catch(() => ({}));
    const paired = (local?.[PAIRING_KEY] as PairingRecord | undefined) ?? {};
    return {
      apiBaseUrl:
        (managed as PairingRecord)?.apiBaseUrl ||
        paired.apiBaseUrl ||
        (local?.[LAST_API_KEY] as string | undefined),
      token: (managed as PairingRecord)?.token || paired.token,
    };
  } catch {
    return {};
  }
}

async function readActiveWorkspaceTab(): Promise<PairingRecord> {
  try {
    const [tab] = (await chrome.tabs?.query?.({ active: true, currentWindow: true })) ?? [];
    if (!tab?.id || !tab.url || !/^https?:\/\//.test(tab.url)) return {};
    const results = await chrome.scripting?.executeScript?.({
      target: { tabId: tab.id },
      func: () => ({
        apiBaseUrl: window.localStorage.getItem("systera.apiBaseUrl") || window.location.origin,
        token: window.sessionStorage.getItem("systera.apiToken") || window.localStorage.getItem("systera.apiToken") || "",
        workspaceOrigin: window.location.origin,
      }),
    });
    const res = results?.[0]?.result as PairingRecord | undefined;
    return res?.token ? res : {};
  } catch {
    return {};
  }
}

async function bootstrapFromEndpoint(base: string): Promise<string | null> {
  try {
    const res = await fetch(`${base}/v1/auth/lens-bootstrap`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "omit",
      cache: "no-store",
    });
    if (!res.ok) return null;
    const data = (await res.json()) as { access_token?: string };
    return data.access_token ?? null;
  } catch {
    return null;
  }
}

/**
 * Automatically configures the extension session in the background without requiring
 * the user to manually type a Workspace API URL or paste a JWT:
 *   1. Managed / local workspace pairing
 *   2. Active Systera Web Workspace tab handshake
 *   3. Workspace API /v1/auth/lens-bootstrap endpoint
 */
export async function autoConfigureSession(
  manualApi?: string,
  manualToken?: string,
): Promise<AuthSession> {
  // Request host permission synchronously inside the user click gesture before any other await!
  const preloadOrigin = manualApi?.trim() ? `${normalizeApiBase(manualApi)}/*` : "http://13.201.7.204:8080/*";
  await chrome.permissions?.request?.({ origins: [preloadOrigin, "http://localhost:8080/*"] }).catch(() => false);

  const localCfg = await readLocalPairing();
  const tabCfg = !manualToken ? await readActiveWorkspaceTab() : {};

  const savedApi = localCfg.apiBaseUrl && !localCfg.apiBaseUrl.includes("localhost:8080") ? localCfg.apiBaseUrl : undefined;
  const candidateRaw =
    manualApi?.trim() ||
    tabCfg.apiBaseUrl ||
    savedApi ||
    DEFAULT_ENDPOINTS[0]!;
  const base = normalizeApiBase(candidateRaw);

  if (`${base}/*` !== preloadOrigin) {
    const granted = await chrome.permissions?.request?.({ origins: [`${base}/*`] });
    if (granted === false) {
      throw new Error("Lens needs permission to connect to your Systera Workspace API.");
    }
  }

  let rawToken = (manualToken?.trim() || tabCfg.token || localCfg.token || "").trim();
  if (!rawToken) {
    const bootstrapped = await bootstrapFromEndpoint(base);
    if (bootstrapped) rawToken = bootstrapped;
  }
  if (!rawToken) {
    throw new Error(
      "Could not auto-detect an active Systera Workspace session. Open your Systera Workspace tab first, or expand Advanced settings below.",
    );
  }

  let lensToken = rawToken;
  if (!isLensScoped(decodeClaims(lensToken))) {
    lensToken = (await exchangeForLensToken(base, lensToken)).access_token;
  }

  try {
    await chrome.storage?.local?.set?.({ [LAST_API_KEY]: base });
    await chrome.storage?.local?.remove?.(PAIRING_KEY);
    // Remember where the workspace runs so "Open in workspace" lands in the signed-in origin.
    if (tabCfg.token && tabCfg.workspaceOrigin) await saveWorkspaceOrigin(base, tabCfg.workspaceOrigin, chrome.storage?.local);
  } catch {
    // non-fatal
  }

  return saveAuthSession(base, lensToken);
}
