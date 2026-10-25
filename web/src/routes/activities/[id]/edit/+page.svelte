<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { clients } from '#lib/connect.js';
	import { ActivityType, Discipline, Privacy } from '#lib/proto/cairn/v1/activity_pb.js';
	import { enumName, enumValue, protoTimestampToISO } from '#lib/proto-helpers.js';
	import { ACTIVITY_TYPES, DISCIPLINES, typeLabel } from '#lib/activity-types.js';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import GearPicker from '#lib/components/GearPicker.svelte';
	import ActivityNav from '#lib/components/ActivityNav.svelte';
	import { m } from '#lib/paraglide/messages.js';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	// Frozen baseline of the loaded values: the form seeds its editable state
	// from this and diffs against it to build the field_mask. `data` is loaded
	// once per navigation, so capturing the initial value here is intentional.
	// svelte-ignore state_referenced_locally
	const init = data.edit;

	let title = $state(init.title);
	let description = $state(init.description);
	let type = $state(init.type);
	let discipline = $state(init.discipline);
	let isVirtual = $state(init.isVirtual);
	let isEbike = $state(init.isEbike);
	let isCommute = $state(init.isCommute);
	let isRace = $state(init.isRace);
	let customSubtype = $state(init.customSubtype);
	let tagsText = $state(init.tags.join(', '));
	let privacy = $state(init.privacy);
	// Gear: picking (incl. "no gear") makes it the user's choice; "reset to
	// automatic" hands it back to the merge.
	let gearId = $state(init.gearId);
	let resetGear = $state(false);

	// Summary-metric corrections (overlay). Shown in friendly units; blank = no
	// override (revert to merged). Baselines are strings so change-detection is
	// exact.
	const initDistKm =
		init.distanceM != null ? String(Math.round((init.distanceM / 1000) * 100) / 100) : '';
	const initElevM = init.elevationGainM != null ? String(Math.round(init.elevationGainM)) : '';
	const initMovMin = init.movingDurationS
		? String(Math.round((init.movingDurationS / 60) * 10) / 10)
		: '';
	let distanceKm = $state(initDistKm);
	let elevationGainM = $state(initElevM);
	let movingMin = $state(initMovMin);

	let saving = $state(false);
	let errMsg = $state<string | null>(null);

	const disciplineOptions = $derived(DISCIPLINES[type] ?? []);

	// Drop a discipline that no longer belongs to the chosen sport.
	function onTypeChange() {
		if (discipline && !disciplineOptions.includes(discipline)) discipline = '';
	}

	async function save() {
		saving = true;
		errMsg = null;
		const tags = tagsText
			.split(',')
			.map((t) => t.trim())
			.filter(Boolean);

		const mask: string[] = [];
		if (title !== init.title) mask.push('title');
		if (description !== init.description) mask.push('description');
		if (type !== init.type) mask.push('type');
		if (discipline !== init.discipline) mask.push('discipline');
		if (isVirtual !== init.isVirtual) mask.push('is_virtual');
		if (isEbike !== init.isEbike) mask.push('is_ebike');
		if (isCommute !== init.isCommute) mask.push('is_commute');
		if (isRace !== init.isRace) mask.push('is_race');
		if (customSubtype !== init.customSubtype) mask.push('custom_subtype');
		if (tags.join('\0') !== init.tags.join('\0')) mask.push('tags');
		if (privacy !== init.privacy) mask.push('privacy');
		if (resetGear) mask.push('reset_gear');
		else if (gearId !== init.gearId) mask.push('gear_id');

		// Summary corrections: blank → -1 (clears the override), else the value.
		let distanceM = -1;
		let elevationGain = -1;
		let movingDurationS = -1n;
		if (distanceKm.trim() !== initDistKm) {
			mask.push('distance_m');
			distanceM = distanceKm.trim() === '' ? -1 : Number(distanceKm) * 1000;
		}
		if (elevationGainM.trim() !== initElevM) {
			mask.push('elevation_gain_m');
			elevationGain = elevationGainM.trim() === '' ? -1 : Number(elevationGainM);
		}
		if (movingMin.trim() !== initMovMin) {
			mask.push('moving_duration_s');
			movingDurationS = movingMin.trim() === '' ? -1n : BigInt(Math.round(Number(movingMin) * 60));
		}

		if (mask.length === 0) {
			goto(`/activities/${init.id}`);
			return;
		}

		try {
			await clients.activity.updateActivity({
				id: init.id,
				fieldMask: mask,
				title,
				description,
				type: enumValue(ActivityType, type),
				discipline: enumValue(Discipline, discipline),
				isVirtual,
				isEbike,
				isCommute,
				isRace,
				customSubtype,
				tags,
				privacy: enumValue(Privacy, privacy),
				gearId,
				distanceM,
				elevationGainM: elevationGain,
				movingDurationS
			});
			goto(`/activities/${init.id}`);
		} catch (e) {
			errMsg = (e as Error).message;
			saving = false;
		}
	}

	// ---- source data from another activity ----
	type Donor = { id: string; label: string };
	const SOURCE_GROUPS = [
		{ key: 'gps_track', label: 'Route & map' },
		{ key: 'distance', label: 'Distance' },
		{ key: 'elevation', label: 'Elevation' },
		{ key: 'speed', label: 'Speed' },
		{ key: 'heart_rate', label: 'Heart rate' },
		{ key: 'power', label: 'Power' },
		{ key: 'cadence', label: 'Cadence' },
		{ key: 'temperature', label: 'Temperature' },
		{ key: 'calories', label: 'Calories' }
	];

	let donors = $state<Donor[]>([]);
	let donorId = $state('');
	let picked = $state<Record<string, boolean>>({
		gps_track: true,
		distance: true,
		elevation: true
	});
	let sourcing = $state(false);
	let sourceMsg = $state<string | null>(null);

	onMount(async () => {
		try {
			const res = await clients.activity.listActivities({});
			donors = res.activities
				.filter((a) => a.id !== init.id)
				.map((a) => {
					const when = a.startTime
						? new Date(protoTimestampToISO(a.startTime)).toLocaleDateString()
						: '';
					const name = a.title || enumName(ActivityType, a.type) || 'activity';
					return { id: a.id, label: `${name} — ${when}` };
				});
		} catch {
			/* non-fatal: the sourcing section just stays empty */
		}
	});

	async function sourceFrom() {
		sourceMsg = null;
		if (!donorId) {
			sourceMsg = 'Pick an activity to source from.';
			return;
		}
		const groups = SOURCE_GROUPS.map((g) => g.key).filter((k) => picked[k]);
		if (groups.length === 0) {
			sourceMsg = 'Pick at least one field to source.';
			return;
		}
		sourcing = true;
		try {
			const res = await fetch(`/api/activities/${init.id}/source-from`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ donor_id: donorId, groups })
			});
			if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
			goto(`/activities/${init.id}`);
		} catch (e) {
			sourceMsg = (e as Error).message;
			sourcing = false;
		}
	}
