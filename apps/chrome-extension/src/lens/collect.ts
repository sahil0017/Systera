import { NormalizedObservation } from "../shared/types";

type Win = typeof globalThis & { __systeraLens?: { collect: () => NormalizedObservation } };

/** Injects the collector into a tab (on demand) and returns its preview observation. */
export async function collectFromTab(tabId: number): Promise<NormalizedObservation> {
  await chrome.scripting.executeScript({ target: { tabId }, files: ["dist/content.js"] });
  const [res] = await chrome.scripting.executeScript({
    target: { tabId },
    func: () => (globalThis as Win).__systeraLens?.collect() ?? null,
  });
  const obs = res?.result as NormalizedObservation | null | undefined;
  if (!obs || !obs.url) throw new Error("This page cannot be scanned (browser pages and the Web Store are off limits).");
  return obs;
}

function waitForLoad(tabId: number, timeoutMs: number): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      chrome.tabs.onUpdated.removeListener(listener);
      reject(new Error("The page took too long to load."));
    }, timeoutMs);
    const listener = (id: number, info: chrome.tabs.TabChangeInfo) => {
      if (id === tabId && info.status === "complete") {
        clearTimeout(timer);
        chrome.tabs.onUpdated.removeListener(listener);
        resolve();
      }
    };
    chrome.tabs.onUpdated.addListener(listener);
  });
}

/** Visits the user-selected pages one at a time in the current tab and collects previews. */
export async function collectPages(tabId: number, urls: string[], onProgress: (done: number, url: string) => void): Promise<NormalizedObservation[]> {
  const out: NormalizedObservation[] = [];
  for (const [i, url] of urls.entries()) {
    onProgress(i, url);
    const loaded = waitForLoad(tabId, 20000);
    await chrome.tabs.update(tabId, { url });
    await loaded;
    out.push(await collectFromTab(tabId));
  }
  onProgress(urls.length, "");
  return out;
}
