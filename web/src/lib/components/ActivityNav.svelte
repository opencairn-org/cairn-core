<script lang="ts">
	import { page } from '$app/state';
	import { m } from '#lib/paraglide/messages.js';

	// Shared header of the activity pages: back link + sub-page buttons. On a
	// sub-page the back link returns to the activity, on the activity to the list.
	let { id, title = '' }: { id: string; title?: string } = $props();

	const base = $derived(`/activities/${id}`);
	const links = $derived([
		{ href: `${base}/similar`, label: 'Similar routes' },
		{ href: `${base}/edit`, label: 'Edit' },
		{ href: `${base}/manage`, label: 'Manage & insights' }
	]);
	const onSubpage = $derived(page.url.pathname !== base);
</script>

<div class="flex items-center justify-between gap-4 max-md:flex-wrap">
	{#if onSubpage}
		<a href={base} class="text-xs text-accent-400 hover:text-accent-300">
			← {title || 'Activity'}
		</a>
	{:else}
		<a href="/activities" class="text-xs text-accent-400 hover:text-accent-300">{m.detail_back()}</a
		>
	{/if}
	<nav class="flex items-center gap-2 max-md:flex-wrap">
		{#each links as l (l.href)}
			{@const active = page.url.pathname === l.href}
			<a
				href={l.href}
				aria-current={active ? 'page' : undefined}
				class="rounded border px-2.5 py-1 text-xs {active
					? 'border-accent-500 text-accent-300'
					: 'border-zinc-700 text-zinc-300 hover:border-accent-500 hover:text-accent-300'}"
			>
				{l.label}
			</a>
		{/each}
	</nav>
</div>
