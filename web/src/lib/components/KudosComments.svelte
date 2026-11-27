<script lang="ts">
	import { onMount } from 'svelte';
	import { formatRelativeDate } from '#lib/format.js';

	// Kudos + comments for an activity. Self-contained: it probes the social
	// endpoints; if the viewer isn't permitted (403) it renders nothing.
	let { activityId }: { activityId: string } = $props();

	type Card = { id: string; username: string; display_name: string; avatar_url: string };
	type Comment = { id: string; body: string; created_at: string; author: Card };

	let permitted = $state(true);
	let count = $state(0);
	let hasKudos = $state(false);
	let kudosers = $state<Card[]>([]);
	let comments = $state<Comment[]>([]);
	let draft = $state('');
	let busy = $state(false);

	async function loadKudos() {
		const res = await fetch(`/api/activities/${activityId}/kudos`);
		if (res.status === 403) {
			permitted = false;
			return;
		}
		if (!res.ok) return;
		const b = await res.json();
		count = b.count;
		hasKudos = b.has_kudos;
		kudosers = b.kudosers ?? [];
	}
	async function loadComments() {
		const res = await fetch(`/api/activities/${activityId}/comments`);
		if (!res.ok) return;
		const b = await res.json();
		comments = b.comments ?? [];
	}

	async function toggleKudos() {
		busy = true;
		try {
			const res = await fetch(`/api/activities/${activityId}/kudos`, {
				method: hasKudos ? 'DELETE' : 'POST',
				headers: { 'Content-Type': 'application/json' }
			});
			if (res.ok) {
				const b = await res.json();
				count = b.count;
				hasKudos = b.has_kudos;
				await loadKudos();
			}
		} finally {
			busy = false;
		}
	}

	async function postComment() {
		const text = draft.trim();
		if (!text) return;
		busy = true;
		try {
			const res = await fetch(`/api/activities/${activityId}/comments`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ body: text })
			});
			if (res.ok) {
				draft = '';
				await loadComments();
			}
		} finally {
			busy = false;
		}
	}

	async function deleteComment(id: string) {
		const res = await fetch(`/api/comments/${id}`, { method: 'DELETE' });
		if (res.ok) comments = comments.filter((c) => c.id !== id);
	}

	onMount(async () => {
		await loadKudos();
		if (permitted) await loadComments();
	});
</script>

{#if permitted}
	<section class="mt-6 rounded-lg border border-zinc-800 p-4">
		<div class="mb-4 flex items-center gap-3">
			<button
				onclick={toggleKudos}
				disabled={busy}
				class={`flex items-center gap-2 rounded-full px-4 py-2 text-sm font-medium ${hasKudos ? 'bg-orange-600 text-white' : 'border border-zinc-700 text-zinc-300 hover:border-orange-600'} disabled:opacity-50`}
			>
				<span>👏</span>
				<span>{hasKudos ? 'Given' : 'Kudos'}</span>
			</button>
			<span class="text-sm text-zinc-400">{count} {count === 1 ? 'kudos' : 'kudos'}</span>
		</div>
		{#if kudosers.length > 0}
			<div class="mb-4 flex flex-wrap gap-2 text-xs text-zinc-400">
				{#each kudosers as k (k.id)}
					<a href={`/u/${k.username}`} class="hover:underline">{k.display_name || k.username}</a>
				{/each}
			</div>
		{/if}

		<h3 class="mb-2 text-sm font-semibold text-zinc-300">Comments</h3>
		<div class="space-y-3">
			{#each comments as c (c.id)}
				<div class="rounded border border-zinc-800 p-3 text-sm">
					<div class="mb-1 flex items-center justify-between text-xs text-zinc-500">
						<a href={`/u/${c.author?.username}`} class="font-medium text-zinc-300 hover:underline">
							{c.author?.display_name || c.author?.username}
						</a>
						<span>{formatRelativeDate(c.created_at)}</span>
					</div>
					<p class="whitespace-pre-line text-zinc-200">{c.body}</p>
					<button
						onclick={() => deleteComment(c.id)}
						class="mt-1 text-xs text-zinc-600 hover:text-red-400"
					>
						Delete
					</button>
				</div>
			{/each}
			{#if comments.length === 0}
				<p class="text-sm text-zinc-500">No comments yet.</p>
			{/if}
		</div>

		<div class="mt-3 flex gap-2">
			<input
				bind:value={draft}
				placeholder="Add a comment…"
				onkeydown={(e) => e.key === 'Enter' && postComment()}
				class="flex-1 rounded border border-zinc-700 bg-zinc-900 px-3 py-2 text-sm text-zinc-100"
			/>
			<button
				onclick={postComment}
				disabled={busy || !draft.trim()}
				class="rounded bg-blue-600 px-4 py-2 text-sm font-medium text-white hover:bg-blue-700 disabled:opacity-50"
			>
				Post
			</button>
		</div>
	</section>
{/if}
