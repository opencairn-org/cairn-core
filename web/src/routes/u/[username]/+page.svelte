<script lang="ts">
	import { onMount } from 'svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import { formatDistance, formatDuration, formatRelativeDate } from '#lib/format.js';

	let { data } = $props();

	type Card = { id: string; username: string; display_name: string; avatar_url: string };
	type Activity = {
		id: string;
		title: string;
		type: string;
		discipline: string;
		start_time: string;
		distance_m: number | null;
		elevation_gain_m: number | null;
		elapsed_duration_s: number;
		start_place?: string;
		start_location_redacted?: boolean;
	};
	type Profile = {
		user: Card;
		is_public: boolean;
		is_self: boolean;
		is_following: boolean;
		followers_count: number;
		following_count: number;
		activities: Activity[];
	};

	let profile = $state<Profile | null>(null);
	let error = $state<string | null>(null);
	let busy = $state(false);

	async function load() {
		error = null;
		try {
			const res = await fetch(`/api/profiles/${data.username}`);
			if (res.status === 403) {
				error = 'This profile is private.';
				return;
			}
			if (res.status === 404) {
				error = 'Athlete not found.';
				return;
			}
			if (!res.ok) throw new Error((await res.text()).trim());
			profile = await res.json();
		} catch (e) {
			error = (e as Error).message;
		}
	}

	async function toggleFollow() {
		if (!profile) return;
		busy = true;
		try {
			const method = profile.is_following ? 'DELETE' : 'POST';
			const res = await fetch(`/api/users/${profile.user.id}/follow`, {
				method,
				headers: { 'Content-Type': 'application/json' }
			});
			if (res.ok) {
				profile.is_following = !profile.is_following;
				profile.followers_count += profile.is_following ? 1 : -1;
			}
		} finally {
			busy = false;
		}
	}

	let blocked = $state(false);
	async function blockUser() {
		if (!profile) return;
		if (
			!confirm(
				`Block ${profile.user.display_name || profile.user.username}? They won't be able to see your activities or interact with you.`
			)
		)
			return;
		const res = await fetch(`/api/users/${profile.user.id}/block`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' }
		});
		if (res.ok) {
			blocked = true;
			profile.is_following = false;
		}
	}
	async function reportUser() {
		if (!profile) return;
		const reason = prompt('Why are you reporting this athlete?') ?? '';
		if (!reason.trim()) return;
		await fetch('/api/reports', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ target_kind: 'user', target_id: profile.user.id, reason })
		});
		alert('Report submitted. Thanks — an admin will review it.');
	}

	onMount(load);
</script>

<svelte:head><title>{data.username} · Cairn</title></svelte:head>

<div class="mx-auto max-w-2xl px-4 py-6">
	{#if error}
		<div class="rounded-lg border border-dashed p-8 text-center text-sm text-zinc-400">{error}</div>
	{:else if profile}
		<header class="mb-6 flex items-center gap-4 max-md:flex-wrap">
			<div
				class="flex h-16 w-16 items-center justify-center rounded-full bg-zinc-800 text-xl font-semibold text-zinc-400"
			>
				{(profile.user.display_name || profile.user.username || '?').slice(0, 1).toUpperCase()}
			</div>
			<div class="flex-1">
				<h1 class="text-xl font-semibold">{profile.user.display_name || profile.user.username}</h1>
				<div class="text-sm text-zinc-400">@{profile.user.username}</div>
				<div class="mt-1 flex gap-4 text-sm text-zinc-400">
					<span>
						<span class="font-semibold text-zinc-100">{profile.followers_count}</span> followers
					</span>
					<span>
						<span class="font-semibold text-zinc-100">{profile.following_count}</span> following
					</span>
				</div>
			</div>
			{#if data.user && !profile.is_self}
				{#if blocked}
					<span class="rounded-full border px-4 py-2 text-sm text-zinc-400">Blocked</span>
				{:else}
					<div class="flex items-center gap-2">
						<button
							onclick={toggleFollow}
							disabled={busy}
							class={`rounded-full px-4 py-2 text-sm font-medium ${profile.is_following ? 'border bg-zinc-900/40 border border-zinc-800 text-zinc-300 hover:bg-zinc-800' : 'bg-accent-500 text-zinc-950 hover:bg-accent-400'} disabled:opacity-50`}
						>
							{profile.is_following ? 'Following' : 'Follow'}
						</button>
						<button
							onclick={reportUser}
							title="Report"
							class="text-zinc-500 hover:text-zinc-300 max-md:p-2"
						>
							⚑
						</button>
						<button
							onclick={blockUser}
							title="Block"
							class="text-zinc-500 hover:text-red-500 max-md:p-2"
						>
							⊘
						</button>
					</div>
				{/if}
			{:else if !data.user}
				<a
					href="/login"
					class="rounded-full bg-accent-500 px-4 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400"
				>
					Sign in to follow
				</a>
			{/if}
		</header>

		<h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-zinc-400">
			Recent activities
		</h2>
		{#if profile.activities.length === 0}
			<p class="text-sm text-zinc-400">No visible activities.</p>
		{/if}
		<div class="space-y-3">
			{#each profile.activities as a (a.id)}
				<a
					href={`/a/${a.id}`}
					class="block rounded-lg border bg-zinc-900/40 border border-zinc-800 p-4 shadow-sm hover:opacity-90"
				>
					<div class="flex items-center gap-3">
						<SportIcon type={a.type} size={24} />
						<div class="min-w-0 flex-1">
							<div class="truncate font-medium">{a.title || 'Untitled'}</div>
							<div class="text-xs text-zinc-400">
								{formatRelativeDate(a.start_time)}{a.start_place
									? ` · ${a.start_place}`
									: a.start_location_redacted
										? ' · 📍 hidden'
										: ''}
							</div>
						</div>
					</div>
					<div class="mt-2 flex gap-6 text-sm">
						<span class="font-semibold">{formatDistance(a.distance_m ?? 0)}</span>
						<span class="font-semibold">{formatDuration(a.elapsed_duration_s)}</span>
						{#if a.elevation_gain_m}<span class="font-semibold">
								{Math.round(a.elevation_gain_m)} m
							</span>{/if}
					</div>
				</a>
			{/each}
		</div>
	{:else}
		<p class="text-sm text-zinc-400">Loading…</p>
	{/if}
</div>
