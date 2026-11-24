<script lang="ts">
	import { onMount } from 'svelte';
	import MapPicker from '#lib/components/MapPicker.svelte';

	// Privacy & sharing controls: public-profile opt-in, the per-audience
	// visibility policy, and privacy zones. Backs onto /api/profile/*.

	let isPublic = $state(false);
	let username = $state('');
	let { username: usernameProp = '' }: { username?: string } = $props();

	type Zone = { id: string; label: string; lat: number; lng: number; radius_m: number };
	let zones = $state<Zone[]>([]);
	let newZone = $state({ label: '', coords: '', radius_m: '200' });
	let zoneError = $state<string | null>(null);
	let showPicker = $state(false);

	let audiences = $state<string[]>([]);
	let categories = $state<string[]>([]);
	let policy = $state<Record<string, string[]>>({});
	let saving = $state(false);
	let saved = $state(false);
	let loadError = $state<string | null>(null);
	// Saving is blocked until the policy loaded, otherwise a failed fetch would
	// be saved back as an empty policy.
	let policyLoaded = $state(false);

	onMount(async () => {
		username = usernameProp;
		// public toggle is derived from the profile endpoint
		try {
			const p = await fetch(`/api/profiles/${username}`);
			if (!p.ok) throw new Error(p.statusText);
			const b = await p.json();
			isPublic = b.is_public;
		} catch {
			loadError = 'Could not load your profile settings — reload to retry.';
		}
		try {
			const v = await fetch('/api/profile/visibility');
			if (!v.ok) throw new Error(v.statusText);
			const b = await v.json();
			audiences = b.all_audiences ?? ['public', 'followers', 'link'];
			categories = b.all_categories ?? [];
			policy = b.policy ?? {};
			policyLoaded = true;
		} catch {
			loadError = 'Could not load your visibility policy — reload to retry.';
		}
		try {
			const z = await fetch('/api/profile/privacy-zones');
			if (!z.ok) throw new Error(z.statusText);
			const b = await z.json();
			zones = b.zones ?? [];
		} catch {
			loadError = 'Could not load your privacy zones — reload to retry.';
		}
	});

	async function togglePublic() {
		const next = !isPublic;
		const res = await fetch('/api/profile/public', {
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ public: next })
		});
		if (res.ok) isPublic = next;
	}

	function has(aud: string, cat: string): boolean {
		return (policy[aud] ?? []).includes(cat);
	}
	function toggleCat(aud: string, cat: string) {
		// eslint-disable-next-line svelte/prefer-svelte-reactivity -- local scratch set, copied into state
		const cur = new Set(policy[aud] ?? []);
		if (cur.has(cat)) cur.delete(cat);
		else cur.add(cat);
		policy = { ...policy, [aud]: [...cur] };
	}
	async function savePolicy() {
		saving = true;
		saved = false;
		try {
			const res = await fetch('/api/profile/visibility', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ policy })
			});
			if (res.ok) {
				saved = true;
				setTimeout(() => (saved = false), 2000);
			}
		} finally {
			saving = false;
		}
	}

	// Accepts "50.1000, 10.2000" (e.g. pasted from a maps right-click) or
	// "50.1000 10.2000". Returns null on anything unparseable.
	function parseCoords(s: string): { lat: number; lng: number } | null {
		const parts = s
			.trim()
			.split(/[,\s]+/)
			.filter(Boolean);
		if (parts.length !== 2) return null;
		const [latS = '', lngS = ''] = parts;
		const lat = parseFloat(latS);
		const lng = parseFloat(lngS);
		if (isNaN(lat) || isNaN(lng) || lat < -90 || lat > 90 || lng < -180 || lng > 180) return null;
		return { lat, lng };
	}

	async function addZone() {
		zoneError = null;
		const coords = parseCoords(newZone.coords);
		if (!coords) {
			zoneError = 'Enter coordinates as "lat, lng" — e.g. 50.1000, 10.2000.';
			return;
		}
		const radius = parseFloat(newZone.radius_m) || 200;
		const body = { label: newZone.label, lat: coords.lat, lng: coords.lng, radius_m: radius };
		const res = await fetch('/api/profile/privacy-zones', {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(body)
		});
		if (res.ok) {
			const b = await res.json();
			zones = [...zones, { id: b.id, ...body }];
			newZone = { label: '', coords: '', radius_m: '200' };
		} else {
			zoneError = 'Could not save the zone — please try again.';
		}
	}
	async function delZone(id: string) {
		const res = await fetch(`/api/profile/privacy-zones/${id}`, { method: 'DELETE' });
		if (res.ok) zones = zones.filter((z) => z.id !== id);
	}
</script>

