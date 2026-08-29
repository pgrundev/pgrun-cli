#!/usr/bin/env node
// Assemble the npm publish tree from goreleaser's build output. Reads
// dist/artifacts.json, copies each Binary into its own @pgrun/<platform> package,
// and writes the wrapper with matching versions and pinned optionalDependencies.
// No network and no publish — that is the release workflow's job (not present
// here; publishing is deferred). The Go binaries are used as-is; this is
// packaging only.
//
//   node npm/build.mjs <version>
//   DIST=path OUT=path node npm/build.mjs <version>   # for tests

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const npmDir = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(npmDir, '..');

const version = (process.env.PGRUN_VERSION || process.argv[2] || '').replace(/^v/, '');
if (!version) {
  console.error('usage: build.mjs <version>   (or set PGRUN_VERSION)');
  process.exit(2);
}
const dist = process.env.DIST || path.join(root, 'dist');
const out = process.env.OUT || path.join(npmDir, 'staging');

// goreleaser goos/goarch -> Node process.platform / process.arch.
const NODE_OS = { linux: 'linux', darwin: 'darwin', windows: 'win32' };
const NODE_ARCH = { amd64: 'x64', arm64: 'arm64', '386': 'ia32' };

const artifacts = JSON.parse(fs.readFileSync(path.join(dist, 'artifacts.json'), 'utf8'));
const bins = artifacts.filter((a) => a.type === 'Binary');
if (bins.length === 0) {
  console.error('no Binary artifacts found in artifacts.json');
  process.exit(1);
}

fs.rmSync(out, { recursive: true, force: true });

const written = [];
for (const b of bins) {
  const os = NODE_OS[b.goos];
  const cpu = NODE_ARCH[b.goarch];
  if (!os || !cpu) {
    console.error(`skipping unsupported target ${b.goos}/${b.goarch}`);
    continue;
  }
  const key = `${os}-${cpu}`;
  const exe = os === 'win32' ? 'pgrun.exe' : 'pgrun';
  const pkgDir = path.join(out, '@pgrun', key);
  fs.mkdirSync(path.join(pkgDir, 'bin'), { recursive: true });

  const src = path.isAbsolute(b.path) ? b.path : path.join(root, b.path);
  const dst = path.join(pkgDir, 'bin', exe);
  fs.copyFileSync(src, dst);
  if (os !== 'win32') fs.chmodSync(dst, 0o755);

  writeJSON(path.join(pkgDir, 'package.json'), {
    name: `@pgrun/${key}`,
    version,
    description: `pgrun prebuilt binary for ${key}`,
    os: [os],
    cpu: [cpu],
    files: ['bin/'],
    license: 'MIT',
    repository: { type: 'git', url: 'git+https://github.com/pgrundev/pgrun.git' },
  });
  written.push(key);
}

// The wrapper: copy the shim + README, inject the release version everywhere.
const wrapSrc = path.join(npmDir, 'pgrun');
const wrapOut = path.join(out, 'pgrun');
fs.mkdirSync(path.join(wrapOut, 'bin'), { recursive: true });
fs.copyFileSync(path.join(wrapSrc, 'bin', 'pgrun.js'), path.join(wrapOut, 'bin', 'pgrun.js'));
if (fs.existsSync(path.join(wrapSrc, 'README.md'))) {
  fs.copyFileSync(path.join(wrapSrc, 'README.md'), path.join(wrapOut, 'README.md'));
}
const wrapPkg = JSON.parse(fs.readFileSync(path.join(wrapSrc, 'package.json'), 'utf8'));
wrapPkg.version = version;
for (const dep of Object.keys(wrapPkg.optionalDependencies || {})) {
  wrapPkg.optionalDependencies[dep] = version;
}
writeJSON(path.join(wrapOut, 'package.json'), wrapPkg);

console.log(`assembled npm tree ${version}: pgrun + ${written.map((k) => '@pgrun/' + k).join(', ')}`);

function writeJSON(p, obj) {
  fs.writeFileSync(p, JSON.stringify(obj, null, 2) + '\n');
}
