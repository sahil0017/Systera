// Bundles the extension (and, with --tests, the test suites) with esbuild. Chrome cannot load
// tsc's extensionless ES imports, and injected collectors must be a single classic script.
import * as esbuild from "esbuild";
import { readdirSync, rmSync } from "node:fs";

const tests = process.argv.includes("--tests");
const common = { bundle: true, target: "chrome116", sourcemap: false, logLevel: "warning", legalComments: "none" };

if (tests) {
  rmSync("dist-test", { recursive: true, force: true });
  await esbuild.build({
    ...common, target: "node22", platform: "node", format: "esm", outdir: "dist-test", outExtension: { ".js": ".mjs" },
    entryPoints: readdirSync("test").filter((f) => f.endsWith(".test.ts")).map((f) => `test/${f}`),
    external: ["jsdom"],
  });
} else {
  rmSync("dist", { recursive: true, force: true });
  await esbuild.build({ ...common, entryPoints: { background: "src/background/service-worker.ts" }, outdir: "dist", format: "esm" });
  await esbuild.build({ ...common, entryPoints: { content: "src/content/page-observer.ts" }, outdir: "dist", format: "iife" });
  await esbuild.build({ ...common, entryPoints: { sidepanel: "src/sidepanel/main.tsx" }, outdir: "dist", format: "iife",
    define: { "process.env.NODE_ENV": '"production"' }, minify: true });
}