<section class="space-y-6">
	<div>
		<h2 class="mb-1 text-lg font-semibold">Privacy &amp; Sharing</h2>
		<p class="text-sm text-zinc-400">Control who sees what across your activities.</p>
	</div>
	{#if loadError}
		<div class="rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-sm text-red-300">
			{loadError}
		</div>
	{/if}

	<!-- Public profile -->
	<div
		class="flex items-center justify-between rounded-lg border border-zinc-800 bg-zinc-900/40 p-4 max-md:gap-3"
	>
		<div>
			<div class="font-medium">Public profile</div>
			<div class="text-sm text-zinc-400">
				When on, anyone can view your profile at <code class="text-zinc-300">/u/{username}</code> (subject
				to the rules below).
			</div>
		</div>
		<button
			onclick={togglePublic}
			aria-label="Toggle public profile"
			class={`relative h-6 w-11 rounded-full transition max-md:shrink-0 ${isPublic ? 'bg-accent-500' : 'bg-zinc-700'}`}
		>
			<span
				class={`absolute top-0.5 h-5 w-5 rounded-full bg-white transition ${isPublic ? 'left-[22px]' : 'left-0.5'}`}
			></span>
		</button>
	</div>

	<!-- Visibility policy -->
	<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
		<div class="mb-1 font-medium">Who sees which data</div>
		<p class="mb-3 text-sm text-zinc-400">
			For each audience, tick the data they may see. Unticked fields are hidden; an audience with
			nothing ticked can't see the activity at all.
		</p>
		<div class="overflow-x-auto">
			<table class="w-full text-sm">
				<thead>
					<tr class="text-left text-xs uppercase text-zinc-500">
						<th class="py-1 pr-3">Data</th>
						{#each audiences as aud (aud)}<th class="px-2 text-center">{aud}</th>{/each}
					</tr>
				</thead>
				<tbody>
					{#each categories as cat (cat)}
						<tr class="border-t border-zinc-800">
							<td class="py-1 pr-3 font-medium">{cat.replace('_', ' ')}</td>
							{#each audiences as aud (aud)}
								<td class="px-2 text-center">
									<input
										type="checkbox"
										checked={has(aud, cat)}
										onchange={() => toggleCat(aud, cat)}
									/>
								</td>
							{/each}
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
		<div class="mt-3 flex items-center gap-3">
			<button
				onclick={savePolicy}
				disabled={saving || !policyLoaded}
				class="rounded bg-accent-500 px-4 py-2 text-sm font-medium text-zinc-950 hover:bg-accent-400 disabled:opacity-50"
			>
				{saving ? 'Saving…' : 'Save visibility'}
			</button>
			{#if saved}<span class="text-sm text-emerald-400">Saved ✓</span>{/if}
		</div>
	</div>

	<!-- Privacy zones -->
	<div class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
		<div class="mb-1 font-medium">Privacy zones</div>
		<p class="mb-3 text-sm text-zinc-400">
			Around each point, your start/finish coordinates <em>and</em> the resolved place name are hidden
			from everyone but you — useful for home and work. It only affects what others see; you always see
			your full data.
		</p>
		<ul class="mb-3 space-y-2">
			{#each zones as z (z.id)}
				<li
					class="flex items-center justify-between rounded border border-zinc-800 px-3 py-2 text-sm max-md:flex-wrap max-md:gap-2"
				>
					<span>
						{z.label || 'Zone'} — {z.lat.toFixed(4)}, {z.lng.toFixed(4)} (±{z.radius_m} m)
					</span>
					<button onclick={() => delZone(z.id)} class="text-xs text-zinc-500 hover:text-red-400">
						Remove
					</button>
				</li>
			{/each}
			{#if zones.length === 0}<li class="text-sm text-zinc-500">No privacy zones.</li>{/if}
		</ul>
		{#if zoneError}
			<div
				class="mb-2 rounded border border-red-700/50 bg-red-950/30 px-3 py-2 text-xs text-red-300"
			>
				{zoneError}
			</div>
		{/if}
		<div class="flex flex-wrap items-center gap-2">
			<input
				bind:value={newZone.label}
				placeholder="Label (e.g. Home)"
				class="w-32 rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-sm max-md:w-full max-md:py-2"
			/>
			<input
				bind:value={newZone.coords}
				placeholder="lat, lng"
				class="w-44 rounded border border-zinc-700 bg-zinc-950 px-2 py-1 font-mono text-sm max-md:w-full max-md:py-2"
			/>
			<input
				bind:value={newZone.radius_m}
				placeholder="Radius m"
				class="w-24 rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-sm max-md:w-full max-md:py-2"
			/>
			<button
				onclick={addZone}
				class="rounded border border-zinc-700 px-3 py-1 text-sm hover:bg-zinc-800 max-md:py-2"
			>
				Add zone
			</button>
			<button
				type="button"
				onclick={() => (showPicker = !showPicker)}
				class="rounded border border-zinc-700 px-3 py-1 text-sm hover:bg-zinc-800 max-md:py-2"
			>
				{showPicker ? 'Hide map' : 'Pick on map'}
			</button>
		</div>
		{#if showPicker}
			<div class="mt-3">
				<MapPicker
					onPick={(lat, lng) => {
						newZone.coords = `${lat.toFixed(5)}, ${lng.toFixed(5)}`;
						zoneError = null;
					}}
				/>
				<p class="mt-1 text-xs text-zinc-500">
					Click the map to set the centre{newZone.coords ? ` — selected ${newZone.coords}` : ''}.
				</p>
			</div>
		{/if}
		<p class="mt-2 text-xs text-zinc-500">
			Tip: click “Pick on map”, or paste coordinates from a map app (right-click → copy) into the
			<span class="font-mono">lat, lng</span> box.
		</p>
	</div>
</section>
