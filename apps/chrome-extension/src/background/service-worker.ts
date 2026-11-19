import { autoConfigureSession } from "../auth/autoconfig";

// MV3 service worker: opens the side panel when the user clicks the toolbar action. That
// click is also what grants activeTab for the current page; nothing runs on its own.
chrome.action.onClicked.addListener(async (tab) => {
  if (tab.id !== undefined) await chrome.sidePanel.open({ tabId: tab.id });
});

// Background pairing listener so the Web Workspace or side panel can auto-configure a
// short-lived lens session in the background without manual URL/token entry.
chrome.runtime?.onMessage?.addListener((msg, _sender, sendResponse) => {
  if (msg?.type === "SYSTERA_AUTO_CONFIGURE") {
    autoConfigureSession(msg.apiBaseUrl, msg.token)
      .then((session) => sendResponse({ ok: true, session }))
      .catch((err: Error) => sendResponse({ ok: false, error: err.message }));
    return true;
  }
  return false;
});
