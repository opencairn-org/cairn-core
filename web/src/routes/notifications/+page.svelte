<script lang="ts">
	import { goto, refreshAll } from '$app/navigation';
	import { m } from '#lib/paraglide/messages.js';

	// Types with their own label and deep link; the rest render their i18n key.
	const typeMeta: Record<string, { label: string; href: string; link: string }> = {
		sync_gear_decision_needed: {
			label: m.notification_gear_decision_label(),
			href: '/gear',
			link: m.notification_gear_decision_link()
		}
	};
	import { formatRelativeDate } from '#lib/format.js';
	import { clients, isUnauthenticated } from '#lib/connect.js';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	let busy = $state(false);
	let actionError = $state<string | null>(null);

	// Client-side mutations (static SPA: no server form actions). Each calls
	// Connect directly, then refreshAll() re-runs the load to refresh.
	async function markRead(id: string) {
		busy = true;
		actionError = null;
		try {
			await clients.notification.markNotificationRead({ id });
			await refreshAll();
		} catch (err) {
			if (isUnauthenticated(err)) return goto('/login');
			actionError = (err as Error).message;
		} finally {
			busy = false;
		}
	}

	async function markAllRead() {
		busy = true;
		actionError = null;
		try {
			await clients.notification.markAllNotificationsRead({});
			await refreshAll();
		} catch (err) {
			if (isUnauthenticated(err)) return goto('/login');
			actionError = (err as Error).message;
		} finally {
			busy = false;
		}
	}

	function toggleUnread(checked: boolean) {
		goto(checked ? '/notifications?unread=1' : '/notifications');
	}

	function severityColor(sev: string): string {
		switch (sev) {
			case 'error':
				return 'text-red-400 border-red-700/50 bg-red-950/30';
			case 'warn':
				return 'text-amber-300 border-amber-700/50 bg-amber-950/30';
			case 'info':
				return 'text-sky-300 border-sky-700/50 bg-sky-950/30';
		}
		return 'text-zinc-300 border-zinc-700 bg-zinc-900/40';
	}
</script>

<section class="space-y-6">
	<header class="flex items-baseline justify-between gap-4 max-md:flex-col">
		<div>
			<h1 class="text-3xl font-semibold tracking-tight max-md:text-2xl">
				{m.notifications_title()}
			</h1>
			<p class="mt-1 text-sm text-zinc-400">{m.notifications_intro()}</p>
		</div>

		<div class="flex items-center gap-3 text-xs max-md:flex-wrap">
			<label class="flex items-center gap-2 text-zinc-400">
				<input
					type="checkbox"
					checked={data.onlyUnread}
					onchange={(e) => toggleUnread(e.currentTarget.checked)}
					class="rounded border-zinc-700 bg-zinc-900"
				/>
				nur ungelesen / unread only
			</label>
			<button
				type="button"
				disabled={busy}
				onclick={markAllRead}
				class="rounded border border-zinc-700 px-2 py-1 text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				Mark all read
			</button>
		</div>
	</header>

	{#if actionError}
		<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
			{actionError}
		</div>
	{/if}

	{#if data.notifications.length === 0}
		<div
			class="rounded-lg border border-dashed border-zinc-700 bg-zinc-900/30 p-8 text-center text-sm text-zinc-400"
		>
			{data.onlyUnread ? '—' : m.notifications_pending()}
		</div>
	{:else}
		<ul class="space-y-2">
			{#each data.notifications as n (n.id)}
				<li
					class="rounded-lg border px-5 py-3 text-sm transition-colors {severityColor(n.severity)}"
					class:opacity-60={n.read}
				>
					<div class="flex items-start gap-4">
						<div class="min-w-0 flex-1">
							<div class="flex items-center gap-2 font-medium">
								<span class="font-mono text-xs uppercase tracking-wide">{n.type}</span>
								{#if n.coalesceCount > 1}
									<span
										class="rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] font-medium text-zinc-300"
									>
										×{n.coalesceCount}
									</span>
								{/if}
							</div>
							<div class="mt-1 text-zinc-300">
								<span class="font-mono text-xs text-zinc-400">
									{typeMeta[n.type]?.label ?? n.titleI18nKey}
								</span>
								{#if Object.keys(n.params).length > 0}
									<details class="mt-1 text-xs text-zinc-500">
										<summary class="cursor-pointer">params</summary>
										<pre
											class="mt-1 rounded bg-black/30 p-2 text-[11px] leading-relaxed">{JSON.stringify(
												n.params,
												null,
												2
											)}</pre>
									</details>
								{/if}
							</div>
							{#if typeMeta[n.type] !== undefined}
								{@const meta = typeMeta[n.type] as { href: string; link: string }}
								<a
									href={meta.href}
									class="mt-2 inline-block text-xs text-accent-400 hover:text-accent-300"
								>
									{meta.link}
								</a>
							{/if}
							{#if n.activityId}
								<a
									href={`/activities/${n.activityId}`}
									class="mt-2 inline-block text-xs text-accent-400 hover:text-accent-300"
								>
									→ Activity
								</a>
							{/if}
						</div>
						<div class="flex shrink-0 flex-col items-end gap-2">
							<time class="text-xs text-zinc-500">
								{formatRelativeDate(n.createdAt)}
							</time>
							{#if !n.read}
								<button
									type="button"
									disabled={busy}
									onclick={() => markRead(n.id)}
									class="rounded border border-zinc-700 px-2 py-0.5 text-[10px] text-zinc-400 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50 max-md:py-2"
								>
									mark read
								</button>
							{/if}
						</div>
					</div>
				</li>
			{/each}
		</ul>
	{/if}
</section>
