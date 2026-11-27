<script lang="ts">
	import { onMount } from 'svelte';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import { formatDistance, formatDuration, formatRelativeDate } from '#lib/format.js';

	let { data } = $props();

	type Card = {
		id: string;
		username: string;
		display_name: string;
		avatar_url: string;
		role?: string;
	};
	type Club = {
		id: string;
		slug: string;
		name: string;
		description?: string;
		is_public: boolean;
		member_count: number;
		is_member: boolean;
		my_role: string;
	};
	type Activity = {
		id: string;
		title: string;
		type: string;
		start_time: string;
		distance_m: number | null;
		elevation_gain_m: number | null;
		elapsed_duration_s: number;
		owner: Card;
	};

	let club = $state<Club | null>(null);
	let members = $state<Card[]>([]);
	let feed = $state<Activity[]>([]);
	let error = $state<string | null>(null);
	let busy = $state(false);

	async function loadClub() {
		const res = await fetch(`/api/clubs/${data.slug}`);
		if (res.status === 403) {
			error = 'This club is private.';
			return;
		}
		if (res.status === 404) {
			error = 'Club not found.';
			return;
		}
		if (!res.ok) {
			error = (await res.text()).trim();
			return;
		}
		club = await res.json();
		await Promise.all([loadMembers(), loadFeed()]);
	}
	async function loadMembers() {
		const res = await fetch(`/api/clubs/${data.slug}/members`);
		if (res.ok) members = (await res.json()).members ?? [];
	}
	async function loadFeed() {
		const res = await fetch(`/api/clubs/${data.slug}/feed`);
		if (res.ok) feed = (await res.json()).activities ?? [];
	}

	async function join() {
		busy = true;
		try {
			const res = await fetch(`/api/clubs/${data.slug}/join`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' }
			});
			if (res.ok && club) {
				club.is_member = true;
				club.member_count++;
				await Promise.all([loadMembers(), loadFeed()]);
			}
		} finally {
			busy = false;
		}
	}
	async function leave() {
		busy = true;
		try {
			const res = await fetch(`/api/clubs/${data.slug}/leave`, { method: 'DELETE' });
			if (res.ok && club) {
				club.is_member = false;
				club.member_count--;
				await loadMembers();
			}
		} finally {
			busy = false;
		}
	}

	onMount(loadClub);
</script>

<svelte:head><title>{club?.name ?? 'Club'} · Cairn</title></svelte:head>

<div class="mx-auto max-w-2xl px-4 py-6">
	{#if error}
		<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">{error}</div>
	{:else if club}
		<header class="mb-6 flex items-start justify-between max-md:flex-wrap max-md:gap-3">
			<div>
				<h1 class="text-2xl font-semibold">{club.name}</h1>
				<div class="text-sm text-gray-500">
					{club.member_count}
					{club.member_count === 1 ? 'member' : 'members'} · {club.is_public ? 'Public' : 'Private'}
				</div>
				{#if club.description}<p class="mt-2 max-w-prose text-sm text-gray-700">
						{club.description}
					</p>{/if}
			</div>
			{#if club.my_role === 'owner'}
				<span class="rounded-full border px-4 py-2 text-sm text-gray-500">Owner</span>
			{:else if club.is_member}
				<button
					onclick={leave}
					disabled={busy}
					class="rounded-full border bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:opacity-50"
				>
					Leave
				</button>
			{:else if club.is_public}
				<button
					onclick={join}
					disabled={busy}
					class="rounded-full bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-700 disabled:opacity-50"
				>
					Join
				</button>
			{/if}
		</header>

		<div class="grid gap-6 md:grid-cols-[1fr_200px]">
			<div>
				<h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-gray-500">Club feed</h2>
				{#if feed.length === 0}<p class="text-sm text-gray-500">No activities yet.</p>{/if}
				<div class="space-y-3">
					{#each feed as a (a.id)}
						<div class="rounded-lg border bg-white p-4 shadow-sm">
							<div class="mb-1 flex items-center gap-2 text-xs text-gray-500">
								<a
									href={`/u/${a.owner?.username}`}
									class="font-medium text-gray-700 hover:underline"
								>
									{a.owner?.display_name || a.owner?.username}
								</a>
								<span>· {formatRelativeDate(a.start_time)}</span>
							</div>
							<a href={`/a/${a.id}`} class="flex items-center gap-3 hover:opacity-90">
								<SportIcon type={a.type} size={22} />
								<div class="font-medium">{a.title || 'Untitled'}</div>
							</a>
							<div class="mt-2 flex gap-5 text-sm">
								<span class="font-semibold">{formatDistance(a.distance_m ?? 0)}</span>
								<span class="font-semibold">{formatDuration(a.elapsed_duration_s)}</span>
							</div>
						</div>
					{/each}
				</div>
			</div>

			<aside>
				<h2 class="mb-3 text-sm font-semibold uppercase tracking-wide text-gray-500">Members</h2>
				<ul class="space-y-1 text-sm">
					{#each members as m (m.id)}
						<li>
							<a href={`/u/${m.username}`} class="hover:underline">
								{m.display_name || m.username}
							</a>
							{#if m.role === 'owner'}<span class="ml-1 text-xs text-gray-400">(owner)</span>{/if}
						</li>
					{/each}
				</ul>
			</aside>
		</div>
	{:else}
		<p class="text-sm text-gray-500">Loading…</p>
	{/if}
</div>
