import { afterEach, describe, expect, it, vi } from 'vitest';

const calls = vi.hoisted(() => [] as unknown[]);
vi.mock('maplibre-gl', () => ({ setWorkerUrl: (u: unknown) => calls.push(u) }));
vi.mock('maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url', () => ({ default: '/w.mjs' }));

import { basemapStyle, basemapVariants } from './basemap';

afterEach(() => vi.unstubAllEnvs());

describe('basemap', () => {
	it('pins the maplibre worker to the bundled asset on import', () => {
		expect(calls).toEqual(['/w.mjs']);
	});

	it('defaults to OpenFreeMap', () => {
		vi.stubEnv('VITE_MAP_STYLE_URL', '');
		expect(basemapStyle()).toBe('https://tiles.openfreemap.org/styles/liberty');
	});

	it('honours VITE_MAP_STYLE_URL', () => {
		vi.stubEnv('VITE_MAP_STYLE_URL', 'https://example.test/style.json');
		expect(basemapStyle()).toBe('https://example.test/style.json');
	});
});

describe('basemap variants', () => {
	const OFM = 'https://tiles.openfreemap.org/styles/';

	it('defaults the overlay variants to OpenFreeMap monochrome styles', () => {
		vi.stubEnv('VITE_MAP_STYLE_URL', '');
		vi.stubEnv('VITE_MAP_STYLE_DARK_URL', '');
		vi.stubEnv('VITE_MAP_STYLE_LIGHT_URL', '');
		expect(basemapStyle('dark')).toBe(`${OFM}dark`);
		expect(basemapStyle('light')).toBe(`${OFM}positron`);
		expect(basemapStyle('standard')).toBe(`${OFM}liberty`);
		expect(basemapVariants()).toEqual(['dark', 'light', 'standard']);
	});

	it('honours per-variant overrides', () => {
		vi.stubEnv('VITE_MAP_STYLE_DARK_URL', 'https://example.test/dark.json');
		vi.stubEnv('VITE_MAP_STYLE_LIGHT_URL', 'https://example.test/light.json');
		expect(basemapStyle('dark')).toBe('https://example.test/dark.json');
		expect(basemapStyle('light')).toBe('https://example.test/light.json');
	});

	it('falls back to an operator default instead of OpenFreeMap', () => {
		vi.stubEnv('VITE_MAP_STYLE_URL', 'https://example.test/style.json');
		vi.stubEnv('VITE_MAP_STYLE_DARK_URL', '');
		vi.stubEnv('VITE_MAP_STYLE_LIGHT_URL', '');
		expect(basemapStyle('dark')).toBe('https://example.test/style.json');
		expect(basemapStyle('light')).toBe('https://example.test/style.json');
		// Nothing distinct to choose from.
		expect(basemapVariants()).toEqual(['dark']);
	});

	it('collapses only the variants that resolve to the same style', () => {
		vi.stubEnv('VITE_MAP_STYLE_URL', 'https://example.test/style.json');
		vi.stubEnv('VITE_MAP_STYLE_DARK_URL', 'https://example.test/dark.json');
		vi.stubEnv('VITE_MAP_STYLE_LIGHT_URL', '');
		expect(basemapVariants()).toEqual(['dark', 'light']);
	});
});
