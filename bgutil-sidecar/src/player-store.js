// Disk store for prepared (ejs-preprocessed) players, keyed by player ID and
// ejs version. Every function is a no-op when no directory is configured.
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";

export const RETENTION_MS = 7 * 24 * 60 * 60 * 1000;
const TMP_MAX_AGE_MS = 60 * 60 * 1000;
const HEADER_RE = /^\/\/moombox-prepared v1 sha256=([0-9a-f]{64})$/;
const SAFE_ID_RE = /^[A-Za-z0-9_-]{1,64}$/;

let ejs12 = null;
try {
    ejs12 = fs.readFileSync(new URL("../vendor/ejs/VERSION", import.meta.url), "utf8").trim().slice(0, 12) || null;
} catch {
    ejs12 = null;
}

let dirOverride; // undefined = use the environment

function warn(msg) {
    process.stderr.write(`[bgutil-sidecar:warn] player-store: ${msg}\n`);
}

export function storeDir() {
    if (!ejs12) return null;
    const d = dirOverride !== undefined ? dirOverride : process.env.MOOMBOX_SIDECAR_PLAYER_DIR;
    return d ? d : null;
}

export function _setStoreDirForTest(dir) {
    dirOverride = dir === null ? undefined : dir;
}

export function isSafePlayerID(id) {
    return typeof id === "string" && SAFE_ID_RE.test(id);
}

export function rawHash(playerJS) {
    return crypto.createHash("sha256").update(playerJS, "utf8").digest("hex");
}

function fileFor(dir, playerID) {
    return path.join(dir, `${playerID}.${ejs12}.js`);
}

export function readPrepared(playerID) {
    const dir = storeDir();
    if (!dir || !isSafePlayerID(playerID)) return null;
    const file = fileFor(dir, playerID);
    let text;
    try {
        text = fs.readFileSync(file, "utf8");
    } catch (e) {
        if (e.code !== "ENOENT") warn(`read ${playerID}: ${e.message}`);
        return null;
    }
    const nl = text.indexOf("\n");
    const m = nl < 0 ? null : HEADER_RE.exec(text.slice(0, nl));
    if (!m) {
        warn(`damaged header for ${playerID}; removing`);
        removePrepared(playerID);
        return null;
    }
    try {
        const now = new Date();
        fs.utimesSync(file, now, now);
    } catch (e) {
        warn(`touch ${playerID}: ${e.message}`);
    }
    return { source: text.slice(nl + 1), rawHash: m[1] };
}

export function writePrepared(playerID, hash, source) {
    const dir = storeDir();
    if (!dir || !isSafePlayerID(playerID) || typeof source !== "string" || !source.isWellFormed()) return false;
    const file = fileFor(dir, playerID);
    const tmp = `${file}.${process.pid}.tmp`;
    try {
        fs.mkdirSync(dir, { recursive: true });
        fs.writeFileSync(tmp, `//moombox-prepared v1 sha256=${hash}\n${source}`, "utf8");
        fs.renameSync(tmp, file);
        return true;
    } catch (e) {
        warn(`write ${playerID}: ${e.message}`);
        try { fs.rmSync(tmp, { force: true }); } catch { /* best effort */ }
        return false;
    }
}

export function removePrepared(playerID) {
    const dir = storeDir();
    if (!dir || !isSafePlayerID(playerID)) return;
    try {
        fs.rmSync(fileFor(dir, playerID), { force: true });
    } catch (e) {
        warn(`remove ${playerID}: ${e.message}`);
    }
}

export function sweepStore(now = Date.now()) {
    const dir = storeDir();
    if (!dir) return 0;
    let names;
    try {
        names = fs.readdirSync(dir);
    } catch (e) {
        if (e.code !== "ENOENT") warn(`sweep: ${e.message}`);
        return 0;
    }
    const current = `.${ejs12}.js`;
    let deleted = 0;
    for (const name of names) {
        const isJS = name.endsWith(".js");
        const isTmp = name.endsWith(".tmp");
        if (!isJS && !isTmp) continue;
        const file = path.join(dir, name);
        try {
            const age = now - fs.statSync(file).mtimeMs;
            let drop;
            if (isTmp) drop = age > TMP_MAX_AGE_MS;
            else drop = !name.endsWith(current) || age > RETENTION_MS;
            if (drop) {
                fs.rmSync(file, { force: true });
                deleted++;
            }
        } catch (e) {
            warn(`sweep ${name}: ${e.message}`);
        }
    }
    return deleted;
}
