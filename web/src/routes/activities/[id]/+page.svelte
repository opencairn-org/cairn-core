<script lang="ts">
	import { m } from '#lib/paraglide/messages.js';
	import {
		formatDate,
		formatDistance,
		formatDuration,
		formatElevation,
		formatHeartRate,
		formatPace,
		formatPower,
		formatSpeed,
		formatTemp
	} from '#lib/format.js';
	import ActivityMap from '#lib/components/ActivityMap.svelte';
	import StreamChart from '#lib/components/StreamChart.svelte';
	import ElevationProfile from '#lib/components/ElevationProfile.svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import KudosComments from '#lib/components/KudosComments.svelte';
	import GearPrompt from '#lib/components/GearPrompt.svelte';
	import ActivityNav from '#lib/components/ActivityNav.svelte';
	import { onMount } from 'svelte';
	import { goto, refreshAll } from '$app/navigation';
	import { buildSeries } from './streamSeries';
	import type { PageData } from './$types';
	import type { SourceView } from './+layout';

	let { data }: { data: PageData } = $props();

	let a = $derived(data.activity);
	let stream = $derived(data.stream);
	let streamStatus = $derived(data.streamStatus);
	let bestEfforts = $derived(data.bestEfforts);

	type SegEffort = {
		segment_id: string;
		segment_name: string;
		segment_distance_m: number;
		climb_category: string | null;
		elapsed_s: number;
		avg_heart_rate: number | null;
		avg_power: number | null;
		is_personal_record: boolean;
		start_distance_m?: number;
		end_distance_m?: number;
	};
	let segmentEfforts = $state<SegEffort[]>([]);
	// Segment effort the pointer is over — shades its span on the elevation profile.
	let hoveredSeg = $state<SegEffort | null>(null);
	const segHighlight = $derived(
		hoveredSeg && hoveredSeg.start_distance_m != null && hoveredSeg.end_distance_m != null
			? { startDist: hoveredSeg.start_distance_m, endDist: hoveredSeg.end_distance_m }
			: null
	);

	type ZoneBand = { label: string; low_pct: number; high_pct: number; seconds: number };
	type ZoneSet = { basis: string; reference: number; total_seconds: number; zones: ZoneBand[] };
	type Decoupling = { pct: number; basis: string; coupled: boolean };
	type Zones = { hr: ZoneSet | null; power: ZoneSet | null; decoupling?: Decoupling };
	let zones = $state<Zones | null>(null);

	type Attachment = { id: string; url: string; caption: string; content_type: string };
	let attachments = $state<Attachment[]>([]);
	let uploadingPhoto = $state(false);
	let photoInput = $state<HTMLInputElement | null>(null);

	async function loadAttachments() {
		try {
			const res = await fetch(`/api/activities/${a.id}/attachments`);
			if (res.ok) attachments = (await res.json()).attachments ?? [];
		} catch {
			/* ignore */
		}
	}

	async function uploadPhotos(files: FileList | null) {
		if (!files || files.length === 0) return;
		uploadingPhoto = true;
		try {
			for (const file of Array.from(files)) {
				const fd = new FormData();
				fd.append('file', file);
				await fetch(`/api/activities/${a.id}/attachments`, { method: 'POST', body: fd });
			}
			await loadAttachments();
		} finally {
			uploadingPhoto = false;
		}
	}

	async function deleteAttachment(id: string) {
		const res = await fetch(`/api/activities/${a.id}/attachments/${id}`, { method: 'DELETE' });
		if (res.ok) attachments = attachments.filter((x) => x.id !== id);
	}

	onMount(async () => {
		try {
			const res = await fetch(`/api/activities/${a.id}/segment-efforts`);
			if (res.ok) segmentEfforts = (await res.json()).efforts ?? [];
		} catch {
			/* ignore */
		}
		try {
			const res = await fetch(`/api/activities/${a.id}/zones`);
			if (res.ok) zones = await res.json();
		} catch {
			/* ignore */
		}
		try {
			const res = await fetch(`/api/activities/${a.id}/laps`);
			if (res.ok) laps = (await res.json()).laps ?? [];
		} catch {
			/* ignore */
		}
		await loadAttachments();
	});

	type Lap = {
		index: number;
		label: string;
		elapsed_s: number;
		moving_s: number;
		distance_m: number | null;
		avg_speed_mps: number | null;
		avg_heart_rate: number | null;
		avg_power: number | null;
		avg_cadence: number | null;
		elevation_gain_m: number | null;
	};
	let laps = $state<Lap[]>([]);
	const anyLapPower = $derived(laps.some((l) => l.avg_power != null));

	// Distinct colour per zone index (Z1 cool → Z7 hot).
	const ZONE_COLORS = ['#3b82f6', '#22c55e', '#eab308', '#f97316', '#ef4444', '#b91c1c', '#7f1d1d'];
	function zonePct(z: ZoneSet, seconds: number): number {
		return z.total_seconds > 0 ? (seconds / z.total_seconds) * 100 : 0;
	}
	const hasZones = $derived(
		!!zones &&
			((zones.hr && zones.hr.total_seconds > 0) || (zones.power && zones.power.total_seconds > 0))
	);

	function formatEffortWindow(b: (typeof bestEfforts)[number]): string {
		if (b.windowKind === 'distance') {
			return b.windowValue >= 1000 ? `${b.windowValue / 1000} km` : `${b.windowValue} m`;
		}
		if (b.windowKind === 'duration') {
			if (b.windowValue >= 3600) return `${Math.round(b.windowValue / 3600)} h`;
			if (b.windowValue >= 60) return `${Math.round(b.windowValue / 60)} min`;
			return `${b.windowValue} s`;
		}
		return '—';
	}

	function formatEffortValue(b: (typeof bestEfforts)[number]): string {
		const v = b.achievedValue;
		switch (b.metric) {
			case 'pace': {
				// seconds per km
				const m = Math.floor(v / 60);
				const s = Math.round(v % 60);
				return `${m}:${s.toString().padStart(2, '0')} /km`;
			}
			case 'speed':
				return `${(v * 3.6).toFixed(1)} km/h`;
			case 'power':
				return `${Math.round(v)} W`;
			case 'heart_rate':
				return `${Math.round(v)} bpm`;
			case 'vam':
				return `${Math.round(v)} m/h`;
		}
		return v.toFixed(0);
	}

	// --- sources / detach ---
	const activeSources = $derived((a.sources ?? []).filter((s) => s.status !== 'detached'));
	let detaching = $state<string | null>(null);
	let sourceError = $state<string | null>(null);

	async function detachSource(s: SourceView) {
		const last = activeSources.length <= 1;
		const msg = last
			? 'This is the only remaining source. Detaching it will delete this activity. Continue?'
			: `Detach the ${s.provider} source? The activity will re-merge from its remaining sources.`;
		if (!confirm(msg)) return;
		detaching = s.id;
		sourceError = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/sources/${s.id}/detach`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ reason: 'user_detach' })
			});
			if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
			const body = await res.json();
			if (body.soft_deleted) {
				await goto('/activities');
				return;
			}
			await refreshAll();
		} catch (e) {
			sourceError = (e as Error).message;
		} finally {
			detaching = null;
		}
	}

	function humanizeStatus(v: string): string {
		return v.replace(/_/g, ' ');
	}

	const sourceCount = $derived(a.sources?.length ?? 0);
	const sourcesLabel = $derived(
		sourceCount === 1
			? m.detail_sources_count({ count: sourceCount })
			: m.detail_sources_count_plural({ count: sourceCount })
	);

	const coordinates = $derived(stream?.coordinates ?? []);
	const offsets = $derived(stream?.offsets ?? []);
	const track = $derived(stream?.track ?? []);
	const hasElevation = $derived(track.some((p) => p.eleM != null));

	// Shared hover position (seconds offset) syncing the map marker and the
	// streams chart cursor — set by whichever the pointer is over.
	let hoverT = $state<number | null>(null);

	// Build chart series — only include channels that actually carry data
	// for this activity. Shared with the full-screen streams subpage.
	const series = $derived(buildSeries(stream));

	// --- overview stats ---
	// Pace for foot/swim sports, speed for wheels.
	const speedSports = new Set(['ride', 'ebikeride', 'velomobile', 'handcycle', 'inline_skate']);
	const useSpeed = $derived(speedSports.has(a.type) || speedSports.has(a.discipline));

	// Primary metric next to distance/time/elevation: pace or avg speed.
	const primaryPace = $derived.by(() => {
		const s = a.summary;
		if (useSpeed) return { label: 'Avg speed', value: formatSpeed(s.avgSpeedMps) };
		// derive avg pace from distance/time when no avg_speed provided
		const mps =
			s.avgSpeedMps ?? (s.distanceM && a.movingDurationS ? s.distanceM / a.movingDurationS : null);
		return { label: 'Avg pace', value: formatPace(mps) };
	});

	// Secondary stats — only those that actually have a value are rendered.
	const secondaryStats = $derived.by(() => {
		const s = a.summary;
		const out: { label: string; value: string }[] = [];
		const push = (label: string, v: string) => {
			if (v && v !== '–' && v !== '—') out.push({ label, value: v });
		};
		push('Elapsed', formatDuration(a.elapsedDurationS));
		if (useSpeed) push('Max speed', formatSpeed(s.maxSpeedMps));
		else push('Avg speed', formatSpeed(s.avgSpeedMps));
		push('Avg HR', formatHeartRate(s.avgHeartRateBpm));
		push('Max HR', formatHeartRate(s.maxHeartRateBpm));
		push('Avg power', formatPower(s.avgPowerW));
		push('Max power', formatPower(s.maxPowerW));
		if (s.normalizedPowerW != null) push('NP', formatPower(s.normalizedPowerW));
		if (s.avgCadence != null) push('Avg cadence', `${s.avgCadence}`);
		if (s.maxCadence != null) push('Max cadence', `${s.maxCadence}`);
		if (s.caloriesKcal != null) push('Calories', `${s.caloriesKcal} kcal`);
		if (s.elevationLossM != null) push('Descent', formatElevation(s.elevationLossM));
		if (s.maxElevationM != null) push('Max elevation', formatElevation(s.maxElevationM));
		if (s.tss != null) push('TSS', s.tss.toFixed(0));
		if (s.intensityFactor != null) push('Intensity', s.intensityFactor.toFixed(2));
		if (s.avgTemperatureC != null) push('Avg temp', formatTemp(s.avgTemperatureC));
		return out;
	});

	// Achievements summary from already-loaded data.
	const prCount = $derived(segmentEfforts.filter((e) => e.is_personal_record).length);
	const segCount = $derived(segmentEfforts.length);
	const beCount = $derived(bestEfforts.length);
</script>

<section class="space-y-8">
	<header>
		<ActivityNav id={a.id} />
		<div class="mt-2 flex items-start gap-4">
			<span
				class="flex h-14 w-14 shrink-0 items-center justify-center rounded-xl bg-zinc-800 text-accent-300"
				title={a.discipline || a.type}
			>
				<SportIcon type={a.discipline || a.type} size={30} />
			</span>
			<div class="min-w-0">
				<h1 class="text-3xl font-semibold leading-tight tracking-tight max-md:text-2xl">
					{a.title || `${a.discipline || a.type} Activity`}
				</h1>
				<p class="mt-1 text-sm text-zinc-400">
					<span class="capitalize text-zinc-300">{a.discipline || a.type}</span>
					{#if a.customSubtype}· <span class="text-zinc-300">{a.customSubtype}</span>{/if}
					· {formatDate(a.startTime)}
					{#if a.startPlace}· {a.startPlace}{/if}
				</p>
				{#if !a.gearId && a.gearSetBy !== 'user' && data.gear.length > 0}
					<GearPrompt activityId={a.id} type={a.type} gear={data.gear} />
				{/if}
				{#if a.gearId}
					<p class="mt-1 text-sm text-zinc-400">
						<a href="/gear" class="text-zinc-300 hover:text-accent-300"
							>{a.gearName || m.gear_title()}</a
						>
						<span class="text-zinc-600">
							· {a.gearSetBy === 'user' ? m.gear_set_by_user() : m.gear_set_by_merge()}</span
						>
					</p>
				{/if}
				{#if a.isVirtual || a.isEbike || a.isCommute || a.isRace}
					<div class="mt-1.5 flex flex-wrap gap-1.5">
						{#if a.isRace}<span
								class="rounded bg-amber-500/15 px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wide text-amber-300"
								>Race</span
							>{/if}
						{#if a.isVirtual}<span
								class="rounded bg-sky-500/15 px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wide text-sky-300"
								>Virtual</span
							>{/if}
						{#if a.isCommute}<span
								class="rounded bg-zinc-500/15 px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wide text-zinc-300"
								>Commute</span
							>{/if}
						{#if a.isEbike}<span
								class="rounded bg-emerald-500/15 px-1.5 py-0.5 text-[11px] font-medium uppercase tracking-wide text-emerald-300"
								>E-bike</span
							>{/if}
					</div>
				{/if}
				<p class="mt-0.5 text-xs text-zinc-600">{a.timezone} · {sourcesLabel}</p>
			</div>
		</div>
	</header>

	<!-- Primary stats: the four headline numbers. -->
	<dl
		class="grid grid-cols-2 gap-px overflow-hidden rounded-xl border border-zinc-800 bg-zinc-800 sm:grid-cols-4"
	>
		{#each [{ label: m.detail_stat_distance(), value: formatDistance(a.summary.distanceM ?? null) }, { label: m.detail_stat_moving(), value: formatDuration(a.movingDurationS) }, { label: primaryPace.label, value: primaryPace.value }, { label: m.detail_stat_elevation(), value: formatElevation(a.summary.elevationGainM ?? null) }] as c (c.label)}
			<div class="bg-zinc-900/60 p-4">
				<dt class="text-xs uppercase tracking-wide text-zinc-500">{c.label}</dt>
				<dd class="mt-1.5 text-2xl font-semibold tabular-nums">{c.value}</dd>
			</div>
		{/each}
	</dl>

	<!-- Achievements -->
	{#if beCount > 0 || segCount > 0}
		<div class="flex flex-wrap gap-2 text-xs">
			{#if prCount > 0}
				<span class="rounded-full bg-emerald-500/15 px-3 py-1 text-emerald-300">
					🏆 {prCount} segment PR{prCount === 1 ? '' : 's'}
				</span>
			{/if}
			{#if segCount > 0}
				<span class="rounded-full bg-zinc-800 px-3 py-1 text-zinc-300">
					{segCount} segment effort{segCount === 1 ? '' : 's'}
				</span>
			{/if}
			{#if beCount > 0}
				<span class="rounded-full bg-zinc-800 px-3 py-1 text-zinc-300">
					{beCount} best effort{beCount === 1 ? '' : 's'}
				</span>
			{/if}
		</div>
	{/if}

	<!-- Secondary stats: only those that carry a value. -->
	{#if secondaryStats.length > 0}
		<dl
			class="grid grid-cols-2 gap-x-6 gap-y-3 rounded-xl border border-zinc-800 bg-zinc-900/40 p-5 sm:grid-cols-3 lg:grid-cols-4"
		>
			{#each secondaryStats as s (s.label)}
				<div class="flex items-baseline justify-between gap-2 border-b border-zinc-800/60 pb-1.5">
					<dt class="text-xs text-zinc-500">{s.label}</dt>
					<dd class="text-sm font-medium tabular-nums text-zinc-200">{s.value}</dd>
				</div>
			{/each}
		</dl>
	{/if}

	{#if coordinates.length > 0}
		<section>
			<div class="mb-3 flex items-center justify-between">
				<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">
					{m.detail_section_route()}
				</h2>
				<a href={`/activities/${a.id}/map`} class="text-xs text-accent-400 hover:text-accent-300">
					Full screen ↗
				</a>
			</div>
			<ActivityMap {coordinates} {track} {hoverT} onHover={(t) => (hoverT = t)} />
		</section>
	{/if}

	<!-- Photos / attachments -->
	<section>
		<div class="mb-2 flex items-center justify-between">
			<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-500">Photos</h2>
			<button
				type="button"
				disabled={uploadingPhoto}
				onclick={() => photoInput?.click()}
				class="rounded border border-zinc-700 px-2.5 py-1 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				{uploadingPhoto ? 'Uploading…' : '+ Add photos'}
			</button>
			<input
				bind:this={photoInput}
				type="file"
				accept="image/*"
				multiple
				class="hidden"
				onchange={(e) => uploadPhotos(e.currentTarget.files)}
			/>
		</div>
		{#if attachments.length > 0}
			<div class="grid grid-cols-2 gap-2 sm:grid-cols-3 md:grid-cols-4">
				{#each attachments as att (att.id)}
					<div class="group relative overflow-hidden rounded-lg border border-zinc-800 bg-zinc-900">
						<img
							src={att.url}
							alt={att.caption || 'Activity photo'}
							loading="lazy"
							class="h-32 w-full object-cover"
						/>
						<button
							type="button"
							onclick={() => deleteAttachment(att.id)}
							title="Remove photo"
							aria-label="Remove photo"
							class="absolute right-1 top-1 hidden rounded bg-black/60 px-1.5 py-0.5 text-xs text-white hover:bg-red-600 group-hover:block"
						>
							✕
						</button>
					</div>
				{/each}
			</div>
		{:else}
			<p class="text-sm text-zinc-600">No photos yet.</p>
		{/if}
	</section>

	{#if series.length > 0}
		<section>
			<div class="mb-3 flex items-center justify-between">
				<h2 class="text-sm font-medium uppercase tracking-wide text-zinc-400">
					{m.detail_section_streams()}
				</h2>
				<a
					href={`/activities/${a.id}/streams`}
					class="text-xs text-accent-400 hover:text-accent-300"
				>
					Full screen ↗
				</a>
			</div>
			<StreamChart {offsets} {series} {hoverT} onHover={(t) => (hoverT = t)} />
		</section>
	{/if}

	<!-- Explain a missing stream instead of silently dropping the sections. -->
	{#if series.length === 0 && coordinates.length === 0}
		{#if streamStatus === 'error'}
			<section
				class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-amber-800/50 bg-amber-950/20 p-4 text-sm text-amber-200"
			>
				<span>Couldn't load this activity's streams.</span>
				<button
					type="button"
					onclick={() => refreshAll()}
					class="rounded border border-amber-700/50 px-3 py-1 text-xs hover:bg-amber-900/30"
				>
					Retry
				</button>
			</section>
		{:else if streamStatus === 'empty' || streamStatus === 'absent'}
			<section
				class="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-dashed border-zinc-700 bg-zinc-900/30 p-4 text-sm text-zinc-400"
			>
				<span>
					No stream data for this activity{streamStatus === 'empty' ? ' yet' : ''} — no GPS track, elevation
					or sensor charts to show.
				</span>
				<a
					href={`/activities/${a.id}/manage`}
					class="rounded border border-zinc-700 px-3 py-1 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
				>
					Re-fetch from provider
				</a>
			</section>
		{/if}
	{/if}

	{#if bestEfforts.length > 0}
		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">Best Efforts</h2>
			<div class="overflow-hidden rounded-lg border border-zinc-800">
				<table class="w-full text-sm">
					<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
						<tr>
							<th class="px-4 py-2 text-left">Metric</th>
							<th class="px-4 py-2 text-left">Fenster</th>
							<th class="px-4 py-2 text-right">Wert</th>
						</tr>
					</thead>
					<tbody class="divide-y divide-zinc-800">
						{#each bestEfforts as b (b.id)}
							<tr
								class="cursor-pointer bg-zinc-900/30 transition-colors hover:bg-zinc-800/60"
								onclick={() =>
									goto(`/best-efforts/${a.type}/${b.metric}/${b.windowKind}/${b.windowValue}`)}
							>
								<td class="px-4 py-2 font-mono text-xs uppercase text-accent-400">{b.metric}</td>
								<td class="px-4 py-2 text-zinc-300">{formatEffortWindow(b)}</td>
								<td class="px-4 py-2 text-right tabular-nums">{formatEffortValue(b)} ↗</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</section>
	{/if}

	{#if laps.length > 1}
		<section>
			<h2 class="mb-3 text-sm font-medium text-zinc-300">
				Laps <span class="text-zinc-600">({laps.length})</span>
			</h2>
			<div class="overflow-x-auto rounded-xl border border-zinc-800">
				<table class="w-full text-sm">
					<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
						<tr>
							<th class="px-4 py-2 text-left font-medium">Lap</th>
							<th class="px-4 py-2 text-right font-medium">Distance</th>
							<th class="px-4 py-2 text-right font-medium">Time</th>
							<th class="px-4 py-2 text-right font-medium">Pace</th>
							<th class="px-4 py-2 text-right font-medium">HR</th>
							{#if anyLapPower}<th class="px-4 py-2 text-right font-medium">Power</th>{/if}
							<th class="px-4 py-2 text-right font-medium">Elev</th>
						</tr>
					</thead>
					<tbody>
						{#each laps as l, i (i)}
							<tr class="border-t border-zinc-800/70">
								<td class="px-4 py-2 font-medium text-zinc-200">{l.label || `Lap ${l.index}`}</td>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-300"
									>{formatDistance(l.distance_m)}</td
								>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-300"
									>{formatDuration(l.moving_s || l.elapsed_s)}</td
								>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-300"
									>{formatPace(l.avg_speed_mps)}</td
								>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-300"
									>{formatHeartRate(l.avg_heart_rate)}</td
								>
								{#if anyLapPower}<td class="px-4 py-2 text-right tabular-nums text-zinc-300"
										>{formatPower(l.avg_power)}</td
									>{/if}
								<td class="px-4 py-2 text-right tabular-nums text-zinc-300"
									>{formatElevation(l.elevation_gain_m)}</td
								>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</section>
	{/if}

	{#if zones?.decoupling}
		{@const dc = zones.decoupling}
		<section>
			<h2 class="mb-3 text-sm font-medium text-zinc-300">Aerobic decoupling</h2>
			<div
				class="flex flex-wrap items-center gap-4 rounded-xl border border-zinc-800 bg-zinc-900/40 p-4"
			>
				<div
					class="text-3xl font-semibold tabular-nums {dc.coupled
						? 'text-emerald-400'
						: 'text-amber-400'}"
				>
					{dc.pct > 0 ? '+' : ''}{dc.pct.toFixed(1)}%
				</div>
				<div class="text-xs text-zinc-400">
					<div class="text-zinc-300">{dc.coupled ? 'Well coupled' : 'Notable drift'}</div>
					<div>
						{dc.basis === 'power' ? 'Power' : 'Pace'}:HR drift, 1st→2nd half. Lower is more
						aerobically efficient; under 5% is strong.
					</div>
				</div>
			</div>
		</section>
	{/if}

	{#if hasZones}
		<section>
			<h2 class="mb-3 text-sm font-medium text-zinc-300">Training zones</h2>
			<div class="grid gap-4 lg:grid-cols-2">
				{#each [{ key: 'hr', title: 'Heart rate', set: zones?.hr, unit: 'bpm' }, { key: 'power', title: 'Power', set: zones?.power, unit: 'W' }] as col (col.key)}
					{#if col.set && col.set.total_seconds > 0}
						{@const zset = col.set}
						<div class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-4">
							<div class="mb-2 flex items-baseline justify-between">
								<h3 class="text-sm font-medium text-zinc-200">{col.title}</h3>
								<span class="text-xs text-zinc-500">
									{zset.basis === 'lthr'
										? `vs LTHR ${zset.reference} bpm`
										: zset.basis === 'max'
											? `vs max HR ${zset.reference} bpm`
											: `vs FTP ${zset.reference} W`}
								</span>
							</div>
							<!-- stacked proportional bar -->
							<div class="mb-3 flex h-3 overflow-hidden rounded-full bg-zinc-800">
								{#each zset.zones as z, i (i)}
									{#if z.seconds > 0}
										<div
											style:width={`${zonePct(zset, z.seconds)}%`}
											style:background={ZONE_COLORS[i]}
											title={`${z.label}: ${formatDuration(z.seconds)}`}
										></div>
									{/if}
								{/each}
							</div>
							<ul class="space-y-1">
								{#each zset.zones as z, i (i)}
									<li class="flex items-center gap-2 text-xs">
										<span class="h-2.5 w-2.5 shrink-0 rounded-sm" style:background={ZONE_COLORS[i]}
										></span>
										<span class="w-28 shrink-0 text-zinc-300 max-md:w-20 max-md:truncate"
											>{z.label}</span
										>
										<span class="w-24 shrink-0 tabular-nums text-zinc-500 max-md:hidden">
											{Math.round(z.low_pct * zset.reference)}{z.high_pct > 0
												? `–${Math.round(z.high_pct * zset.reference)}`
												: '+'}
											{col.unit}
										</span>
										<div class="h-1.5 flex-1 rounded-full bg-zinc-800">
											<div
												class="h-1.5 rounded-full"
												style:width={`${zonePct(zset, z.seconds)}%`}
												style:background={ZONE_COLORS[i]}
											></div>
										</div>
										<span class="w-16 shrink-0 text-right tabular-nums text-zinc-300"
											>{formatDuration(z.seconds)}</span
										>
										<span class="w-10 shrink-0 text-right tabular-nums text-zinc-500"
											>{Math.round(zonePct(zset, z.seconds))}%</span
										>
									</li>
								{/each}
							</ul>
						</div>
					{/if}
				{/each}
			</div>
		</section>
	{/if}

	{#if segmentEfforts.length > 0}
		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">
				Segments <span class="text-zinc-600">({segmentEfforts.length})</span>
			</h2>
			<div class="max-h-[28rem] overflow-y-auto rounded-lg border border-zinc-800">
				{#if hasElevation}
					<!-- Elevation profile pinned to the top of the scroll area; hovering a
					     row shades the segment's span on it. -->
					<div
						class="sticky top-0 z-10 border-b border-zinc-800 bg-zinc-950/95 px-2 pt-2 backdrop-blur"
					>
						<ElevationProfile {track} highlight={segHighlight} />
					</div>
				{/if}
				<table class="w-full text-sm">
					<thead class="bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
						<tr>
							<th class="px-4 py-2 text-left">Segment</th>
							<th class="hidden px-4 py-2 text-right sm:table-cell">{m.col_distance()}</th>
							<th class="px-4 py-2 text-right">{m.col_duration()}</th>
							<th class="hidden px-4 py-2 text-right md:table-cell">{m.col_avg_hr()}</th>
						</tr>
					</thead>
					<tbody class="divide-y divide-zinc-800">
						{#each segmentEfforts as s, i (i)}
							<tr
								class="bg-zinc-900/30 transition-colors hover:bg-zinc-800/50"
								onmouseenter={() => (hoveredSeg = s)}
								onmouseleave={() => (hoveredSeg = null)}
							>
								<td class="px-4 py-2">
									<a
										href={`/segment/${s.segment_id}`}
										class="text-accent-400 hover:text-accent-300 hover:underline"
									>
										{s.segment_name}
									</a>
									{#if s.is_personal_record}
										<span
											class="ml-2 rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] font-bold uppercase text-amber-300"
											title="Personal record">PR</span
										>
									{/if}
									{#if s.climb_category}
										<span
											class="ml-1 rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] uppercase text-zinc-400"
										>
											{s.climb_category.replace('_', ' ')}
										</span>
									{/if}
								</td>
								<td class="hidden px-4 py-2 text-right tabular-nums text-zinc-300 sm:table-cell">
									{formatDistance(s.segment_distance_m)}
								</td>
								<td class="px-4 py-2 text-right tabular-nums text-zinc-100">
									{formatDuration(s.elapsed_s)}
								</td>
								<td class="hidden px-4 py-2 text-right tabular-nums text-zinc-300 md:table-cell">
									{s.avg_heart_rate ? formatHeartRate(s.avg_heart_rate) : m.placeholder_dash()}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		</section>
	{/if}

	{#if a.description}
		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">
				{m.detail_section_description()}
			</h2>
			<p class="whitespace-pre-line text-sm text-zinc-300">{a.description}</p>
		</section>
	{/if}

	{#if a.sources && a.sources.length > 0}
		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">Sources</h2>
			<p class="mb-3 text-xs text-zinc-500">
				This activity is merged from {activeSources.length} source{activeSources.length === 1
					? ''
					: 's'}. Detach a source if it was matched here by mistake — the activity re-merges from
				the rest, or is deleted if it was the only one.
			</p>
			{#if sourceError}
				<div
					class="mb-2 rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300"
				>
					{sourceError}
				</div>
			{/if}
			<ul class="divide-y divide-zinc-800 rounded-lg border border-zinc-800">
				{#each a.sources as s (s.id)}
					<li class="flex items-center justify-between gap-4 px-4 py-3 text-sm max-md:flex-wrap">
						<div class="min-w-0">
							<span class="font-medium text-zinc-200">{s.provider}</span>
							{#if s.isPrimary}
								<span
									class="ml-2 rounded bg-accent-500/15 px-1.5 py-0.5 text-xs text-accent-300"
									title="Maps and charts render from this source">primary</span
								>
							{/if}
							{#if s.status === 'detached'}
								<span class="ml-2 rounded bg-zinc-700/50 px-1.5 py-0.5 text-xs text-zinc-400"
									>detached</span
								>
							{:else if s.status !== 'active'}
								<span class="ml-2 rounded bg-amber-500/15 px-1.5 py-0.5 text-xs text-amber-300"
									>{humanizeStatus(s.status)}</span
								>
							{/if}
							{#if s.reimportStatus === 'update_available'}
								<span class="ml-2 rounded bg-sky-500/15 px-1.5 py-0.5 text-xs text-sky-300"
									>update available</span
								>
							{/if}
							<div class="mt-0.5 truncate text-xs text-zinc-600">
								{s.externalId ? `#${s.externalId} · ` : ''}imported {formatDate(
									s.importedAt
								)}{s.statusReason ? ` · ${humanizeStatus(s.statusReason)}` : ''}
							</div>
						</div>
						<div class="flex shrink-0 items-center gap-2">
							{#if s.hasRawBlob}
								<a
									href={`/api/activities/${a.id}/sources/${s.id}/download`}
									class="rounded border border-zinc-700 px-2 py-1 text-xs text-zinc-400 hover:border-accent-500 hover:text-accent-300"
									title="Download the original imported file"
								>
									Download
								</a>
							{/if}
							{#if s.status !== 'detached'}
								<button
									type="button"
									disabled={detaching === s.id}
									onclick={() => detachSource(s)}
									class="rounded border border-zinc-700 px-2 py-1 text-xs text-zinc-400 hover:border-red-600 hover:text-red-300 disabled:opacity-50"
								>
									{detaching === s.id ? 'Detaching…' : 'Detach'}
								</button>
							{/if}
						</div>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	<KudosComments activityId={a.id} />
</section>
