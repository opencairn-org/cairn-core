<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { clients } from '#lib/connect.js';
	import { ACTIVITY_TYPES, DISCIPLINES, typeLabel } from '#lib/activity-types.js';
	import { gearToView, type GearView } from '#lib/gear.js';
	import SportIcon from '#lib/components/SportIcon.svelte';
	import GearPicker from '#lib/components/GearPicker.svelte';
	import { m } from '#lib/paraglide/messages.js';

	// Manual activity entry — posts to the REST endpoint, which feeds the same
	// ingest → merge pipeline as imports (provider = "manual").

	let type = $state('ride');
	let discipline = $state('');
	let title = $state('');
	let description = $state('');
	let startLocal = $state(''); // datetime-local value
	let durationMin = $state<number | ''>('');
	let movingMin = $state<number | ''>('');
	let distanceKm = $state<number | ''>('');
	let elevationGainM = $state<number | ''>('');
	let isVirtual = $state(false);
	let isEbike = $state(false);
	let isCommute = $state(false);
	let isRace = $state(false);
	let customSubtype = $state('');
	// Gear: blank keeps the sport's default (applied by the server); a pick is
	// the user's choice.
	let gearId = $state('');
	let gear = $state<GearView[]>([]);
	onMount(async () => {
		try {
			gear = (await clients.gear.listGear({})).gear.map(gearToView);
		} catch {
			/* no gear picker without the list */
		}
	});

	let saving = $state(false);
	let errMsg = $state<string | null>(null);

	const disciplineOptions = $derived(DISCIPLINES[type] ?? []);
	function onTypeChange() {
		if (discipline && !disciplineOptions.includes(discipline)) discipline = '';
	}

	const browserTz = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';

	async function create() {
		errMsg = null;
		if (!startLocal) {
			errMsg = 'Pick a start date & time.';
			return;
		}
		if (!durationMin || durationMin <= 0) {
			errMsg = 'Enter a duration in minutes.';
			return;
		}
		saving = true;
		const body = {
			type,
			discipline,
			title: title.trim(),
			description: description.trim(),
			start_time: new Date(startLocal).toISOString(),
			timezone: browserTz,
			elapsed_duration_s: Number(durationMin) * 60,
			moving_duration_s: movingMin ? Number(movingMin) * 60 : 0,
			distance_m: distanceKm ? Math.round(Number(distanceKm) * 1000) : null,
			elevation_gain_m: elevationGainM ? Number(elevationGainM) : null,
			is_virtual: isVirtual,
			is_ebike: isEbike,
			is_commute: isCommute,
			is_race: isRace,
			custom_subtype: customSubtype.trim(),
			gear_id: gearId
		};
		try {
			const res = await fetch('/api/activities/manual', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body)
			});
			if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
			const out = await res.json();
			goto(`/activities/${out.activity_id}`);
		} catch (e) {
			errMsg = (e as Error).message;
			saving = false;
		}
	}
</script>

<div class="mx-auto max-w-2xl px-4 py-6">
	<a href="/activities" class="text-sm text-zinc-400 hover:text-zinc-200">← Activities</a>
	<h1 class="mt-2 flex items-center gap-2 text-2xl font-semibold">
		<SportIcon {type} size={24} /> New activity
	</h1>
	<p class="mt-1 text-sm text-zinc-500">
		Log an activity by hand — no file or device needed. It becomes a normal activity you can edit,
		share, and add data to later.
	</p>

	<form
		class="mt-6 space-y-5"
		onsubmit={(e) => {
			e.preventDefault();
			create();
		}}
	>
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
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="disc">
					Discipline
				</label>
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

		{#if gear.length > 0}
			<div>
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="gear">
					{m.gear_title()}
				</label>
				<GearPicker {gear} {type} bind:value={gearId} />
			</div>
		{/if}

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="title">Title</label>
			<input
				id="title"
				bind:value={title}
				placeholder="Morning Ride"
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			/>
		</div>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="desc">
				Description
			</label>
			<textarea
				id="desc"
				bind:value={description}
				rows="2"
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			></textarea>
		</div>

		<div class="grid grid-cols-2 gap-4 max-md:grid-cols-1">
			<div>
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="start">
					Start ({browserTz})
				</label>
				<input
					id="start"
					type="datetime-local"
					bind:value={startLocal}
					class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
				/>
			</div>
			<div class="grid grid-cols-2 gap-2">
				<div>
					<label class="block text-xs uppercase tracking-wide text-zinc-500" for="dur">
						Duration (min)
					</label>
					<input
						id="dur"
						type="number"
						min="1"
						bind:value={durationMin}
						class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
					/>
				</div>
				<div>
					<label class="block text-xs uppercase tracking-wide text-zinc-500" for="mov">
						Moving (min)
					</label>
					<input
						id="mov"
						type="number"
						min="0"
						bind:value={movingMin}
						placeholder="opt"
						class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
					/>
				</div>
			</div>
		</div>

		<div class="grid grid-cols-2 gap-4 max-md:grid-cols-1">
			<div>
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="dist">
					Distance (km)
				</label>
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
				<label class="block text-xs uppercase tracking-wide text-zinc-500" for="elev">
					Elevation gain (m)
				</label>
				<input
					id="elev"
					type="number"
					min="0"
					bind:value={elevationGainM}
					class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
				/>
			</div>
		</div>

		<div>
			<label class="block text-xs uppercase tracking-wide text-zinc-500" for="subtype">
				Custom subtype
			</label>
			<input
				id="subtype"
				bind:value={customSubtype}
				placeholder="e.g. Strength, Yoga"
				class="mt-1 w-full rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 text-sm"
			/>
		</div>

		<fieldset class="flex flex-wrap gap-4">
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={isRace} /> Race
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={isVirtual} /> Virtual
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={isCommute} /> Commute
			</label>
			<label class="flex items-center gap-2 text-sm">
				<input type="checkbox" bind:checked={isEbike} /> E-bike
			</label>
		</fieldset>

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
				{saving ? 'Creating…' : 'Create activity'}
			</button>
			<a
				href="/activities"
				class="rounded border border-zinc-700 px-4 py-2 text-sm hover:bg-zinc-800"
			>
				Cancel
			</a>
		</div>
	</form>
</div>
