import { setWorkerUrl, type StyleSpecification } from 'maplibre-gl';
import workerUrl from 'maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url';

// MapLibre ≥6 resolves its worker as `./maplibre-gl-worker.mjs` relative to
// import.meta.url at runtime. In the production bundle that is a hashed chunk
// directory where no such file exists, so the worker 404s and the map stays
// blank (no vector tiles, no GeoJSON — both are parsed in the worker). Vite
// dev serves node_modules directly, which is why it only breaks in prod. Pin
// the worker to the Vite-bundled asset instead.
setWorkerUrl(workerUrl);

// Default basemap for every interactive map. OpenFreeMap serves OpenMapTiles
// vector styles without an API key (CARTO's free raster basemap started
// watermarking tiles with "API KEY REQUIRED" in 2026-08). Operators can point
// VITE_MAP_STYLE_URL at any MapLibre style (MapTiler, keyed CARTO, self-hosted).
// Attribution comes from the style's own sources (OpenFreeMap's TileJSON
// carries it), so none is added here.
const OPENFREEMAP = 'https://tiles.openfreemap.org/styles/';

// Low-detail monochrome variants for data overlays (the heatmap), where the
// standard basemap's colours compete with the tracks. Each is overridable at
// build time like the default (VITE_MAP_STYLE_DARK_URL / _LIGHT_URL).
export type BasemapVariant = 'dark' | 'light' | 'standard';

function env(name: string): string {
	return (import.meta.env[name] as string | undefined) ?? '';
}

export function basemapStyle(variant: BasemapVariant = 'standard'): string | StyleSpecification {
	const own = env('VITE_MAP_STYLE_URL');
	if (variant === 'standard') return own || `${OPENFREEMAP}liberty`;
	const override = env(variant === 'dark' ? 'VITE_MAP_STYLE_DARK_URL' : 'VITE_MAP_STYLE_LIGHT_URL');
	// An operator who moved the default off OpenFreeMap gets no OpenFreeMap
	// requests they didn't opt into: unset variants fall back to their style.
	return override || own || `${OPENFREEMAP}${variant === 'dark' ? 'dark' : 'positron'}`;
}

// The variants worth offering, best overlay contrast first; ones that resolve
// to the same style collapse, so a single-style setup offers no choice.
export function basemapVariants(): BasemapVariant[] {
	const seen = new Set<string | StyleSpecification>();
	return (['dark', 'light', 'standard'] as const).filter((v) => {
		const s = basemapStyle(v);
		if (seen.has(s)) return false;
		seen.add(s);
		return true;
	});
}
