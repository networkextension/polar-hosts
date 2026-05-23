// Build the hosts plugin UI.
//
//   src/*.ts        -> dist/scripts/<name>.js   (esbuild bundle, ESM, one per entry)
//   public/*        -> dist/                    (HTML + any static here)
//   tsc --noEmit                                (type check first)
//
// Run from `ui/`: `npm run build`.
//
// Multi-entry note:
//   hosts has TWO top-level entries — hosts.ts (page load) and
//   hosts-shell.ts (lazy-loaded xterm modal via `await
//   import("/scripts/hosts-shell.js")`). esbuild scans src/*.ts and
//   emits each as a self-contained bundle. splitting:false is
//   intentional — code-splitting would extract shared chunks and
//   break the runtime `/scripts/hosts-shell.js` URL lookup that nginx
//   serves; the dock build worked the same way.

import { spawn } from "node:child_process";
import { cp, mkdir, readdir, rm } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import * as esbuild from "esbuild";

const root = path.dirname(fileURLToPath(import.meta.url)) + "/..";
const srcDir = path.join(root, "src");
const publicDir = path.join(root, "public");
const distDir = path.join(root, "dist");
const scriptsDir = path.join(distDir, "scripts");

async function runTsc() {
    await new Promise((resolve, reject) => {
        const child = spawn(
            process.execPath,
            [
                path.join(root, "node_modules", "typescript", "bin", "tsc"),
                "-p",
                path.join(root, "tsconfig.json"),
            ],
            { cwd: root, stdio: "inherit" },
        );
        child.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`tsc exit ${code}`))));
        child.on("error", reject);
    });
}

await rm(distDir, { recursive: true, force: true });
await mkdir(scriptsDir, { recursive: true });

await runTsc();

const entries = (await readdir(srcDir, { withFileTypes: true }))
    .filter((e) => e.isFile() && e.name.endsWith(".ts"))
    .map((e) => path.join(srcDir, e.name));

await esbuild.build({
    entryPoints: entries,
    bundle: true,
    splitting: false,
    format: "esm",
    target: ["es2022"],
    platform: "browser",
    outdir: scriptsDir,
    sourcemap: true,
    logLevel: "info",
    loader: { ".css": "css" },
});

// Copy public/ assets verbatim — hosts.html and anything else the
// plugin owns. polar-ui-common's static/styles.css + static/assets/*
// are deployed separately by scripts/deploy-ui.sh from the installed
// node_modules so the plugin doesn't carry a CSS copy.
await cp(publicDir, distDir, { recursive: true });

console.log("built hosts plugin UI -> dist/");
