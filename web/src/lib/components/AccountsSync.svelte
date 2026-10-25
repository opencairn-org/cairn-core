<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import { refreshAll } from '$app/navigation';
	import { m } from '#lib/paraglide/messages.js';
	import { formatDate } from '#lib/format.js';

	// When `connectionId` is set, only the account belonging to that connection
	// is shown (no outer "Connected accounts" header) — used to embed an
	// account's stats inside its connection card.
	let { connectionId = '' }: { connectionId?: string } = $props();

	type Queue = {
		pending: number;
		in_progress: number;
		done: number;
		failed: number;
		skipped: number;
	};
	type BudgetWindow = { used: number; limit: number; resets_at: string };
	type Account = {
		id: string;
		provider: string;
		connection_id: string | null;
		provider_account_id: string;
		label: string;
		status: string;
		imported: number;
		provider_total: number;
		last_sync_at: string | null;
		watermark: string | null;
		rate_limited: boolean;
		rate_limit_budget: { short?: BudgetWindow; daily?: BudgetWindow } | null;
		discovering: boolean;
		poll_interval_seconds: number;
		auto_import_enabled: boolean;
		assigned_worker: string;
		worker_pools: {
			name: string;
			package: string;
			parser_version: string;
			release: string;
			online: boolean;
		}[];
		out_of_date?: Record<string, number>;
		queue: Queue;
	};
	type Preview = { total: number; already_present: number; new: number; complete?: boolean };

	function fmtWhen(iso: string | null): string {
		if (!iso) return 'never';
		return formatDate(iso);
	}

	// Consolidated, human-readable connection status derived from the account's
	// auth state, rate-limit flag and import-queue counts. Ordered by severity /
	// salience so the single badge always reflects what the connection is doing
	// right now.
	type Tone = 'live' | 'warn' | 'err' | 'idle';
	function connStatus(a: Account): { label: string; tone: Tone } {
		if (a.status === 'needs_reauth') return { label: 'Needs reauth', tone: 'err' };
		if (a.status && a.status !== 'active')
			return { label: a.status.replace(/_/g, ' '), tone: 'warn' };
		if (a.discovering) return { label: 'Discovering', tone: 'live' };
		if (a.rate_limited) return { label: 'Rate limited', tone: 'warn' };
		if (a.queue.in_progress > 0) return { label: 'Importing', tone: 'live' };
		if (a.queue.pending > 0) return { label: 'Waiting', tone: 'warn' };
		if (a.queue.failed > 0) return { label: 'Errors', tone: 'err' };
		return { label: 'Idle', tone: 'idle' };
	}
	const toneClass: Record<Tone, string> = {
		live: 'bg-sky-500/15 text-sky-300',
		warn: 'bg-amber-500/15 text-amber-300',
		err: 'bg-red-500/15 text-red-300',
		idle: 'bg-emerald-500/15 text-emerald-300'
	};
	const toneDot: Record<Tone, string> = {
		live: 'bg-sky-400 animate-pulse',
		warn: 'bg-amber-400',
		err: 'bg-red-400',
		idle: 'bg-emerald-400'
	};

	let configOpen = $state<Record<string, boolean>>({});
	let savedCfg = $state<Record<string, boolean>>({});

	type HistoryEvent = {
		kind: string;
		count: number;
		detail: string;
		external_id: string;
		activity_id?: string;
		external_url?: string;
		status: string; // completed | ongoing | failed
		occurred_at: string;
	};
	const histStatusClass: Record<string, string> = {
		completed: 'bg-emerald-500/15 text-emerald-300',
		ongoing: 'bg-sky-500/15 text-sky-300',
		failed: 'bg-red-500/15 text-red-300'
	};
	let historyOpen = $state<Record<string, boolean>>({});
	let history = $state<Record<string, HistoryEvent[]>>({});

	async function toggleHistory(id: string) {
		historyOpen = { ...historyOpen, [id]: !historyOpen[id] };
		if (historyOpen[id]) await loadHistory(id);
	}
	async function loadHistory(id: string) {
		try {
			const res = await fetch(`/api/accounts/${id}/history`);
			if (res.ok) history = { ...history, [id]: (await res.json()).events ?? [] };
		} catch {
			/* ignore */
		}
	}
	const HISTORY_LABEL: Record<string, string> = {
		sync_started: 'Full sync started',
		activity_imported: 'Imported',
		activity_updated: 'Updated',
		failed: 'Failed'
	};

	// Debugging view: the actual import-queue rows (pending / in_progress /
	// failed) behind the counts, so a stuck import is visible with its
	// attempts + last error.
	type QueueItem = {
		id: string;
		item_type: string;
		external_id: string;
		status: string;
		priority: number;
		attempts: number;
		last_error: string;
		item_time?: string;
		created_at: string;
		started_at?: string;
		completed_at?: string;
		external_url?: string;
	};
	const queueStatusClass: Record<string, string> = {
		pending: 'bg-zinc-700/40 text-zinc-300',
		in_progress: 'bg-sky-500/15 text-sky-300',
		done: 'bg-emerald-500/15 text-emerald-300',
		failed: 'bg-red-500/15 text-red-300',
		skipped: 'bg-zinc-800 text-zinc-500'
	};
	let queueOpen = $state<Record<string, boolean>>({});
	let queueItems = $state<Record<string, QueueItem[]>>({});
	let queueShowAll = $state<Record<string, boolean>>({});

	async function toggleQueue(id: string) {
		queueOpen = { ...queueOpen, [id]: !queueOpen[id] };
		if (queueOpen[id]) await loadQueueItems(id);
	}
	async function loadQueueItems(id: string) {
		try {
			const status = queueShowAll[id] ? 'all' : '';
			const res = await fetch(
				`/api/accounts/${id}/queue/items?limit=100${status ? `&status=${status}` : ''}`
			);
			if (res.ok) queueItems = { ...queueItems, [id]: (await res.json()).items ?? [] };
		} catch {
			/* ignore */
		}
	}
	async function toggleQueueShowAll(id: string) {
		queueShowAll = { ...queueShowAll, [id]: !queueShowAll[id] };
		await loadQueueItems(id);
	}

	// Per-row queue actions: requeue a failed item, or bump a pending one to
	// the front of the claim order. Both refresh the rows; requeue also moves
	// the failed→pending counters, so reload the accounts too.
	async function queueItemAction(
		accountId: string,
		itemId: string,
		action: 'requeue' | 'move-to-top'
	) {
		try {
			const res = await fetch(`/api/accounts/${accountId}/queue/items/${itemId}/${action}`, {
				method: 'POST'
			});
			if (!res.ok) {
				error = await errText(res);
				return;
			}
			await loadQueueItems(accountId);
			if (action === 'requeue') await loadAccounts();
		} catch {
			/* ignore */
		}
	}

	// Per-connection poll interval, edited in minutes (0 = instance default).
	async function saveConfig(id: string, minutes: number) {
		try {
			const res = await fetch(`/api/accounts/${id}/config`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ poll_interval_seconds: Math.max(0, Math.round(minutes * 60)) })
			});
			if (res.ok) {
				savedCfg = { ...savedCfg, [id]: true };
				await loadAccounts();
				setTimeout(() => (savedCfg = { ...savedCfg, [id]: false }), 1500);
			}
		} catch {
			/* ignore */
		}
	}

	// Suspend/resume automatic imports for one account (#93).
	async function toggleAutoImport(id: string, enabled: boolean) {
		try {
			const res = await fetch(`/api/accounts/${id}/auto-import`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ enabled })
			});
			if (res.ok) await loadAccounts();
		} catch {
			/* ignore */
		}
	}

	// Quota-free re-parse of every source a newer parser of its pool can read.
	let reparsed = $state<Record<string, string>>({});
	async function reparseOutOfDate(id: string) {
		busy = { ...busy, [id]: true };
		try {
			const res = await fetch(`/api/accounts/${id}/reparse-out-of-date`, { method: 'POST' });
			if (!res.ok) {
				error = (await res.text()).trim() || res.statusText;
				return;
			}
			const b = await res.json();
			reparsed = {
				...reparsed,
				[id]: `${b.queued} queued${b.remaining ? `, ${b.remaining} left` : ''}`
			};
			await loadAccounts();
		} finally {
			busy = { ...busy, [id]: false };
		}
	}

	// Route the account's jobs to a named worker pool ("" = the only one).
	async function setWorker(id: string, worker: string) {
		try {
			const res = await fetch(`/api/accounts/${id}/worker`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ worker })
			});
			if (!res.ok) error = (await res.text()).trim() || res.statusText;
			await loadAccounts();
		} catch {
			/* ignore */
		}
	}

	let accounts = $state<Account[]>([]);
	let previews = $state<Record<string, Preview>>({});
	let busy = $state<Record<string, boolean>>({});
	let error = $state<string | null>(null);
	let poll: ReturnType<typeof setInterval> | null = null;
	// Instance-wide reconcile cadence, so the override setting can spell out
	// what "0 = instance default" means.
	let defaultPollSeconds = $state(0);
	const defaultPollLabel = $derived(
		defaultPollSeconds > 0 ? `${Math.round(defaultPollSeconds / 60)} min` : ''
	);

	async function loadAccounts() {
		try {
			const res = await fetch('/api/accounts');
			if (!res.ok) return; // not connected / not authed
			const body = await res.json();
			accounts = body.accounts ?? [];
			defaultPollSeconds = body.default_poll_interval_seconds ?? 0;
		} catch {
			/* ignore */
		}
	}

	// Extracts a human message from an error response, unwrapping the JSON
	// rate-limit envelope ({error,message}) the server sends on 429.
	async function errText(res: Response): Promise<string> {
		const raw = (await res.text()).trim();
		try {
			const j = JSON.parse(raw);
			if (j && j.message) return j.message;
		} catch {
			/* not json */
		}
		return raw;
	}

	async function preview(id: string) {
		busy = { ...busy, [id]: true };
		error = null;
		try {
			const res = await fetch(`/api/accounts/${id}/sync/preview`, { method: 'POST' });
			if (!res.ok) {
				const msg = await errText(res);
				// On rate-limit the server persisted a cooldown; refresh so the
				// connection's status badge flips to "Rate limited" right away.
				if (res.status === 429) await loadAccounts();
				throw new Error(msg);
			}
			previews = { ...previews, [id]: await res.json() };
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = { ...busy, [id]: false };
		}
	}

	let notice = $state<string | null>(null);

	// Single-activity import: per-account toggle + provider-ID input.
	let importOpen = $state<Record<string, boolean>>({});
	let importExtId = $state<Record<string, string>>({});

	async function importOne(id: string) {
		const ext = (importExtId[id] ?? '').trim();
		if (!ext) return;
		busy = { ...busy, [id]: true };
		error = null;
		notice = null;
		try {
			const res = await fetch(`/api/accounts/${id}/import-one`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ external_id: ext })
			});
			if (!res.ok) throw new Error(await errText(res));
			const out = await res.json();
			notice = out.queued
				? `Activity ${ext} queued — it imports with the next processor tick.`
				: `Activity ${ext} is already queued.`;
			importExtId = { ...importExtId, [id]: '' };
			importOpen = { ...importOpen, [id]: false };
			await loadAccounts();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = { ...busy, [id]: false };
		}
	}

	// Garmin-only: queue a health-metric import (HRV/sleep/weight/steps/RHR)
	// for the last 30 days on the account's worker.
	async function importHealth(id: string) {
		busy = { ...busy, [id]: true };
		error = null;
		notice = null;
		try {
			const res = await fetch(`/api/accounts/${id}/import-health?days=30`, { method: 'POST' });
			if (!res.ok) throw new Error(await errText(res));
			notice =
				'Health import queued (last 30 days) — data appears under Health as the worker processes it.';
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = { ...busy, [id]: false };
		}
	}

	// Ask the account's worker for its gear list; 409 = the worker does not
	// import gear.
	async function syncGear(id: string) {
		busy = { ...busy, [id]: true };
		error = null;
		notice = null;
		try {
			const res = await fetch(`/api/accounts/${id}/sync-gear`, { method: 'POST' });
			if (!res.ok) throw new Error(await errText(res));
			notice = m.gear_sync_queued();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = { ...busy, [id]: false };
		}
	}

	async function start(id: string, skipExisting: boolean) {
		busy = { ...busy, [id]: true };
		error = null;
		try {
			const res = await fetch(`/api/accounts/${id}/sync/start`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ skip_existing: skipExisting })
			});
			if (!res.ok) {
				const msg = await errText(res);
				if (res.status === 429) await loadAccounts();
				throw new Error(msg);
			}
			const rest = { ...previews };
			delete rest[id];
			previews = rest;
			await loadAccounts();
		} catch (e) {
			error = (e as Error).message;
		} finally {
			busy = { ...busy, [id]: false };
		}
	}

	// In single-connection mode, only the matching account is shown.
	const shown = $derived(
		connectionId ? accounts.filter((a) => a.connection_id === connectionId) : accounts
	);
	const hasPending = $derived(accounts.some((a) => a.queue.pending > 0 || a.queue.in_progress > 0));

	onMount(() => {
		loadAccounts();
		// Poll while there's queued work so the activities list + counts update.
		poll = setInterval(async () => {
			await loadAccounts();
			if (hasPending) await refreshAll();
		}, 5000);
	});
	onDestroy(() => poll && clearInterval(poll));
