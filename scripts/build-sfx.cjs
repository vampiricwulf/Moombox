/**
 * Build Self-Extracting Moombox Executable
 *
 * Creates a Node.js SEA that extracts and runs the app bundle directly:
 * - On first run, extracts the app bundle to ./assets/ next to the exe
 * - Subsequent runs use the extracted files (faster startup)
 * - Runs the app in-process via createRequire() (no child process)
 *
 * The SEA contains:
 * - Compressed app bundle (loaded directly — the SEA IS the Node.js runtime)
 *
 * No TypeScript compilation needed - bundles the source directly with esbuild
 */

const { execSync, spawnSync } = require("child_process");
const https = require("https");
const fs = require("fs");
const path = require("path");
const zlib = require("zlib");
const esbuild = require("esbuild");

const rootDir = path.resolve(__dirname, "..");
const buildDir = path.join(rootDir, "sea-build");
const nodeDir = path.join(buildDir, "node");
const nodeExe = path.join(nodeDir, "node.exe");
const outputExe = path.join(rootDir, "Moombox.exe");

const NODE_VERSION = "v20.18.0";
const NODE_URL = `https://nodejs.org/dist/${NODE_VERSION}/node-${NODE_VERSION}-win-x64.zip`;

// SEA config for the launcher
const seaConfig = {
  main: path.join(buildDir, "launcher.cjs"),
  output: path.join(buildDir, "sea-prep.blob"),
  disableExperimentalSEAWarning: true,
  useCodeCache: true,
};

async function downloadFile(url, dest) {
  return new Promise((resolve, reject) => {
    console.log(`Downloading ${url}...`);
    const file = fs.createWriteStream(dest);

    https
      .get(url, (response) => {
        if (response.statusCode === 302 || response.statusCode === 301) {
          file.close();
          fs.unlinkSync(dest);
          return downloadFile(response.headers.location, dest).then(
            resolve,
            reject,
          );
        }

        if (response.statusCode !== 200) {
          reject(new Error(`HTTP ${response.statusCode}`));
          return;
        }

        const totalBytes = parseInt(response.headers["content-length"], 10);
        let downloadedBytes = 0;

        response.on("data", (chunk) => {
          downloadedBytes += chunk.length;
          const percent = ((downloadedBytes / totalBytes) * 100).toFixed(1);
          process.stdout.write(
            `\rDownloading: ${percent}% (${(downloadedBytes / 1024 / 1024).toFixed(1)}MB)`,
          );
        });

        response.pipe(file);
        file.on("finish", () => {
          file.close();
          console.log("\nDownload complete.");
          resolve();
        });
      })
      .on("error", (err) => {
        fs.unlink(dest, () => {});
        reject(err);
      });
  });
}

async function downloadNode() {
  if (fs.existsSync(nodeExe)) {
    console.log(`Using existing Node.js: ${nodeExe}`);
    return;
  }

  console.log("Node.js not found, downloading...\n");

  if (!fs.existsSync(nodeDir)) {
    fs.mkdirSync(nodeDir, { recursive: true });
  }

  const zipPath = path.join(nodeDir, "node.zip");
  await downloadFile(NODE_URL, zipPath);

  console.log("Extracting Node.js...");
  const extractDir = path.join(nodeDir, `node-${NODE_VERSION}-win-x64`);

  // Use PowerShell to extract
  execSync(
    `powershell -Command "Expand-Archive -Path '${zipPath}' -DestinationPath '${nodeDir}' -Force"`,
    { stdio: "inherit" },
  );

  // Move node.exe to expected location
  fs.copyFileSync(path.join(extractDir, "node.exe"), nodeExe);

  // Clean up
  fs.rmSync(extractDir, { recursive: true, force: true });
  fs.unlinkSync(zipPath);

  console.log(`Node.js ${NODE_VERSION} ready.\n`);
}

