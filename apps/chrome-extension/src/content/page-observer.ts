import { buildNormalizedObservation } from "../browser-adapter/normalize";

// Injected on demand with chrome.scripting.executeScript after the user asks for a scan; it
// only defines a collector function and sends nothing anywhere. The side panel calls it and
// shows the result as a preview.
(globalThis as unknown as { __systeraLens?: unknown }).__systeraLens = {
  collect: () => buildNormalizedObservation(document, location.href, performance),
};
