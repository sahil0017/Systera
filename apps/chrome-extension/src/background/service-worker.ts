// MV3 service worker: opens the side panel when the user clicks the toolbar action. That
// click is also what grants activeTab for the current page; nothing runs on its own.
chrome.action.onClicked.addListener(async (tab) => {
  if (tab.id !== undefined) await chrome.sidePanel.open({ tabId: tab.id });
});
