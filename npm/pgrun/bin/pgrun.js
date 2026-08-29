#!/usr/bin/env node
'use strict';

// pgrun npm wrapper — locates the prebuilt Go binary for this platform (shipped
// as an optionalDependency: one @pgrun/<platform>-<arch> package per target that
// npm installs only when its os/cpu match) and execs it. It passes argv, stdio,
// signals, and the exit code through verbatim. ALL behaviour lives in the Go
// binary; this file stays pure plumbing.

const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

// pkgFor / exeName are the whole "locating" decision, factored out so the mapping
// is unit-testable independently of actually spawning anything.
function pkgFor(platform, arch) {
  return `@pgrun/${platform}-${arch}`;
}
function exeName(platform) {
  return platform === 'win32' ? 'pgrun.exe' : 'pgrun';
}

function resolveBinary(platform, arch) {
  const pkg = pkgFor(platform, arch);
  try {
    const pkgJson = require.resolve(`${pkg}/package.json`);
    return path.join(path.dirname(pkgJson), 'bin', exeName(platform));
  } catch (_) {
    return null;
  }
}

function main() {
  const platform = process.platform; // 'linux' | 'darwin' | 'win32' | ...
  const arch = process.arch; // 'x64' | 'arm64' | ...

  const binPath = resolveBinary(platform, arch);
  if (!binPath || !fs.existsSync(binPath)) {
    process.stderr.write(
      `pgrun: no prebuilt binary for your platform (${platform}-${arch}).\n` +
        `pgrun ships binaries for linux, darwin, and win32 on x64 and arm64.\n` +
        `If you ran npx moments after a release, npm may have cached an install\n` +
        `made before your platform's package propagated — retry with a fresh\n` +
        `cache: npx --cache "$(mktemp -d)" -y @pgrun/cli version\n` +
        `Install another way: https://github.com/pgrundev/pgrun#install\n`
    );
    process.exit(64); // usage/environment error, per pgrun's exit-code contract
  }

  // npm does not reliably preserve the executable bit through packing; restore it
  // rather than relying on a forbidden postinstall script.
  if (platform !== 'win32') {
    try {
      fs.chmodSync(binPath, 0o755);
    } catch (_) {
      /* best effort */
    }
  }

  const child = spawn(binPath, process.argv.slice(2), { stdio: 'inherit' });

  // Forward termination signals so the Go binary's own signal handling still
  // fires when npx is interrupted.
  for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    process.on(sig, () => {
      try {
        child.kill(sig);
      } catch (_) {
        /* child already gone */
      }
    });
  }

  child.on('error', (err) => {
    process.stderr.write(`pgrun: failed to launch ${binPath}: ${err.message}\n`);
    process.exit(1); // operation failure, per the contract
  });

  child.on('exit', (code, signal) => {
    if (signal) {
      // Re-raise so our exit status reflects the signal (128+n by convention),
      // matching what the child experienced.
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code === null ? 1 : code);
  });
}

if (require.main === module) {
  main();
} else {
  module.exports = { pkgFor, exeName, resolveBinary };
}
