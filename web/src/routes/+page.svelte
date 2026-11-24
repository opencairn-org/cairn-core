<script lang="ts">
	import { onMount } from 'svelte';
	import { m } from '#lib/paraglide/messages.js';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import {
		formatDistance,
		formatDuration,
		formatElevation,
		formatRelativeDate
	} from '#lib/format.js';

	let { data } = $props();

	type Facet = { value: string; count: number };
	type Recent = {
		id: string;
		title: string;
		type: string;
		discipline: string;
		start_time: string;
		distance_m: number | null;
		elapsed_duration_s: number;
	};
	type WeekStat = { count: number; distance_m: number; moving_s: number; elevation_gain_m: number };
	type Overview = {
		totals: WeekStat;
		by_sport: Facet[];
		recent: Recent[];
		this_week?: WeekStat;
		last_week?: WeekStat;
	};

	let ov = $state<Overview | null>(null);

	// Deep-link a week's stat block into the activities view filtered to that
	// week. UTC, Monday-start, half-open [from, to) — mirrors the server's
	// this_week/last_week buckets in overview.go.
	function weekHref(weeksAgo: number): string {
		const now = new Date();
		const today = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
		const dow = (today.getUTCDay() + 6) % 7; // Mon=0 … Sun=6
		// eslint-disable-next-line svelte/prefer-svelte-reactivity -- local scratch date
		const monday = new Date(today);
		monday.setUTCDate(today.getUTCDate() - dow - weeksAgo * 7);
		// eslint-disable-next-line svelte/prefer-svelte-reactivity -- local scratch date
		const next = new Date(monday);
		next.setUTCDate(monday.getUTCDate() + 7);
		const iso = (d: Date) => d.toISOString().slice(0, 10);
		return `/activities?from=${iso(monday)}&to=${iso(next)}`;
	}

	type FeedItem = {
		source: 'self' | 'following' | 'federated';
		id: string;
		title: string;
		type: string;
		start_time: string;
		distance_m?: number | null;
		elevation_gain_m?: number | null;
		elapsed_duration_s?: number;
		start_place?: string;
		start_location_redacted?: boolean;
		map_image_url?: string;
		url?: string;
		owner?: { display_name?: string; username?: string; remote?: boolean; actor?: string };
	};
	let feed = $state<FeedItem[]>([]);

	onMount(async () => {
		if (!data.user) return;
		try {
			const res = await fetch('/api/overview');
			if (res.ok) ov = await res.json();
		} catch {
			/* ignore */
		}
		try {
			const fr = await fetch('/api/feed/home');
			if (fr.ok) feed = (await fr.json()).activities ?? [];
		} catch {
			/* ignore */
		}
	});

	// Quick filter: which feed sources to show (own / friends / federated).
	// The federated chip only exists when the instance federates at all.
	let sources = $state({ self: true, following: true, federated: true });
	const sourceMeta = $derived(
		[
			{ key: 'self' as const, label: 'Mine' },
			{ key: 'following' as const, label: 'Friends' },
			{ key: 'federated' as const, label: 'Federated' }
		].filter((s) => s.key !== 'federated' || data.features?.federation)
	);
	const shown = $derived(feed.filter((a) => sources[a.source]));

	const summary = $derived(
		ov
			? [
					{ label: 'Activities', value: ov.totals.count.toLocaleString() },
					{ label: 'Distance', value: formatDistance(ov.totals.distance_m) },
					{ label: 'Moving time', value: formatDuration(ov.totals.moving_s) },
					{ label: 'Elevation', value: formatElevation(ov.totals.elevation_gain_m) }
				]
			: []
	);
</script>

