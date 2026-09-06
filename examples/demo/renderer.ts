// The JS half of the Go demo: renders pages, and sends every rpc() call to Go.
//
// This is the entire renderer. It owns no routing decisions of its own —
// createRscHandler does that from the manifest the build wrote — and it holds
// no data, because the data lives behind the host-call endpoint.
//
//   bun run renderer.ts --bundle <dir> --host-call <url> --secret <s> --port <n>

// An app writes these as '@rsc-kit/core/host', '@rsc-kit/core/host-calls' and
// '@rsc-kit/core/files'. This one is inside the repo that publishes them, and
// the monorepo has no self-link in node_modules, so it reaches the source
// directly — the only difference between this file and a real one.
import { createRscHandler } from '../../../../packages/core/src/host'
import { httpHostCalls } from '../../../../packages/core/src/hostCalls'
import { assetsFrom } from '../../../../packages/core/src/files'
import { join } from 'node:path'

function arg(name: string, fallback = ''): string {
  const i = process.argv.indexOf(`--${name}`)
  return i === -1 ? fallback : (process.argv[i + 1] ?? fallback)
}

const bundleDir = arg('bundle')
const hostCallUrl = arg('host-call')
const secret = arg('secret')
const port = Number(arg('port', '5273'))

if (!bundleDir || !hostCallUrl || !secret) {
  console.error('need --bundle <dir> --host-call <url> --secret <s>')
  process.exit(1)
}

const engine = await import(join(bundleDir, 'dist/rsc/index.js'))

const handle = createRscHandler({
  engine,
  // The browser's root, not the asset folder — /assets/x.js reads
  // <dir>/assets/x.js. Passing the folder itself 404s every asset, and the
  // page still renders, so the only symptom is that nothing hydrates.
  assets: assetsFrom(join(bundleDir, 'public')),
  hostCalls: httpHostCalls({ endpoint: hostCallUrl, secret }),
  version: 'demo-1',
})

Bun.serve({
  port,
  idleTimeout: 60,
  fetch: async (request) => (await handle(request)) ?? new Response('Not found', { status: 404 }),
})

console.log(`renderer listening on http://127.0.0.1:${port}`)
