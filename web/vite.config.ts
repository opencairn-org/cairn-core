import { readFileSync } from 'node:fs';
import adapterNode from '@sveltejs/adapter-node';
import adapterStatic from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { paraglideVitePlugin } from '@inlang/paraglide-js';
import { defineConfig } from 'vitest/config';

// Where the Go server listens during dev. Override with CAIRN_API_ORIGIN when
// :8080 is taken (e.g. `export CAIRN_API_ORIGIN=http://localhost:8090` to match
// CAIRN_HTTP_ADDR=0.0.0.0:8090). `make dev-web` sources dev.env, which sets it.
const apiOrigin = process.env.CAIRN_API_ORIGIN || 'http://localhost:8080';

// Dev (default): adapter-node — the SvelteKit Node server, SSR on, run in
// the cairn-web container behind Caddy.
//
// Prod: CAIRN_WEB_TARGET=static → adapter-static, a self-contained SPA
// (fallback index.html) that the Go binary embeds and serves. Pages opt
// out of SSR for this target via src/routes/+layout.ts so there's nothing
// to prerender against a live backend at build time.
const target = process.env.CAIRN_WEB_TARGET ?? 'node';

const adapter =
	target === 'static'
		? adapterStatic({
				pages: 'build',
				assets: 'build',
				fallback: 'index.html',
				precompress: false,
				strict: false
			})
		: adapterNode({ out: 'build' });

export default defineConfig({
	plugins: [
		tailwindcss(),
		paraglideVitePlugin({
			project: './project.inlang',
			outdir: './src/lib/paraglide',
			// cookie + Accept-Language header + baseLocale fallback.
			// No URL strategy: the operator UI lives on a single path tree
			// and the cookie + header are enough.
			strategy: ['cookie', 'preferredLanguage', 'baseLocale']
		}),
		sveltekit({ preprocess: vitePreprocess(), adapter })
	],
	server: {
		// Local HTTPS lets phones use the dev web app/PWA over the LAN.
		// Generate with make dev-web-cert; opt in with make dev-web HTTPS=1.
		// Production TLS is handled by the reverse proxy.
		...(process.env.CAIRN_WEB_HTTPS === '1'
			? {
					https: {
						cert: readFileSync(new URL('../.certs/dev-cert.pem', import.meta.url)),
						key: readFileSync(new URL('../.certs/dev-key.pem', import.meta.url))
					}
				}
			: {}),
		// CAIRN_WEB_PORT lets this dev server avoid colliding with another
		// project's vite on the same origin (a shared :5173 mixes module graphs
		// in the browser). `make dev-web` sources dev.env, which sets it.
		port: Number(process.env.CAIRN_WEB_PORT) || 5173,
		strictPort: true,
		proxy: {
			// Proxy backend-owned URL prefixes to the Go server during dev.
			// In production a reverse proxy (Caddy / nginx) does the same
			// split. NB: `/admin` is intentionally NOT proxied — the
			// SvelteKit /admin route lives at that path and is reached
			// via the Connect-RPC cairn.v1.AdminService instead of the
			// legacy /admin/* HTTP endpoints. Operators who want the
			// legacy endpoints (smoketest, manual curl) hit the Go server
			// directly on :8080.
			'/auth': apiOrigin,
			'/api': apiOrigin,
			'/webhooks': apiOrigin,
			'/cairn.v1.': apiOrigin,
			'/cairn.worker.v1.': apiOrigin,
			// OAuth 2.1 authorization server (backend). NB: /oauth/consent is a
			// SvelteKit route (the consent UI), so it is intentionally NOT
			// proxied — only the backend endpoints below are.
			'/oauth/authorize': apiOrigin,
			'/oauth/token': apiOrigin,
			'/oauth/register': apiOrigin,
			'/oauth/revoke': apiOrigin,
			'/.well-known': apiOrigin,
			// MCP server (OAuth-protected, read-only).
			'/mcp': apiOrigin
		}
	},
	test: {
		include: ['src/**/*.test.ts'],
		environment: 'node',
		coverage: {
			provider: 'v8',
			// Pure TS modules; Svelte components and route loaders are covered by svelte-check only.
			include: ['src/lib/*.ts', 'src/routes/**/streamSeries.ts'],
			thresholds: { lines: 95, functions: 95, branches: 95, statements: 95 }
		}
	}
});