{#if !data.user}
	<div class="space-y-6">
		<header>
			<h1 class="text-3xl font-semibold tracking-tight max-md:text-2xl">{m.overview_title()}</h1>
			<p class="mt-1 text-sm text-zinc-400">{m.overview_intro()}</p>
		</header>
		<a
			href="/login"
			class="inline-block rounded border border-accent-500 bg-accent-500/20 px-4 py-2 text-sm text-accent-300 hover:bg-accent-500/30"
		>
			{m.menu_signin()}
		</a>
	</div>
{:else}
	<div class="space-y-6">
		<!-- Top header: welcome + summary strip -->
		<header class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-5">
			<div class="flex flex-wrap items-end justify-between gap-4">
				<div>
					<h1 class="text-2xl font-semibold tracking-tight">
						Welcome back, {data.user.displayName}
					</h1>
					<p class="mt-1 text-sm text-zinc-400">Your training at a glance.</p>
				</div>
				{#if summary.length}
					<dl class="flex flex-wrap gap-x-8 gap-y-2">
						{#each summary as s (s.label)}
							<div>
								<dt class="text-xs uppercase tracking-wide text-zinc-500">{s.label}</dt>
								<dd class="text-lg font-semibold tabular-nums text-zinc-100">{s.value}</dd>
							</div>
						{/each}
					</dl>
				{/if}
			</div>
		</header>

		{#snippet weekBlock(label: string, w: WeekStat | undefined, href: string)}
			<section class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-4">
				<a
					{href}
					class="group mb-3 flex items-center justify-between text-xs font-medium uppercase tracking-wide text-zinc-500 hover:text-zinc-300"
				>
					<h2>{label}</h2>
					<span
						class="text-zinc-600 transition group-hover:translate-x-0.5 group-hover:text-zinc-400"
					>
						→
					</span>
				</a>
				{#if w && w.count > 0}
					<dl class="grid grid-cols-2 gap-3">
						<div>
							<dt class="text-xs text-zinc-500">Activities</dt>
							<dd class="text-lg font-semibold tabular-nums text-zinc-100">{w.count}</dd>
						</div>
						<div>
							<dt class="text-xs text-zinc-500">Distance</dt>
							<dd class="text-lg font-semibold tabular-nums text-zinc-100">
								{formatDistance(w.distance_m)}
							</dd>
						</div>
						<div>
							<dt class="text-xs text-zinc-500">Moving time</dt>
							<dd class="text-lg font-semibold tabular-nums text-zinc-100">
								{formatDuration(w.moving_s)}
							</dd>
						</div>
						<div>
							<dt class="text-xs text-zinc-500">Elevation</dt>
							<dd class="text-lg font-semibold tabular-nums text-zinc-100">
								{formatElevation(w.elevation_gain_m)}
							</dd>
						</div>
					</dl>
				{:else}
					<p class="text-sm text-zinc-500">No activities.</p>
				{/if}
			</section>
		{/snippet}

		<div class="grid gap-6 lg:grid-cols-[20rem_1fr]">
			<!-- Left sidebar: weekly stats + stuff -->
			<aside class="space-y-4">
				{@render weekBlock('This week', ov?.this_week, weekHref(0))}
				{@render weekBlock('Last week', ov?.last_week, weekHref(1))}

				<nav class="rounded-xl border border-zinc-800 bg-zinc-900/40 p-4 text-sm">
					<h2 class="mb-2 text-xs font-medium uppercase tracking-wide text-zinc-500">Explore</h2>
					<div class="space-y-0.5">
						<a
							href="/analysis"
							class="block rounded px-2 py-1.5 text-zinc-300 hover:bg-zinc-800/60"
						>
							Training load
						</a>
						<a href="/stats" class="block rounded px-2 py-1.5 text-zinc-300 hover:bg-zinc-800/60">
							Stats
						</a>
						<a
							href="/segments"
							class="block rounded px-2 py-1.5 text-zinc-300 hover:bg-zinc-800/60"
						>
							Segments
						</a>
						<a href="/athlete" class="block rounded px-2 py-1.5 text-zinc-300 hover:bg-zinc-800/60">
							Find athletes
						</a>
					</div>
				</nav>
			</aside>

			<!-- Main content: the feed (own + friends + federated, quick-filterable) -->
			<main>
				<div class="mb-3 flex flex-wrap items-center justify-between gap-2">
					<h2 class="text-sm font-medium text-zinc-300">Your feed</h2>
					{#if feed.length > 0}
						<div class="flex flex-wrap gap-1.5">
							{#each sourceMeta as s (s.key)}
								<button
									type="button"
									onclick={() => (sources[s.key] = !sources[s.key])}
									class="rounded-full border px-2.5 py-1 text-xs {sources[s.key]
										? 'border-accent-500/40 bg-accent-500/20 text-accent-300'
										: 'border-zinc-700 text-zinc-500 hover:text-zinc-300'}"
								>
									{s.label}<span class="ml-1 opacity-60">
										{feed.filter((a) => a.source === s.key).length}
									</span>
								</button>
							{/each}
						</div>
					{/if}
				</div>
				{#if shown.length > 0}
					<div class="space-y-3">
						{#each shown as a (a.source + ':' + a.id)}
							<article class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4 shadow-sm">
								<div class="mb-2 flex items-center gap-2 text-sm text-zinc-400">
									<span class="font-medium text-zinc-100">
										{a.owner?.display_name || a.owner?.username}
									</span>
									{#if a.source === 'self'}
										<span
											class="rounded bg-zinc-700/50 px-1.5 py-0.5 text-[10px] uppercase text-zinc-300"
										>
											you
										</span>
									{:else if a.source === 'federated'}
										<span
											class="rounded bg-violet-500/20 px-1.5 py-0.5 text-[10px] uppercase text-violet-300"
										>
											federated
										</span>
									{/if}
									<span>·</span>
									<span>{formatRelativeDate(a.start_time)}</span>
								</div>
								{#if a.source === 'federated'}
									<a
										href={a.url || '#'}
										target="_blank"
										rel="noopener"
										class="flex items-center gap-3 hover:opacity-90"
									>
										<SportIcon type={a.type} size={24} />
										<div class="min-w-0 flex-1">
											<div class="truncate font-medium">{a.title || 'Activity'}</div>
											<div class="truncate text-xs text-zinc-500">
												{a.owner?.actor ? new URL(a.owner.actor).host : ''} ↗
											</div>
										</div>
									</a>
								{:else}
									<a
										href={a.source === 'self' ? `/activities/${a.id}` : `/a/${a.id}`}
										class="flex items-center gap-3 hover:opacity-90"
									>
										<SportIcon type={a.type} size={24} />
										<div class="min-w-0 flex-1">
											<div class="truncate font-medium">{a.title || 'Untitled'}</div>
											{#if a.start_place}<div class="truncate text-xs text-zinc-400">
													{a.start_place}
												</div>{:else if a.start_location_redacted}<div
													class="truncate text-xs text-zinc-400"
												>
													📍 Start location hidden
												</div>{/if}
										</div>
									</a>
								{/if}
								{#if a.distance_m || a.elapsed_duration_s || a.elevation_gain_m}
									<div
										class="mt-3 flex divide-x divide-zinc-800 rounded-md border border-zinc-800 text-sm"
									>
										{#if a.distance_m}
											<div class="flex-1 px-3 py-1.5">
												<div class="text-[10px] uppercase tracking-wide text-zinc-500">
													Distance
												</div>
												<div class="font-semibold tabular-nums">{formatDistance(a.distance_m)}</div>
											</div>
										{/if}
										{#if a.elapsed_duration_s}
											<div class="flex-1 px-3 py-1.5">
												<div class="text-[10px] uppercase tracking-wide text-zinc-500">Time</div>
												<div class="font-semibold tabular-nums">
													{formatDuration(a.elapsed_duration_s)}
												</div>
											</div>
										{/if}
										{#if a.elevation_gain_m}
											<div class="flex-1 px-3 py-1.5">
												<div class="text-[10px] uppercase tracking-wide text-zinc-500">
													Elevation
												</div>
												<div class="font-semibold tabular-nums">
													{Math.round(a.elevation_gain_m)} m
												</div>
											</div>
										{/if}
									</div>
								{/if}
								{#if a.map_image_url}
									<a
										href={a.source === 'federated'
											? a.url || '#'
											: a.source === 'self'
												? `/activities/${a.id}`
												: `/a/${a.id}`}
										target={a.source === 'federated' ? '_blank' : undefined}
										rel="noopener"
										class="mt-3 block"
									>
										<img
											src={a.map_image_url}
											alt="Course map"
											loading="lazy"
											class="aspect-[3/1] w-full rounded-md border border-zinc-800 object-cover"
											onerror={(e) =>
												((e.currentTarget as HTMLImageElement).style.display = 'none')}
										/>
									</a>
								{/if}
							</article>
						{/each}
					</div>
				{:else if feed.length > 0}
					<div
						class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-8 text-center text-sm text-zinc-400"
					>
						No activities match the selected filters.
					</div>
				{:else}
					<div
						class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-8 text-center text-sm text-zinc-400"
					>
						Nothing in your feed yet. Follow other athletes — or a remote handle over federation —
						to see their activities here.
						<div class="mt-2">
							<a href="/athlete" class="text-accent-400 hover:underline">Find athletes →</a>
						</div>
					</div>
				{/if}
			</main>
		</div>
	</div>
{/if}
