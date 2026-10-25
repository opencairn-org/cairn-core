<script lang="ts">
	import { onMount } from 'svelte';
	import { formatDate } from '#lib/format.js';
	import ActivityNav from '#lib/components/ActivityNav.svelte';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();
	const a = $derived(data.activity);

	// --- share links ---
	type ShareLink = { token: string; path: string; created_at: string; active: boolean };
	let shareLinks = $state<ShareLink[]>([]);
	let shareBusy = $state(false);
	async function loadShares() {
		try {
			const res = await fetch(`/api/activities/${a.id}/shares`);
			if (res.ok) shareLinks = (await res.json()).links ?? [];
		} catch {
			/* ignore */
		}
	}
	async function createShare() {
		shareBusy = true;
		try {
			const res = await fetch(`/api/activities/${a.id}/share`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' }
			});
			if (res.ok) await loadShares();
		} finally {
			shareBusy = false;
		}
	}
	async function revokeShare(token: string) {
		const res = await fetch(`/api/shares/${token}`, { method: 'DELETE' });
		if (res.ok) shareLinks = shareLinks.filter((l) => l.token !== token);
	}
	function shareURL(path: string): string {
		return typeof window !== 'undefined' ? window.location.origin + path : path;
	}

	type ManageSource = {
		id: string;
		provider: string;
		external_id: string;
		external_account: string;
		worker_name: string;
		worker_version: string;
		worker_release: string;
		parser_version: string;
		dataset_version: string;
		reparse_with: string;
		upgrade: '' | 'new_parser' | 'new_dataset';
		worker_package: string;
		reparse_eligible: boolean;
		has_blob: boolean;
		raw_content_type: string;
		raw_size_bytes: number;
		has_stream: boolean;
		lap_count: number;
		status: string;
		status_reason: string;
		reimport_status: string;
		reimport_reason: string;
		imported_at: string;
		last_reimported_at: string | null;
		updated_at: string;
		is_primary: boolean;
		won_field_groups: string[];
	};
	type FieldProvenance = {
		field: string;
		field_label: string;
		source_id: string;
		provider: string;
		decided_by: string;
		synced_at: string;
	};
	type ManagePayload = {
		activity: { id: string; source_count: number; merged_at: string; primary: string };
		sources: ManageSource[];
		provenance?: FieldProvenance[];
	};

	let payload = $state<ManagePayload | null>(null);
	let loadError = $state<string | null>(null);
	let busy = $state<string | null>(null); // which action is running
	let actionMsg = $state<{ ok: boolean; text: string } | null>(null);

	async function loadManage() {
		loadError = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/manage`);
			if (!res.ok) throw new Error(await res.text());
			payload = await res.json();
		} catch (e) {
			loadError = (e as Error).message || 'failed to load';
		}
	}
	// --- export customization ---
	// Which features this activity's merged stream actually has (from the
	// options endpoint); null while loading or when there is nothing to export.
	const exportFeatures: [string, string][] = [
		['gps', 'GPS track'],
		['altitude', 'Altitude'],
		['distance', 'Distance'],
		['speed', 'Speed'],
		['heart_rate', 'Heart rate'],
		['power', 'Power'],
		['cadence', 'Cadence'],
		['temperature', 'Temperature'],
		['title', 'Title']
	];
	let exportAvail = $state<Record<string, boolean> | null>(null);
	let exportSel = $state<Record<string, boolean>>({});
	async function loadExportOptions() {
		try {
			const res = await fetch(`/api/activities/${a.id}/export/options`);
			if (!res.ok) return; // no stream to export — keep plain buttons
			const av = (await res.json()).available ?? {};
			exportAvail = av;
			const sel: Record<string, boolean> = {};
			for (const [key] of exportFeatures) if (av[key]) sel[key] = true;
			exportSel = sel;
		} catch {
			/* ignore — buttons fall back to full export */
		}
	}
	const exportExclude = $derived(
		exportFeatures.map(([key]) => key).filter((key) => exportAvail?.[key] && !exportSel[key])
	);
	const exportQuery = $derived(
		exportExclude.length > 0 ? `?exclude=${exportExclude.join(',')}` : ''
	);
	// GPX is position-only; without the GPS track the file would be empty.
	const gpxDisabled = $derived(exportAvail != null && (!exportAvail.gps || !exportSel.gps));

	onMount(() => {
		loadManage();
		loadShares();
		loadExportOptions();
	});

	function fmtBytes(n: number): string {
		if (!n) return '—';
		if (n < 1024) return `${n} B`;
		if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
		return `${(n / (1024 * 1024)).toFixed(1)} MB`;
	}

	async function runAction(path: string, label: string, confirmMsg?: string) {
		if (confirmMsg && !confirm(confirmMsg)) return;
		busy = label;
		actionMsg = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/${path}`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: '{}'
			});
			const body = await res.json().catch(() => ({}));
			if (!res.ok) throw new Error(body.error || res.statusText);
			let text = `${label} done.`;
			if (Array.isArray(body.warnings) && body.warnings.length > 0) {
				text = `${label} finished with ${body.warnings.length} warning(s): ${body.warnings.join('; ')}`;
			} else if (body.soft_deleted) {
				text = 'Activity had no remaining sources and was removed.';
			}
			actionMsg = { ok: true, text };
			await loadManage();
		} catch (e) {
			actionMsg = { ok: false, text: (e as Error).message };
		} finally {
			busy = null;
		}
	}

	// Cache-buster for the snapshot preview after a regenerate.
	let mapBust = $state(0);
	async function regenerateMap() {
		await runAction('map/regenerate', 'Regenerate map');
		mapBust = Date.now();
	}

	async function refetchSource(s: ManageSource) {
		if (
			!confirm(
				`Re-fetch this ${s.provider} source fresh from the provider? It re-downloads the activity (uses provider API budget) and re-merges. Your manual edits are preserved.`
			)
		)
			return;
		busy = `refetch-${s.id}`;
		actionMsg = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/sources/${s.id}/reimport`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: '{}'
			});
			const body = await res.json().catch(() => ({}));
			if (!res.ok) throw new Error(body.error || (await res.text?.()) || res.statusText);
			actionMsg = {
				ok: true,
				text: `Re-fetch queued — the source will update once the ${s.provider} worker processes it.`
			};
			await loadManage();
		} catch (e) {
			actionMsg = { ok: false, text: (e as Error).message };
		} finally {
			busy = null;
		}
	}

	async function reparseSource(s: ManageSource) {
		if (
			!confirm(
				`Re-parse this ${s.provider} source from its archived copy with the current importer? No provider data is downloaded; the activity re-merges from the freshly-parsed result.`
			)
		)
			return;
		busy = `reparse-${s.id}`;
		actionMsg = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/sources/${s.id}/reimport`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ mode: 'reparse' })
			});
			const body = await res.json().catch(() => ({}));
			if (!res.ok) throw new Error(body.error || res.statusText);
			actionMsg = {
				ok: true,
				text: 'Re-parse queued — the source will update from its archive shortly.'
			};
			await loadManage();
		} catch (e) {
			actionMsg = { ok: false, text: (e as Error).message };
		} finally {
			busy = null;
		}
	}

	async function detachSource(s: ManageSource) {
		if (
			!confirm(
				`Detach the ${s.provider} source from this activity? It stays archived but no longer contributes to the merged result. The activity re-merges from its remaining sources.`
			)
		)
			return;
		busy = `detach-${s.id}`;
		actionMsg = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/sources/${s.id}/detach`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ reason: 'user_detach' })
			});
			const body = await res.json().catch(() => ({}));
			if (!res.ok) throw new Error(body.error || res.statusText);
			actionMsg = {
				ok: true,
				text: body.soft_deleted
					? 'Last source detached — activity removed.'
					: 'Source detached and activity re-merged.'
			};
			await loadManage();
		} catch (e) {
			actionMsg = { ok: false, text: (e as Error).message };
		} finally {
			busy = null;
		}
	}

	async function splitSource(s: ManageSource) {
		if (
			!confirm(
				`Split the ${s.provider} source into its own activity? It's a different workout, not a duplicate — it'll be moved to a new activity and kept apart from this one on future re-imports.`
			)
		)
			return;
		busy = `split-${s.id}`;
		actionMsg = null;
		try {
			const res = await fetch(`/api/activities/${a.id}/split`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ source_id: s.id })
			});
			const body = await res.json().catch(() => ({}));
			if (!res.ok) throw new Error(body.error || res.statusText);
			actionMsg = { ok: true, text: 'Source split into its own activity.' };
			await loadManage();
		} catch (e) {
			actionMsg = { ok: false, text: (e as Error).message };
		} finally {
			busy = null;
		}
	}

	// Per-source "view parsed data" (the normalized payload Cairn stored).
	let parsedOpen = $state<Record<string, boolean>>({});
	let parsedJson = $state<Record<string, string>>({});
	async function toggleParsed(s: ManageSource) {
		if (parsedOpen[s.id]) {
			parsedOpen[s.id] = false;
			return;
		}
		if (!parsedJson[s.id]) {
			try {
				const res = await fetch(`/api/activities/${a.id}/sources/${s.id}/parsed`);
				parsedJson[s.id] = res.ok ? JSON.stringify(await res.json(), null, 2) : 'failed to load';
			} catch (e) {
				parsedJson[s.id] = (e as Error).message;
			}
		}
		parsedOpen[s.id] = true;
	}

	function statusColor(status: string): string {
		switch (status) {
			case 'active':
				return 'bg-emerald-500/15 text-emerald-300';
			case 'detached':
				return 'bg-zinc-600/30 text-zinc-400';
			default:
				return 'bg-amber-500/15 text-amber-300';
		}
	}
</script>

<section class="space-y-8">
	<header>
		<ActivityNav id={a.id} title={a.title} />
		<h1 class="mt-2 text-2xl font-semibold tracking-tight">Manage &amp; insights</h1>
		<p class="mt-1 text-sm text-zinc-400">
			Technical provenance and maintenance actions for this activity.
		</p>
	</header>

	{#if loadError}
		<div class="rounded-lg border border-red-900/50 bg-red-950/30 p-4 text-sm text-red-300">
			{loadError}
		</div>
	{/if}

	<!-- Export -->
	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
		<h2 class="mb-1 text-sm font-medium text-zinc-300">Export</h2>
		<p class="mb-4 text-xs text-zinc-500">
			Download the merged activity as a standard file, generated from Cairn's own data (works
			regardless of where it was imported from). GPX needs a GPS track; TCX also carries indoor
			HR/power.
		</p>
		{#if exportAvail != null}
			<fieldset class="mb-4 flex flex-wrap gap-x-4 gap-y-2">
				<legend class="mb-2 text-xs text-zinc-500">
					Include in the file (deselected features are left out):
				</legend>
				{#each exportFeatures as [key, label] (key)}
					{#if exportAvail[key]}
						<label class="flex items-center gap-2 text-xs text-zinc-300">
							<input type="checkbox" bind:checked={exportSel[key]} />
							{label}
						</label>
					{/if}
				{/each}
			</fieldset>
		{/if}
		<div class="flex flex-wrap gap-3">
			{#if gpxDisabled}
				<span
					class="cursor-not-allowed rounded border border-zinc-800 px-3 py-1.5 text-xs text-zinc-600"
					title="GPX needs the GPS track"
				>
					Download GPX
				</span>
			{:else}
				<a
					href={`/api/activities/${a.id}/export.gpx${exportQuery}`}
					class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
				>
					Download GPX
				</a>
			{/if}
			<a
				href={`/api/activities/${a.id}/export.tcx${exportQuery}`}
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
			>
				Download TCX
			</a>
			<a
				href={`/api/activities/${a.id}/export.fit${exportQuery}`}
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300"
			>
				Download FIT
			</a>
		</div>
	</section>

	<!-- Share links -->
	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
		<h2 class="mb-1 text-sm font-medium text-zinc-300">Share</h2>
		<p class="mb-4 text-xs text-zinc-500">
			Create an unguessable read-only link. Anyone with the link sees this activity projected
			through your "link" visibility rules — no Cairn account needed. Revoke any time.
		</p>
		<div class="mb-3">
			<button
				type="button"
				disabled={shareBusy}
				onclick={createShare}
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				{shareBusy ? 'Creating…' : 'Create share link'}
			</button>
		</div>
		<ul class="space-y-2">
			{#each shareLinks as l (l.token)}
				<li class="flex items-center gap-2 rounded border border-zinc-800 px-3 py-2 text-xs">
					<input
						readonly
						value={shareURL(l.path)}
						class="flex-1 bg-transparent text-zinc-300 outline-none max-md:min-w-0"
						onclick={(e) => (e.currentTarget as HTMLInputElement).select()}
					/>
					<button
						type="button"
						onclick={() => navigator.clipboard?.writeText(shareURL(l.path))}
						class="text-zinc-400 hover:text-accent-300">Copy</button
					>
					<button
						type="button"
						onclick={() => revokeShare(l.token)}
						class="text-zinc-500 hover:text-red-400">Revoke</button
					>
				</li>
			{/each}
			{#if shareLinks.length === 0}
				<li class="text-xs text-zinc-600">No active share links.</li>
			{/if}
		</ul>
	</section>

	<!-- Maintenance actions -->
	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
		<h2 class="mb-1 text-sm font-medium text-zinc-300">Maintenance</h2>
		<p class="mb-4 text-xs text-zinc-500">
			These re-run Cairn's own computations from the data already stored — they don't contact any
			external provider.
		</p>
		<div class="flex flex-wrap gap-3">
			<button
				type="button"
				disabled={!!busy}
				onclick={() =>
					runAction('recompute', 'Re-merge', 'Re-merge this activity from its current sources?')}
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				{busy === 'Re-merge' ? 'Re-merging…' : 'Re-merge from sources'}
			</button>
			<button
				type="button"
				disabled={!!busy}
				onclick={() => runAction('recompute-derived', 'Recompute derived')}
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				{busy === 'Recompute derived'
					? 'Recomputing…'
					: 'Recompute best-efforts, segments & training load'}
			</button>
			<button
				type="button"
				disabled={!!busy}
				onclick={regenerateMap}
				title="Re-render the static map snapshot used in feeds and shares"
				class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
			>
				{busy === 'Regenerate map' ? 'Regenerating…' : 'Regenerate map snapshot'}
			</button>
		</div>
		<img
			src={`/api/activities/${a.id}/map.png${mapBust ? `?v=${mapBust}` : ''}`}
			alt=""
			class="mt-3 h-20 rounded border border-zinc-800 object-cover"
			onerror={(e) => ((e.currentTarget as HTMLImageElement).style.display = 'none')}
		/>
		{#if actionMsg}
			<p class="mt-3 text-xs {actionMsg.ok ? 'text-emerald-400' : 'text-red-400'}">
				{actionMsg.text}
			</p>
		{/if}
	</section>

	<!-- Per-field provenance: which source won each field, and whether by rule or a manual pin -->
	{#if payload && payload.provenance && payload.provenance.length > 0}
		<section>
			<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">
				Field provenance
			</h2>
			<ul class="flex flex-wrap gap-2">
				{#each payload.provenance as p (p.field)}
					<li
						class="rounded border border-zinc-800 bg-zinc-900/40 px-2 py-1 text-xs"
						title={`${p.field_label} from ${p.provider || 'unknown'} (${p.decided_by})`}
					>
						<span class="text-zinc-400">{p.field_label}</span>
						<span class="ml-1 text-zinc-200">{p.provider || '—'}</span>
						{#if p.decided_by === 'manual'}
							<span class="ml-1 rounded bg-amber-500/15 px-1 text-amber-300">pinned</span>
						{/if}
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	<!-- Per-source provenance -->
	<section>
		<h2 class="mb-3 text-sm font-medium uppercase tracking-wide text-zinc-400">
			Sources {#if payload}<span class="text-zinc-500">({payload.sources.length})</span>{/if}
		</h2>
		{#if payload && payload.sources.length > 0}
			<ul class="space-y-4">
				{#each payload.sources as s (s.id)}
					<li class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
						<div class="flex flex-wrap items-center justify-between gap-2">
							<div class="flex items-center gap-2">
								<span class="font-medium text-zinc-200">{s.provider}</span>
								<span class="rounded px-1.5 py-0.5 text-[10px] uppercase {statusColor(s.status)}">
									{s.status}
								</span>
								{#if s.is_primary}
									<span
										class="rounded bg-accent-500/15 px-1.5 py-0.5 text-[10px] uppercase text-accent-300"
										>primary stream</span
									>
								{/if}
								{#if s.reparse_eligible}
									<span
										class="rounded bg-sky-500/15 px-1.5 py-0.5 text-[10px] uppercase text-sky-300"
										title={`A worker with parser ${s.reparse_with} (dataset v${s.dataset_version}) is online — this source can be re-parsed from its archive`}
										>re-parsable</span
									>
								{/if}
								{#if s.upgrade === 'new_parser'}
									<span
										class="rounded bg-sky-500/15 px-1.5 py-0.5 text-[10px] uppercase text-sky-300"
										title={`Parsed by ${s.parser_version}; a newer parser is online. Re-parse from the archive to pick up its changes (no provider quota).`}
										>newer parser</span
									>
								{:else if s.upgrade === 'new_dataset'}
									<span
										class="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] uppercase text-amber-300"
										title={`Imported as dataset v${s.dataset_version}; the online worker uses a newer archive format and cannot read it. Re-fetch from the provider.`}
										>new dataset</span
									>
								{/if}
								{#if s.reimport_status && s.reimport_status !== 'current'}
									<span
										class="rounded bg-amber-500/15 px-1.5 py-0.5 text-[10px] uppercase text-amber-300"
										>{s.reimport_status}</span
									>
								{/if}
							</div>
							<div class="flex flex-wrap items-center justify-end gap-3">
								{#if s.has_blob}
									<a
										href={`/api/activities/${a.id}/sources/${s.id}/download`}
										class="text-xs text-accent-400 hover:text-accent-300"
									>
										Download original ↓
									</a>
								{/if}
								{#if s.status !== 'detached' && s.external_account}
									<button
										type="button"
										disabled={!!busy || s.reimport_status === 'updating'}
										onclick={() => refetchSource(s)}
										class="text-xs text-accent-400 hover:text-accent-300 disabled:opacity-50"
									>
										{busy === `refetch-${s.id}`
											? 'Queuing…'
											: s.reimport_status === 'updating'
												? 'Re-fetching…'
												: 'Re-fetch from provider'}
									</button>
								{/if}
								{#if s.status !== 'detached' && s.reparse_eligible}
									<button
										type="button"
										disabled={!!busy || s.reimport_status === 'updating'}
										onclick={() => reparseSource(s)}
										class="text-xs text-accent-400 hover:text-accent-300 disabled:opacity-50"
									>
										{busy === `reparse-${s.id}`
											? 'Queuing…'
											: s.reparse_with && s.reparse_with !== s.parser_version
												? `Re-parse with ${s.reparse_with}`
												: 'Re-parse from archive'}
									</button>
								{:else if s.status !== 'detached'}
									<span
										class="inline-flex items-center gap-1 text-xs text-zinc-600"
										title={s.has_blob
											? `Re-parse needs a ${s.provider} worker online that reads dataset v${s.dataset_version || '?'} with parser ${s.parser_version || '?'} or newer.`
											: 'No archived original to re-parse — re-fetch from the provider to create one.'}
									>
										<span class="cursor-not-allowed line-through decoration-zinc-700"
											>Re-parse from archive</span
										>
										<span class="text-[10px] not-italic text-zinc-500">
											{s.has_blob
												? `· needs ${s.provider} parser ≥ ${s.parser_version || s.worker_version} online`
												: '· no archived original yet'}
										</span>
									</span>
								{/if}
								{#if s.status !== 'detached'}
									{#if payload && payload.sources.filter((x) => x.status !== 'detached').length > 1}
										<button
											type="button"
											disabled={!!busy}
											onclick={() => splitSource(s)}
											class="text-xs text-amber-400 hover:text-amber-300 disabled:opacity-50"
											title="Move this source to its own activity (it's a different workout, not a duplicate)"
										>
											{busy === `split-${s.id}` ? 'Splitting…' : 'Split off'}
										</button>
									{/if}
									<button
										type="button"
										disabled={!!busy}
										onclick={() => detachSource(s)}
										class="text-xs text-red-400 hover:text-red-300 disabled:opacity-50"
									>
										{busy === `detach-${s.id}` ? 'Detaching…' : 'Detach'}
									</button>
								{/if}
							</div>
						</div>

						<dl
							class="mt-3 grid grid-cols-1 gap-x-6 gap-y-1.5 text-xs sm:grid-cols-2 lg:grid-cols-3"
						>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">External ID</dt>
								<dd class="truncate font-mono text-zinc-300">{s.external_id || '—'}</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">Imported</dt>
								<dd class="text-zinc-300">{formatDate(s.imported_at)}</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">Last parsed</dt>
								<dd class="text-zinc-300">
									{s.last_reimported_at ? formatDate(s.last_reimported_at) : '—'}
								</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500" title="Archive format the source was imported as">
									Dataset
								</dt>
								<dd class="text-zinc-300">{s.dataset_version ? `v${s.dataset_version}` : '—'}</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500" title="Parser that produced the current data">Parser</dt>
								<dd class="text-zinc-300">
									{s.parser_version || '—'}
									<span class="text-zinc-500"
										>{s.worker_name || ''}{s.worker_release ? ` ${s.worker_release}` : ''}</span
									>
								</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">Package</dt>
								<dd class="font-mono text-xs text-zinc-300" title={s.worker_package}>
									{s.worker_package || '—'}
								</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">Archived original</dt>
								<dd class="text-zinc-300">
									{s.has_blob
										? `${fmtBytes(s.raw_size_bytes)} (${s.raw_content_type || '?'})`
										: 'none'}
								</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">Stream</dt>
								<dd class="text-zinc-300">{s.has_stream ? 'yes' : 'no'}</dd>
							</div>
							<div class="flex justify-between gap-2">
								<dt class="text-zinc-500">Laps</dt>
								<dd class="text-zinc-300">{s.lap_count || '—'}</dd>
							</div>
							{#if s.status_reason}
								<div class="flex justify-between gap-2">
									<dt class="text-zinc-500">Status reason</dt>
									<dd class="text-zinc-400">{s.status_reason}</dd>
								</div>
							{/if}
						</dl>

						{#if s.won_field_groups && s.won_field_groups.length > 0}
							<div class="mt-3 border-t border-zinc-800 pt-3">
								<p class="mb-1.5 text-[10px] uppercase tracking-wide text-zinc-500">
									Won these merged fields
								</p>
								<div class="flex flex-wrap gap-1.5">
									{#each s.won_field_groups as g (g)}
										<span class="rounded bg-zinc-800 px-2 py-0.5 text-[11px] text-zinc-300"
											>{g}</span
										>
									{/each}
								</div>
							</div>
						{/if}

						<div class="mt-3 border-t border-zinc-800 pt-3">
							<button
								type="button"
								onclick={() => toggleParsed(s)}
								class="text-xs text-accent-400 hover:text-accent-300"
							>
								{parsedOpen[s.id] ? 'Hide' : 'View'} parsed data
							</button>
							{#if parsedOpen[s.id]}
								<pre
									class="mt-2 max-h-80 overflow-auto rounded bg-zinc-950 p-2 text-[11px] leading-relaxed text-zinc-300">{parsedJson[
										s.id
									]}</pre>
							{/if}
						</div>
					</li>
				{/each}
			</ul>
		{:else if payload}
			<p class="text-sm text-zinc-500">No sources.</p>
		{:else if !loadError}
			<p class="text-sm text-zinc-500">Loading…</p>
		{/if}
	</section>
</section>
