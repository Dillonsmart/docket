#!/usr/bin/env node
//
// A thin wrapper so `npx @dillonsmart/docket` works without a Go toolchain.
//
// docket is a single static binary. This shim downloads the build for the
// current platform on first use, caches it, verifies it against the published
// checksums, and then gets out of the way: every argument, the exit code and
// stdio all pass straight through.

"use strict";

const { spawn } = require("node:child_process");
const { createHash } = require("node:crypto");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { execFileSync } = require("node:child_process");

const REPO = "Dillonsmart/docket";
const VERSION = "v" + require("../package.json").version;

const TARGETS = {
  "darwin-arm64": "darwin_arm64",
  "darwin-x64": "darwin_amd64",
  "linux-x64": "linux_amd64",
  "linux-arm64": "linux_arm64",
  "win32-x64": "windows_amd64",
  "win32-arm64": "windows_arm64",
};

function target() {
  const key = `${process.platform}-${process.arch}`;
  const t = TARGETS[key];
  if (!t) {
    fail(
      `docket does not publish a build for ${key}.\n` +
        `Build from source instead: go install github.com/${REPO}/cmd/docket@latest`
    );
  }
  return t;
}

function fail(message) {
  process.stderr.write(`docket: ${message}\n`);
  process.exit(1);
}

// Cache outside node_modules so repeated npx invocations do not re-download.
function cacheDir() {
  const base =
    process.env.DOCKET_CACHE_DIR ||
    (process.platform === "win32"
      ? path.join(process.env.LOCALAPPDATA || os.homedir(), "docket")
      : path.join(process.env.XDG_CACHE_HOME || path.join(os.homedir(), ".cache"), "docket"));
  return path.join(base, VERSION);
}

async function download(url) {
  const res = await fetch(url, { redirect: "follow" });
  if (!res.ok) fail(`could not download ${url} (HTTP ${res.status})`);
  return Buffer.from(await res.arrayBuffer());
}

async function verify(archive, assetName) {
  // A checksum file that is never compared is decoration, so a mismatch is
  // fatal; an unpublished checksum file is only a warning.
  const url = `https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS`;
  let sums;
  try {
    sums = (await download(url)).toString("utf8");
  } catch {
    process.stderr.write(`docket: no SHA256SUMS for ${VERSION}, skipping verification\n`);
    return;
  }
  const line = sums.split("\n").find((l) => l.trim().endsWith(assetName));
  if (!line) return;
  const expected = line.trim().split(/\s+/)[0];
  const actual = createHash("sha256").update(archive).digest("hex");
  if (expected !== actual) {
    fail(`checksum mismatch for ${assetName}: expected ${expected}, got ${actual}`);
  }
}

async function install(binPath) {
  const t = target();
  const windows = process.platform === "win32";
  const assetName = `docket_${VERSION}_${t}.${windows ? "zip" : "tar.gz"}`;
  const url = `https://github.com/${REPO}/releases/download/${VERSION}/${assetName}`;

  process.stderr.write(`docket: fetching ${VERSION} for ${t}\n`);
  const archive = await download(url);
  await verify(archive, assetName);

  const dir = path.dirname(binPath);
  fs.mkdirSync(dir, { recursive: true });
  const archivePath = path.join(dir, assetName);
  fs.writeFileSync(archivePath, archive);

  // tar ships with macOS, Linux and Windows 10 and later; unpacking in process
  // would mean a dependency, and this wrapper is meant to have none.
  try {
    execFileSync("tar", ["-xf", archivePath, "-C", dir], { stdio: "ignore" });
  } catch (err) {
    fail(`could not unpack ${assetName}: ${err.message}`);
  }
  fs.rmSync(archivePath, { force: true });

  const unpacked = path.join(dir, `docket_${VERSION}_${t}`, windows ? "docket.exe" : "docket");
  if (!fs.existsSync(unpacked)) fail(`the archive did not contain a docket binary`);
  fs.renameSync(unpacked, binPath);
  fs.rmSync(path.join(dir, `docket_${VERSION}_${t}`), { recursive: true, force: true });
  if (!windows) fs.chmodSync(binPath, 0o755);
}

async function main() {
  const binPath = path.join(cacheDir(), process.platform === "win32" ? "docket.exe" : "docket");
  if (!fs.existsSync(binPath)) {
    await install(binPath);
  }
  const child = spawn(binPath, process.argv.slice(2), { stdio: "inherit" });
  child.on("error", (err) => fail(err.message));
  child.on("exit", (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code ?? 0);
  });
}

main().catch((err) => fail(err.message));
