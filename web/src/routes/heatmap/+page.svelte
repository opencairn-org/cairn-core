<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { Map as MapLibreMap, LngLatBounds, type GeoJSONSource } from 'maplibre-gl';
	import type { FeatureCollection } from 'geojson';
	import 'maplibre-gl/dist/maplibre-gl.css';
	import { basemapStyle, basemapVariants, type BasemapVariant } from '#lib/basemap.js';
	import ActivityFilterBar from '#lib/components/ActivityFilterBar.svelte';
	import { ActivityFilter, type ActivityFacets } from '#lib/activity-filter.svelte.js';

	// Every GPS track the filter matches, drawn as translucent lines so the
	// routes you ride most burn brightest.
	type Heatmap = {
		matched: number;
		with_track: number;
		truncated: boolean;
		facets: ActivityFacets;
		geojson: FeatureCollection;
	};
	const EMPTY: FeatureCollection = { type: 'FeatureCollection', features: [] };

	const filter = new ActivityFilter(page.url.searchParams);
	let data = $state<Heatmap | null>(null);
	let loading = $state(false);
	let error = $state<string | null>(null);

	// View settings (basemap, track color) are per-browser conveniences kept in
	// localStorage; blocked storage just means the defaults.
	function loadSetting(key: string): string | null {
		try {
			return localStorage.getItem(key);
		} catch {
			return null;
		}
	}
	function saveSetting(key: string, value: string) {
		try {
			localStorage.setItem(key, value);
		} catch {
			/* storage blocked */
		}
	}

	// Basemap: a dark, low-detail map by default so the tracks stand out.
	const BASEMAP_KEY = 'cairn.heatmap.basemap';
	const BASEMAP_LABELS: Record<BasemapVariant, string> = {
		dark: 'Dark',
		light: 'Light',
		standard: 'Standard'
	};
	const variants = basemapVariants();
	const savedBasemap = loadSetting(BASEMAP_KEY);
	let basemap = $state<BasemapVariant>(
		variants.find((v) => v === savedBasemap) ?? variants[0] ?? 'standard'
	);

	function setBasemap(v: BasemapVariant) {
		if (v === basemap) return;
		basemap = v;
		saveSetting(BASEMAP_KEY, v);
		// A full reload (no diff) so style.load fires and re-adds the tracks.
		map?.setStyle(basemapStyle(v), { diff: false });
	}

	// Track color: a hue slider at the default's saturation and lightness, so
	// every choice stays as vivid as the original orange, which the default hue
	// maps to exactly.
	const HUE_KEY = 'cairn.heatmap.trackHue';
	const DEFAULT_HUE = 19;
	const DEFAULT_COLOR = '#ec7a45'; // ≈ hsl(19, 81%, 60%)
	const TRACK_LAYERS = ['tracks-glow', 'tracks-line'];
	const savedHue = Number(loadSetting(HUE_KEY) ?? DEFAULT_HUE);
	let trackHue = $state(
		Number.isInteger(savedHue) && savedHue >= 0 && savedHue < 360 ? savedHue : DEFAULT_HUE
	);
	const trackColor = $derived(
		trackHue === DEFAULT_HUE ? DEFAULT_COLOR : `hsl(${trackHue}, 81%, 60%)`
	);

	function setTrackHue(h: number) {
		trackHue = h;
		for (const id of TRACK_LAYERS) {
			if (map?.getLayer(id)) map.setPaintProperty(id, 'line-color', trackColor);
		}
	}

	let container = $state<HTMLDivElement | null>(null);
	let map: MapLibreMap | null = null;
	let mapLoaded = false;
	let fitted = false;

	function bounds(fc: FeatureCollection): LngLatBounds | null {
		let b: LngLatBounds | null = null;
		for (const f of fc.features) {
			if (f.geometry.type !== 'LineString') continue;
			for (const c of f.geometry.coordinates) {
				const [lng, lat] = c;
				if (lng === undefined || lat === undefined) continue;
				const ll: [number, number] = [lng, lat];
				b = b ? b.extend(ll) : new LngLatBounds(ll, ll);
			}
		}
		return b;
	}

	function applyData() {
		if (!map || !mapLoaded) return;
		const fc = data?.geojson ?? EMPTY;
		(map.getSource('tracks') as GeoJSONSource | undefined)?.setData(fc);
		// Frame the tracks on the first result only; later filter changes keep
		// the viewport the user has set up.
		if (!fitted && fc.features.length > 0) fitTracks();
	}

	function fitTracks() {
		if (!map) return;
		const b = bounds(data?.geojson ?? EMPTY);
		if (!b) return;
		map.fitBounds(b, { padding: 40, duration: 0, maxZoom: 14 });
		fitted = true;
	}

	let seq = 0;
	let abort: AbortController | null = null;
	async function load() {
		const s = ++seq;
		abort?.abort();
		const ctrl = new AbortController();
		abort = ctrl;
		loading = true;
		error = null;
		try {
			const res = await fetch(`/api/activities/heatmap?${filter.params().toString()}`, {
				signal: ctrl.signal
			});
			if (!res.ok) throw new Error(await res.text());
			if (s !== seq) return;
			data = (await res.json()) as Heatmap;
			applyData();
		} catch (e) {
			if ((e as Error).name !== 'AbortError') error = (e as Error).message || 'failed to load';
		} finally {
			if (s === seq) loading = false;
		}
	}

	// Chip/select edits re-query at once; dates and ranges after a debounce.
	let debounced = $state(0);
	let debounceTimer: ReturnType<typeof setTimeout> | undefined;
	let debounceInit = false;
	$effect(() => {
		void filter.textKey;
		if (!debounceInit) {
			debounceInit = true;
			return;
		}
		clearTimeout(debounceTimer);
		debounceTimer = setTimeout(() => (debounced += 1), 350);
	});
	$effect(() => {
		void filter.immediateKey;
		void debounced;
		load();
	});

	// Tracks source + layers. Runs on every style load: setStyle() drops
	// everything the new style doesn't define.
	function addTracks() {
		if (!map) return;
		map.addSource('tracks', { type: 'geojson', data: EMPTY });
		// Two passes: a wide, faint glow that accumulates where tracks overlap,
		// and a thin core so single passes still read as a line.
		map.addLayer({
			id: 'tracks-glow',
			type: 'line',
			source: 'tracks',
			layout: { 'line-cap': 'round', 'line-join': 'round' },
			paint: { 'line-color': trackColor, 'line-width': 7, 'line-opacity': 0.07, 'line-blur': 3 }
		});
		map.addLayer({
			id: 'tracks-line',
			type: 'line',
			source: 'tracks',
			layout: { 'line-cap': 'round', 'line-join': 'round' },
			paint: { 'line-color': trackColor, 'line-width': 1.6, 'line-opacity': 0.35 }
		});
		mapLoaded = true;
		applyData();
	}

	onMount(() => {
		if (!container) return;
		map = new MapLibreMap({
			container,
			style: basemapStyle(basemap),
			center: [10, 50],
			zoom: 4,
			attributionControl: { compact: true }
		});
		map.on('style.load', addTracks);
		// Layer-bound listeners resolve the layer per event, so they survive
		// style swaps.
		map.on('mouseenter', 'tracks-line', () => map && (map.getCanvas().style.cursor = 'pointer'));
		map.on('mouseleave', 'tracks-line', () => map && (map.getCanvas().style.cursor = ''));
		map.on('click', 'tracks-line', (e) => {
			const id = e.features?.[0]?.properties?.id;
			if (id) goto(`/activities/${id}`);
		});
	});
	onDestroy(() => {
		abort?.abort();
		map?.remove();
		map = null;
	});
