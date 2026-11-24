<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import KudosComments from '#lib/components/KudosComments.svelte';
	import { formatDistance, formatDuration, formatRelativeDate } from '#lib/format.js';

	let { data } = $props();

	type Card = { id: string; username: string; display_name: string; avatar_url: string };
	type Activity = {
		id: string;
		title: string;
		type: string;
		start_time: string;
		distance_m: number | null;
		elevation_gain_m: number | null;
		elapsed_duration_s: number;
		moving_duration_s?: number;
		description?: string;
		start_place?: string;
		start_location_redacted?: boolean;
		avg_hr?: number | null;
		avg_power?: number | null;
		avg_speed_mps?: number | null;
		visible_categories?: string[];
		photos_url?: string;
	};

	let activity = $state<Activity | null>(null);
	let owner = $state<Card | null>(null);
	let error = $state<string | null>(null);
	let photos = $state<{ id: string; url: string; caption: string }[]>([]);

	async function load() {
		try {
			const res = await fetch(`/api/activities/${data.id}/projected`);
			if (res.status === 403) {
				error = 'You do not have access to this activity.';
				return;
			}
			if (res.status === 404) {
				error = 'Activity not found.';
				return;
			}
			if (!res.ok) throw new Error((await res.text()).trim());
			const body = await res.json();
			// Owners get the full-fidelity page.
			if (body.is_owner) {
				goto(`/activities/${data.id}`, { replace: true });
				return;
			}
			activity = body.activity;
			owner = body.owner;
			if (activity?.photos_url) {
				try {
					const pr = await fetch(activity.photos_url);
					if (pr.ok) photos = (await pr.json()).attachments ?? [];
				} catch {
					/* ignore */
				}
			}
		} catch (e) {
			error = (e as Error).message;
		}
	}

	const can = (c: string) => activity?.visible_categories?.includes(c) ?? false;
	onMount(load);
</script>

<svelte:head><title>{activity?.title ?? 'Activity'} · Cairn</title></svelte:head>

<div class="mx-auto max-w-2xl px-4 py-6">
	{#if error}
		<div class="rounded-lg border border-dashed p-8 text-center text-sm text-zinc-400">{error}</div>
	{:else if activity}
		<header class="mb-4 flex items-center gap-3">
			<SportIcon type={activity.type} size={28} />
			<div>
				<h1 class="text-xl font-semibold">{activity.title || 'Untitled'}</h1>
				<div class="text-sm text-zinc-400">
					{#if owner}<a href={`/u/${owner.username}`} class="hover:underline">
							{owner.display_name || owner.username}
						</a> ·
					{/if}
					{formatRelativeDate(activity.start_time)}
					{#if activity.start_place && can('location')}
						· {activity.start_place}{:else if activity.start_location_redacted && can('location')}
						· <span title="The owner hid this start location">📍 hidden</span>{/if}
				</div>
			</div>
		</header>

		{#if activity.description && can('summary')}
			<p class="mb-4 whitespace-pre-line text-sm text-zinc-300">{activity.description}</p>
		{/if}

		<div
			class="grid grid-cols-3 gap-4 rounded-lg border bg-zinc-900/40 border border-zinc-800 p-4 shadow-sm max-md:grid-cols-2"
		>
			<div>
				<div class="text-xs text-zinc-400">Distance</div>
				<div class="text-lg font-semibold">{formatDistance(activity.distance_m ?? 0)}</div>
			</div>
			<div>
				<div class="text-xs text-zinc-400">Time</div>
				<div class="text-lg font-semibold">{formatDuration(activity.elapsed_duration_s)}</div>
			</div>
			{#if activity.elevation_gain_m}<div>
					<div class="text-xs text-zinc-400">Elevation</div>
					<div class="text-lg font-semibold">{Math.round(activity.elevation_gain_m)} m</div>
				</div>{/if}
			{#if can('hr') && activity.avg_hr}<div>
					<div class="text-xs text-zinc-400">Avg HR</div>
					<div class="text-lg font-semibold">{activity.avg_hr} bpm</div>
				</div>{/if}
			{#if can('power') && activity.avg_power}<div>
					<div class="text-xs text-zinc-400">Avg Power</div>
					<div class="text-lg font-semibold">{activity.avg_power} W</div>
				</div>{/if}
		</div>

		{#if photos.length > 0}
			<div class="mt-4 grid grid-cols-2 gap-2 sm:grid-cols-3">
				{#each photos as p (p.id)}
					<img
						src={p.url}
						alt={p.caption || 'Activity photo'}
						loading="lazy"
						class="h-32 w-full rounded-lg border border-zinc-800 object-cover"
					/>
				{/each}
			</div>
		{/if}

		<KudosComments activityId={activity.id} />
	{:else}
		<p class="text-sm text-zinc-400">Loading…</p>
	{/if}
</div>