</script>

<section class="space-y-8">
	<header>
		<ActivityNav id={init.id} title={init.title} />
		<h1 class="mt-2 flex items-center gap-2 text-2xl font-semibold tracking-tight">
			<SportIcon {type} size={24} /> Edit activity
		</h1>
		<p class="mt-1 text-sm text-zinc-400">
			Classification edits (sport, flags) are saved as your overlay — they stick even when a source
			re-imports.
		</p>
	</header>

	<form
		class="max-w-2xl space-y-5"
		onsubmit={(e) => {
			e.preventDefault();
			save();
		}}
	>
		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="title">Title</label>
			<input
				id="title"
				bind:value={title}
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			/>
		</div>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="desc"
				>Description</label
			>
			<textarea
				id="desc"
				bind:value={description}
				rows="3"
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			></textarea>
		</div>

		<div class="grid grid-cols-2 gap-4 max-md:grid-cols-1">
			<div>
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="type">Sport</label>
				<select
					id="type"
					bind:value={type}
					onchange={onTypeChange}
					class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm capitalize"
				>
					{#each ACTIVITY_TYPES as t (t)}
						<option value={t}>{t}</option>
					{/each}
				</select>
			</div>
			<div>
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="disc"
					>Discipline</label
				>
				<select
					id="disc"
					bind:value={discipline}
					disabled={disciplineOptions.length === 0}
					class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm capitalize disabled:opacity-50"
				>
					<option value="">— none —</option>
					{#each disciplineOptions as d (d)}
						<option value={d}>{typeLabel(d)}</option>
					{/each}
				</select>
			</div>
		</div>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="subtype"
				>Custom subtype</label
			>
			<input
				id="subtype"
				bind:value={customSubtype}
				placeholder="e.g. Brick, Recovery"
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			/>
		</div>

		<fieldset class="flex flex-wrap gap-4">
			<label class="flex items-center gap-2 text-sm"
				><input type="checkbox" bind:checked={isRace} /> Race</label
			>
			<label class="flex items-center gap-2 text-sm"
				><input type="checkbox" bind:checked={isVirtual} /> Virtual</label
			>
			<label class="flex items-center gap-2 text-sm"
				><input type="checkbox" bind:checked={isCommute} /> Commute</label
			>
			<label class="flex items-center gap-2 text-sm"
				><input type="checkbox" bind:checked={isEbike} /> E-bike</label
			>
		</fieldset>

		<!-- Summary corrections (overlay): hand-fix merged numbers; blank = use the
		     merged value. Survives re-import like the classification edits. -->
		<fieldset class="rounded-lg border border-zinc-800 p-3">
			<legend class="px-1 text-xs uppercase tracking-wide text-zinc-500">Summary corrections</legend
			>
			<div class="grid grid-cols-3 gap-3 max-md:grid-cols-1">
				<div>
					<label class="block text-[11px] text-zinc-500" for="dist">Distance (km)</label>
					<input
						id="dist"
						type="number"
						min="0"
						step="0.01"
						bind:value={distanceKm}
						class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
					/>
				</div>
				<div>
					<label class="block text-[11px] text-zinc-500" for="elev">Elevation gain (m)</label>
					<input
						id="elev"
						type="number"
						min="0"
						bind:value={elevationGainM}
						class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
					/>
				</div>
				<div>
					<label class="block text-[11px] text-zinc-500" for="mov">Moving time (min)</label>
					<input
						id="mov"
						type="number"
						min="0"
						step="0.1"
						bind:value={movingMin}
						class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
					/>
				</div>
			</div>
			<p class="mt-1.5 text-[11px] text-zinc-600">
				Leave blank to use the value merged from your sources. Editing distance or moving time
				re-derives average pace/speed.
			</p>
		</fieldset>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="tags">Tags</label>
			<input
				id="tags"
				bind:value={tagsText}
				placeholder="comma, separated"
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			/>
		</div>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="gear"
				>{m.gear_title()}
				<span class="normal-case tracking-normal text-zinc-600"
					>· {init.gearSetBy === 'user' ? m.gear_set_by_user() : m.gear_set_by_merge()}</span
				></label
			>
			<GearPicker gear={data.gear} {type} bind:value={gearId} disabled={resetGear} />
			{#if init.gearSetBy === 'user'}
				<label class="mt-1.5 flex items-center gap-2 text-xs text-zinc-400"
					><input type="checkbox" bind:checked={resetGear} /> {m.gear_reset_auto()}</label
				>
			{/if}
		</div>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="privacy"
				>Privacy</label
			>
			<select
				id="privacy"
				bind:value={privacy}
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm capitalize"
			>
				<option value="private">Private</option>
				<option value="followers">Followers</option>
				<option value="public">Public</option>
			</select>
		</div>

		{#if errMsg}
			<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
				{errMsg}
			</div>
		{/if}

		<div class="flex gap-2">
			<button
				type="submit"
				disabled={saving}
				class="rounded bg-accent-600 px-4 py-2 text-sm font-medium text-white hover:bg-accent-500 disabled:opacity-50"
			>
				{saving ? 'Saving…' : 'Save'}
			</button>
			<a
				href={`/activities/${init.id}`}
				class="rounded border border-zinc-700 px-4 py-2 text-sm hover:bg-zinc-800">Cancel</a
			>
		</div>
	</form>

	<!-- Source data from another activity (e.g. borrow a route you rode without
	     your watch). Creates a derived source pinned to the chosen fields. -->
	<section class="max-w-2xl rounded-xl border border-zinc-800 bg-zinc-900/40 p-5">
		<h2 class="text-lg font-semibold">Source data from another activity</h2>
		<p class="mt-1 text-sm text-zinc-500">
			Borrow the route or metrics from one of your other activities. The route is rebased to this
			activity's start time and marked as a derived source — undo it later by detaching it on the
			Manage page.
		</p>

		<div class="mt-4 space-y-4">
			<div>
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="donor"
					>Donor activity</label
				>
				<select
					id="donor"
					bind:value={donorId}
					class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
				>
					<option value="">— choose an activity —</option>
					{#each donors as d (d.id)}
						<option value={d.id}>{d.label}</option>
					{/each}
				</select>
			</div>

			<div class="flex flex-wrap gap-x-4 gap-y-2">
				{#each SOURCE_GROUPS as g (g.key)}
					<label class="flex items-center gap-2 text-sm"
						><input type="checkbox" bind:checked={picked[g.key]} /> {g.label}</label
					>
				{/each}
			</div>

			{#if sourceMsg}
				<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300">
					{sourceMsg}
				</div>
			{/if}

			<button
				type="button"
				onclick={sourceFrom}
				disabled={sourcing}
				class="rounded border border-accent-500 bg-accent-500/20 px-4 py-2 text-sm text-accent-300 hover:bg-accent-500/30 disabled:opacity-50"
			>
				{sourcing ? 'Sourcing…' : 'Source selected data'}
			</button>
		</div>
	</section>
</section>