</script>

<section class="space-y-6">
	<header class="flex items-center justify-between gap-4 max-md:flex-wrap">
		<div>
			<h1 class="text-3xl font-semibold tracking-tight max-md:text-2xl">Heatmap</h1>
			{#if data}
				<p class="mt-1 text-sm text-zinc-400">
					<span class="font-medium text-zinc-200">{data.with_track}</span> tracks from {data.matched}
					{#if filter.active}matching{/if} activities{#if data.truncated}
						<span class="text-amber-400">(newest {data.with_track} shown)</span>{/if}
				</p>
			{/if}
		</div>
		<div class="flex flex-wrap items-center gap-2">
			{#if variants.length > 1}
				<div
					role="group"
					aria-label="Basemap"
					class="flex overflow-hidden rounded border border-zinc-700 text-xs"
				>
					{#each variants as v (v)}
						<button
							type="button"
							aria-pressed={basemap === v}
							onclick={() => setBasemap(v)}
							class="px-2.5 py-1.5 transition-colors"
							class:bg-zinc-800={basemap === v}
							class:text-accent-300={basemap === v}
							class:text-zinc-400={basemap !== v}
							class:hover:text-zinc-100={basemap !== v}
						>
							{BASEMAP_LABELS[v]}
						</button>
					{/each}
				</div>
			{/if}
			<div class="flex items-center gap-1">
				<input
					type="range"
					min="0"
					max="359"
					step="1"
					class="hue w-28"
					aria-label="Track color"
					title="Track color"
					style:--thumb={trackColor}
					value={trackHue}
					oninput={(e) => setTrackHue(e.currentTarget.valueAsNumber)}
					onchange={() => saveSetting(HUE_KEY, String(trackHue))}
				/>
				<button
					type="button"
					class="rounded p-1 text-zinc-500 hover:text-zinc-200"
					class:invisible={trackHue === DEFAULT_HUE}
					disabled={trackHue === DEFAULT_HUE}
					aria-label="Reset track color"
					title="Reset track color"
					onclick={() => {
						setTrackHue(DEFAULT_HUE);
						saveSetting(HUE_KEY, String(DEFAULT_HUE));
					}}
				>
					<svg
						class="h-3.5 w-3.5"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="2"
						stroke-linecap="round"
						stroke-linejoin="round"
					>
						<path d="M3 12a9 9 0 1 0 3-6.7L3 8 M3 3v5h5" />
					</svg>
				</button>
			</div>
			<button
				type="button"
				onclick={fitTracks}
				disabled={!data || data.with_track === 0}
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				Fit to tracks
			</button>
		</div>
	</header>

	<ActivityFilterBar {filter} facets={data?.facets ?? null} showSort={false} />

	{#if error}
		<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
			{error}
		</div>
	{/if}

	<div class="relative">
		<div
			bind:this={container}
			class="h-[70vh] min-h-96 w-full overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900"
		></div>
		{#if loading}
			<div
				class="pointer-events-none absolute top-3 left-3 rounded bg-zinc-900/80 px-2 py-1 text-xs text-zinc-300"
			>
				Loading tracks…
			</div>
		{:else if data && data.with_track === 0}
			<div class="pointer-events-none absolute inset-0 flex items-center justify-center">
				<p class="rounded bg-zinc-900/80 px-4 py-2 text-sm text-zinc-400">
					{filter.active ? 'No GPS tracks match this filter.' : 'No GPS tracks yet.'}
				</p>
			</div>
		{/if}
	</div>
	<p class="text-xs text-zinc-500">Click a track to open the activity.</p>
</section>

<style>
	/* Hue slider: the track shows the spectrum, the thumb the chosen color. */
	.hue {
		appearance: none;
		height: 6px;
		border-radius: 9999px;
		cursor: pointer;
		background: linear-gradient(
			to right,
			hsl(0, 81%, 60%),
			hsl(60, 81%, 60%),
			hsl(120, 81%, 60%),
			hsl(180, 81%, 60%),
			hsl(240, 81%, 60%),
			hsl(300, 81%, 60%),
			hsl(359, 81%, 60%)
		);
	}
	.hue::-webkit-slider-thumb {
		appearance: none;
		width: 14px;
		height: 14px;
		border-radius: 9999px;
		background: var(--thumb);
		border: 2px solid #f4f4f5;
		box-shadow: 0 0 0 1px rgb(0 0 0 / 0.5);
	}
	.hue::-moz-range-track {
		background: transparent;
	}
	.hue::-moz-range-thumb {
		width: 10px;
		height: 10px;
		border-radius: 9999px;
		background: var(--thumb);
		border: 2px solid #f4f4f5;
		box-shadow: 0 0 0 1px rgb(0 0 0 / 0.5);
	}
	.hue:focus-visible {
		outline: 2px solid var(--color-accent-400);
		outline-offset: 4px;
	}
</style>