</script>

{#if shown.length > 0}
	<section class={connectionId ? '' : 'rounded-lg border border-zinc-800 bg-zinc-900/40 p-4'}>
		{#if !connectionId}
			<h2 class="mb-3 text-sm font-medium text-zinc-300">Connected accounts</h2>
		{/if}
		{#if error}
			<div
				class="mb-3 rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300"
			>
				{error}
			</div>
		{/if}
		{#if notice}
			<div
				class="mb-3 rounded border border-emerald-700/50 bg-emerald-950/30 px-3 py-2 text-xs text-emerald-300"
			>
				{notice}
			</div>
		{/if}
		<ul class="space-y-3">
			{#each shown as a (a.id)}
				{@const st = connStatus(a)}
				{@const pv = previews[a.id]}
				<li class="rounded border border-zinc-800 bg-zinc-950/40 px-4 py-3">
					<div class="flex items-center justify-between gap-4 max-md:flex-wrap">
						<div class="text-sm">
							<span class="font-medium">
								{a.provider_account_id ? `Account ${a.provider_account_id}` : a.label}
							</span>
							{#if a.assigned_worker && accounts.some((o) => o.id !== a.id && o.provider === a.provider && o.provider_account_id && o.provider_account_id === a.provider_account_id)}
								<span
									class="ml-2 rounded bg-zinc-800 px-1.5 py-0.5 text-[10px] text-zinc-300"
									title="This account is connected once per importer; this connection uses this one"
									>via {a.assigned_worker}</span
								>
							{/if}
							<span
								class="ml-2 inline-flex items-center gap-1.5 rounded px-1.5 py-0.5 text-[10px] uppercase tracking-wide {toneClass[
									st.tone
								]}"
								title="Current connection status"
							>
								<span class="h-1.5 w-1.5 rounded-full {toneDot[st.tone]}"></span>
								{st.label}
							</span>
						</div>
						<div class="flex items-center gap-3 text-xs max-md:flex-wrap">
							{#if a.queue.pending > 0 || a.queue.in_progress > 0}
								<span class="text-amber-300">
									{a.queue.in_progress} importing · {a.queue.pending} queued
								</span>
							{/if}
							{#if a.queue.failed > 0}<span class="text-red-400">{a.queue.failed} failed</span>{/if}
							{#if !pv}
								<button
									type="button"
									disabled={busy[a.id]}
									onclick={() => preview(a.id)}
									class="rounded border border-accent-500 bg-accent-500/20 px-2.5 py-1 text-accent-300 hover:bg-accent-500/30 disabled:opacity-50 max-md:py-2"
								>
									{busy[a.id] ? 'Checking…' : 'Full sync'}
								</button>
							{/if}
							<button
								type="button"
								onclick={() => (importOpen = { ...importOpen, [a.id]: !importOpen[a.id] })}
								class="rounded border border-zinc-700 px-2.5 py-1 text-zinc-300 hover:border-accent-500 hover:text-accent-300 max-md:py-2"
							>
								{importOpen[a.id] ? 'Cancel import' : 'Import activity'}
							</button>
							{#if (a.out_of_date?.newer_parser ?? 0) > 0}
								<button
									type="button"
									disabled={busy[a.id]}
									onclick={() => reparseOutOfDate(a.id)}
									title="A newer parser is available for these activities. Re-parses them from their archived originals; no provider quota is used."
									class="rounded border border-sky-700/60 px-2.5 py-1 text-sky-300 hover:border-sky-500 disabled:opacity-50 max-md:py-2"
								>
									{reparsed[a.id] ?? `Re-parse ${a.out_of_date?.newer_parser} out of date`}
								</button>
							{/if}
							<button
								type="button"
								disabled={busy[a.id]}
								onclick={() => syncGear(a.id)}
								class="rounded border border-zinc-700 px-2.5 py-1 text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50 max-md:py-2"
							>
								Sync gear
							</button>
							{#if a.provider === 'garmin'}
								<button
									type="button"
									disabled={busy[a.id]}
									onclick={() => importHealth(a.id)}
									class="rounded border border-zinc-700 px-2.5 py-1 text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50 max-md:py-2"
								>
									Import health
								</button>
							{/if}
						</div>
					</div>

					{#if importOpen[a.id]}
						<!-- Single-activity import: provider activity ID → import queue. -->
						<div
							class="mt-2 flex items-center gap-2 rounded border border-zinc-800 bg-zinc-900/60 p-2 text-xs max-md:flex-wrap"
						>
							<input
								bind:value={importExtId[a.id]}
								placeholder={`${a.provider} activity ID`}
								onkeydown={(e) => e.key === 'Enter' && importOne(a.id)}
								class="flex-1 rounded border border-zinc-700 bg-zinc-950 px-2.5 py-1.5 font-mono text-zinc-200 focus:border-accent-400 focus:outline-none max-md:basis-full"
							/>
							<button
								type="button"
								disabled={busy[a.id] || !(importExtId[a.id] ?? '').trim()}
								onclick={() => importOne(a.id)}
								class="rounded bg-accent-500 px-3 py-1.5 font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
							>
								{busy[a.id] ? 'Queueing…' : 'Import'}
							</button>
						</div>
					{/if}

					<!-- Detailed per-connection stats -->
					<dl class="mt-2 flex flex-wrap gap-x-6 gap-y-1 text-xs text-zinc-400">
						<div>
							<dt class="inline text-zinc-500">Imported:</dt>
							<dd class="inline font-medium text-zinc-200">{a.imported}</dd>
							{#if pv}
								<span class="text-zinc-500"> / {pv.total} on {a.provider}</span>
							{:else if a.provider_total > 0}
								<span class="text-zinc-500"> / {a.provider_total} on {a.provider}</span>
							{/if}
						</div>
						<div>
							<dt class="inline text-zinc-500">In queue:</dt>
							<dd class="inline text-zinc-200">{a.queue.pending + a.queue.in_progress}</dd>
						</div>
						{#if a.queue.failed > 0}
							<div>
								<dt class="inline text-zinc-500">Failed:</dt>
								<dd class="inline text-red-400">{a.queue.failed}</dd>
							</div>
						{/if}
						{#if a.rate_limit_budget?.short || a.rate_limit_budget?.daily}
							{@const b = a.rate_limit_budget}
							<div title="Provider API budget (from the worker's live usage headers)">
								<dt class="inline text-zinc-500">API budget:</dt>
								<dd class="inline">
									{#if b.short}
										<span
											class={b.short.used >= b.short.limit ? 'text-amber-300' : 'text-zinc-200'}
										>
											{b.short.used}/{b.short.limit}
										</span>
										<span class="text-zinc-600">·15min</span>
									{/if}
									{#if b.daily}
										<span
											class={b.daily.used >= b.daily.limit ? 'text-amber-300' : 'text-zinc-200'}
										>
											{b.daily.used}/{b.daily.limit}
										</span>
										<span class="text-zinc-600">·day</span>
									{/if}
								</dd>
							</div>
						{/if}
						<div>
							<dt class="inline text-zinc-500">Last sync:</dt>
							<dd class="inline text-zinc-200">{fmtWhen(a.last_sync_at)}</dd>
						</div>
						<div>
							<dt class="inline text-zinc-500">Up to:</dt>
							<dd class="inline text-zinc-200">{fmtWhen(a.watermark)}</dd>
						</div>
						<button
							type="button"
							onclick={() => (configOpen = { ...configOpen, [a.id]: !configOpen[a.id] })}
							class="text-accent-400 hover:text-accent-300"
						>
							⚙ Settings
						</button>
						<button
							type="button"
							onclick={() => toggleHistory(a.id)}
							class="text-accent-400 hover:text-accent-300"
						>
							🕑 History
						</button>
						<button
							type="button"
							onclick={() => toggleQueue(a.id)}
							class="text-accent-400 hover:text-accent-300"
						>
							⏳ Queue
						</button>
					</dl>

					{#if historyOpen[a.id]}
						<div class="mt-3 rounded border border-zinc-800 bg-zinc-950/40 px-3 py-2 text-xs">
							{#if (history[a.id] ?? []).length === 0}
								<p class="text-zinc-500">No import history yet.</p>
							{:else}
								<ul class="space-y-1">
									{#each history[a.id] as ev, i (i)}
										<li class="flex items-baseline justify-between gap-3 max-md:flex-wrap">
											<span class="text-zinc-300">
												<span
													class="mr-1.5 rounded px-1 py-0.5 text-[10px] uppercase tracking-wide {histStatusClass[
														ev.status
													] ?? 'bg-zinc-800 text-zinc-400'}"
												>
													{ev.status}
												</span>
												<span class="text-zinc-500">{HISTORY_LABEL[ev.kind] ?? ev.kind}</span>
												{#if ev.kind === 'sync_started'}
													<span class="text-zinc-200">— {ev.count} queued</span>
												{:else if ev.activity_id}
													<a
														href={`/activities/${ev.activity_id}`}
														class="text-accent-400 hover:text-accent-300 hover:underline"
													>
														{ev.detail || 'View activity'}
													</a>
												{:else}
													<span class="text-zinc-200">{ev.detail}</span>
												{/if}
												{#if ev.external_url}
													<a
														href={ev.external_url}
														target="_blank"
														rel="noopener noreferrer"
														class="ml-1 text-zinc-500 hover:text-accent-300"
														title={`Open original on ${a.provider}`}>↗ {a.provider}</a
													>
												{/if}
											</span>
											<span class="shrink-0 text-zinc-600">{formatDate(ev.occurred_at)}</span>
										</li>
									{/each}
								</ul>
							{/if}
						</div>
					{/if}

					{#if queueOpen[a.id]}
						<div class="mt-3 rounded border border-zinc-800 bg-zinc-950/40 px-3 py-2 text-xs">
							<div class="mb-2 flex items-center justify-between gap-2">
								<span class="text-zinc-400">
									Import queue — {a.queue.pending} pending · {a.queue.in_progress} in progress ·
									{a.queue.failed} failed · {a.queue.done} done
								</span>
								<div class="flex items-center gap-2">
									<button
										type="button"
										onclick={() => toggleQueueShowAll(a.id)}
										class="text-accent-400 hover:text-accent-300"
									>
										{queueShowAll[a.id] ? 'Show live only' : 'Show all (incl. done)'}
									</button>
									<button
										type="button"
										onclick={() => loadQueueItems(a.id)}
										class="text-accent-400 hover:text-accent-300"
									>
										↻ Refresh
									</button>
								</div>
							</div>
							{#if (queueItems[a.id] ?? []).length === 0}
								<p class="text-zinc-500">
									{queueShowAll[a.id]
										? 'Queue is empty.'
										: 'Nothing pending, in progress, or failed.'}
								</p>
							{:else}
								<div class="overflow-x-auto">
									<table class="w-full text-left">
										<thead>
											<tr class="text-[10px] uppercase tracking-wide text-zinc-500">
												<th class="py-1 pr-3 font-medium">Status</th>
												<th class="py-1 pr-3 font-medium">Activity</th>
												<th class="py-1 pr-3 font-medium">Tries</th>
												<th class="py-1 pr-3 font-medium">Queued</th>
												<th class="py-1 pr-3 font-medium">Started</th>
												<th class="py-1 pr-3 font-medium">Last error</th>
												<th class="py-1 font-medium"></th>
											</tr>
										</thead>
										<tbody>
											{#each queueItems[a.id] as it (it.id)}
												<tr class="border-t border-zinc-800/60 align-top">
													<td class="py-1 pr-3">
														<span
															class="rounded px-1 py-0.5 text-[10px] uppercase tracking-wide {queueStatusClass[
																it.status
															] ?? 'bg-zinc-800 text-zinc-400'}"
														>
															{it.status.replace(/_/g, ' ')}
														</span>
														{#if it.priority > 0 && it.status === 'pending'}
															<span class="ml-1 text-accent-400" title="Moved to top">⇧</span>
														{/if}
													</td>
													<td class="py-1 pr-3 font-mono text-zinc-200">
														{#if it.external_url}
															<a
																href={it.external_url}
																target="_blank"
																rel="noopener noreferrer"
																class="hover:text-accent-300 hover:underline"
																title={`Open on ${a.provider}`}>{it.external_id}</a
															>
														{:else}
															{it.external_id}
														{/if}
														{#if it.item_time}
															<span class="ml-1 text-zinc-500">({formatDate(it.item_time)})</span>
														{/if}
													</td>
													<td class="py-1 pr-3 text-zinc-300">{it.attempts}</td>
													<td class="py-1 pr-3 whitespace-nowrap text-zinc-400"
														>{formatDate(it.created_at)}</td
													>
													<td class="py-1 pr-3 whitespace-nowrap text-zinc-400">
														{it.started_at ? formatDate(it.started_at) : '—'}
													</td>
													<td class="max-w-64 py-1 pr-3 text-red-300/90">
														{it.last_error || ''}
													</td>
													<td class="py-1 whitespace-nowrap text-right">
														{#if it.status === 'failed'}
															<button
																type="button"
																onclick={() => queueItemAction(a.id, it.id, 'requeue')}
																class="text-accent-400 hover:text-accent-300"
																title="Queue this item for another import attempt"
															>
																⟳ Requeue
															</button>
														{:else if it.status === 'pending'}
															<button
																type="button"
																onclick={() => queueItemAction(a.id, it.id, 'move-to-top')}
																class="text-accent-400 hover:text-accent-300"
																title="Import this item next"
															>
																⇧ Top
															</button>
														{/if}
													</td>
												</tr>
											{/each}
										</tbody>
									</table>
								</div>
							{/if}
						</div>
					{/if}

					{#if configOpen[a.id]}
						<div
							class="mt-3 flex flex-wrap items-center gap-2 rounded border border-zinc-800 bg-zinc-950/40 px-3 py-2 text-xs"
						>
							<label for={`poll-${a.id}`} class="text-zinc-400"
								>Check for new activities every</label
							>
							<input
								id={`poll-${a.id}`}
								type="number"
								min="0"
								step="5"
								value={a.poll_interval_seconds ? Math.round(a.poll_interval_seconds / 60) : 0}
								onchange={(e) => saveConfig(a.id, Number(e.currentTarget.value))}
								class="w-20 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-200 focus:border-accent-400 focus:outline-none"
							/>
							<span class="text-zinc-400">minutes</span>
							<span class="text-zinc-600">
								(0 = instance default{defaultPollLabel ? `: ${defaultPollLabel}` : ''})
							</span>
							{#if savedCfg[a.id]}<span class="text-emerald-400">saved</span>{/if}

							<label class="ml-auto flex items-center gap-2 text-zinc-300">
								<input
									type="checkbox"
									checked={a.auto_import_enabled}
									onchange={(e) => toggleAutoImport(a.id, e.currentTarget.checked)}
								/>
								Auto-import
							</label>
						</div>
						{#if (a.worker_pools ?? []).length > 1 || (a.assigned_worker && !(a.worker_pools ?? []).some((p) => p.name === a.assigned_worker))}
							<div class="mt-2 flex flex-wrap items-center gap-2 text-xs">
								<label class="text-zinc-400" for={`worker-${a.id}`}>Worker</label>
								<select
									id={`worker-${a.id}`}
									value={a.assigned_worker}
									onchange={(e) => setWorker(a.id, e.currentTarget.value)}
									class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-200"
								>
									<option value="">— choose —</option>
									{#each a.worker_pools ?? [] as p (p.name)}
										<option value={p.name}
											>{p.name} · {p.package}{p.online
												? ` · parser ${p.parser_version}`
												: ' · offline'}</option
										>
									{/each}
									{#if a.assigned_worker && !(a.worker_pools ?? []).some((p) => p.name === a.assigned_worker)}
										<option value={a.assigned_worker}>{a.assigned_worker} (offline)</option>
									{/if}
								</select>
								<span class="text-zinc-600"
									>Imports and re-fetches for this connection run on this worker.</span
								>
							</div>
						{/if}
					{/if}

					{#if (a.out_of_date?.newer_dataset ?? 0) > 0}
						<div class="mt-2 text-xs text-amber-300">
							{a.out_of_date?.newer_dataset} activities were imported in an older archive format and need
							a re-fetch from the provider to pick up the new parser.
						</div>
					{/if}

					{#if !a.auto_import_enabled}
						<div
							class="mt-2 rounded border border-amber-700/50 bg-amber-950/30 px-3 py-1.5 text-xs text-amber-300"
						>
							Auto-import is paused for this account — scheduled syncs and incoming webhooks are
							ignored. Manual sync still works. Re-enable it under the gear/config toggle.
						</div>
					{/if}

					{#if pv}
						{@const p = pv}
						<div
							class="mt-3 rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-xs text-zinc-300"
						>
							Found <b>{p.complete === false ? '≥' : ''}{p.total}</b> activities — <b>{p.new}</b>
							new,
							{p.already_present} already imported.
							{#if p.complete === false}
								<span class="text-amber-300"
									>(rate-limited — the full sync discovers the rest in the background.)</span
								>
							{/if}
							<div class="mt-2 flex gap-2 max-md:flex-wrap">
								<button
									type="button"
									disabled={busy[a.id]}
									onclick={() => start(a.id, true)}
									class="rounded bg-accent-500 px-3 py-1 font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50 max-md:py-2"
								>
									Import {p.new} new
								</button>
								<button
									type="button"
									disabled={busy[a.id]}
									onclick={() => start(a.id, false)}
									class="rounded border border-zinc-600 px-3 py-1 text-zinc-300 hover:border-zinc-500 disabled:opacity-50 max-md:py-2"
								>
									Re-download all {p.total}
								</button>
								<button
									type="button"
									onclick={() =>
										(previews = Object.fromEntries(
											Object.entries(previews).filter(([k]) => k !== a.id)
										))}
									class="px-2 py-1 text-zinc-500 hover:text-zinc-300"
								>
									Cancel
								</button>
							</div>
						</div>
					{/if}
				</li>
			{/each}
		</ul>
	</section>
{/if}