async function bundleApp() {
  console.log("Bundling application with esbuild (from TypeScript)...");

  // Read public files for embedding
  const publicDir = path.join(rootDir, "src", "web", "public");
  const assets = {};

  function readDirRecursive(dir, prefix = "") {
    if (!fs.existsSync(dir)) return;
    const entries = fs.readdirSync(dir, { withFileTypes: true });
    for (const entry of entries) {
      const fullPath = path.join(dir, entry.name);
      const assetPath = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.isDirectory()) {
        readDirRecursive(fullPath, assetPath);
      } else {
        assets[assetPath] = fs.readFileSync(fullPath);
      }
    }
  }

  readDirRecursive(publicDir);

  // Embed sql-wasm.wasm for AutoCookieService (Firefox SQLite reading)
  const sqlWasmPath = path.join(rootDir, "node_modules", "sql.js", "dist", "sql-wasm.wasm");
  if (fs.existsSync(sqlWasmPath)) {
    assets["sql-wasm.wasm"] = fs.readFileSync(sqlWasmPath);
    console.log(`  Embedded sql-wasm.wasm (${(assets["sql-wasm.wasm"].length / 1024).toFixed(0)} KB)`);
  }

  // Plugin to handle jsdom's xhr-sync-worker.js require.resolve
  const xhrWorkerPlugin = {
    name: "xhr-worker-plugin",
    setup(build) {
      // Replace the require.resolve call for xhr-sync-worker.js with null
      build.onLoad({ filter: /XMLHttpRequest-impl\.js$/ }, async (args) => {
        let contents = fs.readFileSync(args.path, "utf8");
        // Replace the syncWorkerFile assignment to return null
        contents = contents.replace(
          /const syncWorkerFile = require\.resolve\s*\?\s*require\.resolve\(["']\.\/xhr-sync-worker\.js["']\)\s*:\s*null;/,
          "const syncWorkerFile = null;",
        );
        return { contents, loader: "js" };
      });
    },
  };

  // Plugin to redirect bare yoga-layout imports to our shim (avoids top-level await)
  const yogaShimPlugin = {
    name: "yoga-shim",
    setup(build) {
      build.onResolve({ filter: /^yoga-layout$/ }, () => ({
        path: path.join(__dirname, "yoga-shim.js"),
      }));
    },
  };

  // Plugin to strip top-level await from ink's reconciler (dead devtools import)
  const inkReconcilerPlugin = {
    name: "ink-reconciler-fix",
    setup(build) {
      build.onLoad({ filter: /ink[\\/]build[\\/]reconciler\.js$/ }, async (args) => {
        let contents = fs.readFileSync(args.path, "utf8");
        contents = contents.replace(
          /await import\('\.\/devtools\.js'\);/,
          "import('./devtools.js');"
        );
        return { contents, loader: "js" };
      });
    },
  };

  // Bundle directly from TypeScript source
  await esbuild.build({
    entryPoints: [path.join(rootDir, "src", "index.ts")],
    bundle: true,
    platform: "node",
    target: "node20",
    format: "cjs",
    outfile: path.join(buildDir, "app-bundle.cjs"),
    external: [
      "fsevents",             // macOS-only native module
      "canvas",               // Not installed, native module
      "react-devtools-core",  // Optional peer dep, not installed
    ],
    minify: false,
    keepNames: true,
    sourcemap: false,
    inject: [path.join(__dirname, "import-meta-shim.js")],
    define: {
      "import.meta.url": "importMetaUrl",
    },
    plugins: [xhrWorkerPlugin, yogaShimPlugin, inkReconcilerPlugin],
    logLevel: "warning",
  });

  // Prepend embedded assets
  const appCode = fs.readFileSync(
    path.join(buildDir, "app-bundle.cjs"),
    "utf8",
  );

  const finalCode = `// Moombox Application Bundle
// Embedded static assets
globalThis.__MOOMBOX_ASSETS__ = {
${Object.entries(assets)
  .map(([name, content]) => {
    const base64 = content.toString("base64");
    return `  ${JSON.stringify(name)}: Buffer.from(${JSON.stringify(base64)}, "base64")`;
  })
  .join(",\n")}
};

${appCode}
`;

  const appBundlePath = path.join(buildDir, "moombox-app.cjs");
  fs.writeFileSync(appBundlePath, finalCode);
  fs.unlinkSync(path.join(buildDir, "app-bundle.cjs"));

  console.log(`App bundle created: ${appBundlePath}`);
  return appBundlePath;
}

function createLauncher(appBundlePath) {
  console.log("Creating launcher with embedded payload...");

  const appBundleData = fs.readFileSync(appBundlePath);

  console.log(
    `  App bundle size: ${(appBundleData.length / 1024 / 1024).toFixed(1)} MB`,
  );

  // Read yt-dlp plugin for embedding
  const pluginPath = path.join(rootDir, "src", "pot-plugin", "yt_dlp_plugins", "extractor", "getpot_moombox.py");
  const pluginData = fs.readFileSync(pluginPath, "utf8");
  console.log(`  yt-dlp plugin: ${pluginData.length} bytes`);

  // Compress payload
  console.log("  Compressing payload...");
  const appCompressed = zlib.gzipSync(appBundleData, { level: 9 });

  console.log(
    `  App compressed: ${(appCompressed.length / 1024 / 1024).toFixed(1)} MB`,
  );

  // Create launcher script — runs app in-process via createRequire (no child process)
  const launcherCode = `// Moombox Self-Extracting Launcher
const fs = require("fs");
const path = require("path");
const zlib = require("zlib");
const { createRequire } = require("module");

// Embedded compressed app bundle
const APP_BUNDLE_GZ = Buffer.from(${JSON.stringify(appCompressed.toString("base64"))}, "base64");

// Embedded yt-dlp plugin
const YTDLP_PLUGIN_PY = ${JSON.stringify(pluginData)};

// Version marker for updates
const BUNDLE_VERSION = ${JSON.stringify(new Date().toISOString())};

function getExeDir() {
  return path.dirname(process.execPath);
}

function getAssetsDir() {
  return path.join(getExeDir(), "assets");
}

function extractIfNeeded() {
  const assetsDir = getAssetsDir();
  const appBundlePath = path.join(assetsDir, "moombox-app.cjs");
  const versionFile = path.join(assetsDir, ".version");

  let needsExtract = false;

  if (!fs.existsSync(assetsDir)) {
    fs.mkdirSync(assetsDir, { recursive: true });
    needsExtract = true;
  } else if (!fs.existsSync(appBundlePath)) {
    needsExtract = true;
  } else if (fs.existsSync(versionFile)) {
    const currentVersion = fs.readFileSync(versionFile, "utf8").trim();
    if (currentVersion !== BUNDLE_VERSION) {
      needsExtract = true;
    }
  } else {
    needsExtract = true;
  }

  if (needsExtract) {
    console.log("Extracting Moombox...");

    const appData = zlib.gunzipSync(APP_BUNDLE_GZ);
    fs.writeFileSync(appBundlePath, appData);

    // Clean up leftover node.exe from older builds
    const oldNodeExe = path.join(assetsDir, "node.exe");
    if (fs.existsSync(oldNodeExe)) {
      try { fs.unlinkSync(oldNodeExe); } catch (_) {}
    }

    // Extract yt-dlp plugin (moombox/ wrapper is the package root for yt-dlp's plugin finder)
    const pluginDir = path.join(assetsDir, "yt-dlp-plugin", "moombox", "yt_dlp_plugins", "extractor");
    fs.mkdirSync(pluginDir, { recursive: true });
    fs.writeFileSync(path.join(pluginDir, "getpot_moombox.py"), YTDLP_PLUGIN_PY);

    fs.writeFileSync(versionFile, BUNDLE_VERSION);
    console.log("Ready!\\n");
  }

  return { appBundlePath, assetsDir };
}

try {
  const { appBundlePath, assetsDir } = extractIfNeeded();

  const exeDir = getExeDir();
  process.chdir(exeDir);
  process.env.MOOMBOX_ASSETS_DIR = assetsDir;

  // SEA sets argv to [execPath, execPath, ...userArgs] — skip both exe entries
  process.argv = [process.execPath, appBundlePath, ...process.argv.slice(2)];

  // Load app bundle directly — no child process, so stdin/stdout stay connected to the terminal
  const loadModule = createRequire(appBundlePath);
  loadModule(appBundlePath);
} catch (err) {
  console.error("Moombox failed to start:", err.message);
  console.error(err.stack);
  // Keep window open so the error is visible
  if (process.stdin.isTTY) {
    const { spawnSync } = require("child_process");
    spawnSync(process.env.COMSPEC || "cmd", ["/c", "pause"], { stdio: "inherit" });
  }
  process.exit(1);
}
`;

  const launcherPath = path.join(buildDir, "launcher.cjs");
  fs.writeFileSync(launcherPath, launcherCode);

  const launcherSize = fs.statSync(launcherPath).size;
  console.log(
    `Launcher created: ${(launcherSize / 1024 / 1024).toFixed(1)} MB`,
  );

  return launcherPath;
}

async function build() {
  console.log("Building Self-Extracting Moombox...\n");

  // Create build directory
  if (!fs.existsSync(buildDir)) {
    fs.mkdirSync(buildDir, { recursive: true });
  }

  // Step 1: Download Node.js if needed
  console.log("Step 1: Checking Node.js...");
  await downloadNode();

  // Step 2: Bundle the application (no tsc needed - esbuild handles TypeScript)
  console.log("\nStep 2: Bundling application...");
  const appBundlePath = await bundleApp();

  // Step 3: Create launcher with embedded payloads
  console.log("\nStep 3: Creating launcher...");
  createLauncher(appBundlePath);

  // Step 4: Generate SEA config
  console.log("\nStep 4: Creating SEA config...");
  const configPath = path.join(buildDir, "sea-config.json");
  fs.writeFileSync(configPath, JSON.stringify(seaConfig, null, 2));

  // Step 5: Generate SEA blob
  console.log("\nStep 5: Generating SEA blob...");
  const genResult = spawnSync(
    nodeExe,
    ["--experimental-sea-config", configPath],
    {
      cwd: buildDir,
      stdio: "inherit",
    },
  );
  if (genResult.status !== 0) {
    console.error("Failed to generate SEA blob");
    process.exit(1);
  }

  // Step 6: Copy Node.js executable
  console.log("\nStep 6: Copying Node.js executable...");
  const tempExe = path.join(buildDir, "moombox-unsigned.exe");
  fs.copyFileSync(nodeExe, tempExe);

  // Step 7: Inject blob using postject
  console.log("\nStep 7: Injecting SEA blob...");
  const blobPath = path.join(buildDir, "sea-prep.blob");

  const postjectResult = spawnSync(
    "npx",
    [
      "postject",
      tempExe,
      "NODE_SEA_BLOB",
      blobPath,
      "--sentinel-fuse",
      "NODE_SEA_FUSE_fce680ab2cc467b6e072b8b5df1996b2",
    ],
    {
      cwd: rootDir,
      stdio: "inherit",
      shell: true,
    },
  );

  if (postjectResult.status !== 0) {
    console.error("Failed to inject SEA blob");
    process.exit(1);
  }

  // Step 8: Move to final location
  console.log("\nStep 8: Finalizing executable...");
  if (fs.existsSync(outputExe)) {
    fs.unlinkSync(outputExe);
  }
  fs.renameSync(tempExe, outputExe);

  // Report sizes
  const stats = fs.statSync(outputExe);
  const sizeMB = (stats.size / 1024 / 1024).toFixed(1);

  console.log(`\n${"=".repeat(50)}`);
  console.log(`Self-extracting build complete: ${outputExe}`);
  console.log(`Size: ${sizeMB} MB`);
  console.log(`\nOn first run, extracts to: ./assets/`);
  console.log(`  - moombox-app.cjs (application bundle)`);
  console.log(`  - yt-dlp-plugin/ (yt-dlp POT provider plugin)`);
  console.log(`\nPorts: 774`);
  console.log(`${"=".repeat(50)}`);
}

build().catch((err) => {
  console.error("Build failed:", err);
  process.exit(1);
});
