<script lang="ts">
	import { invalidateAll } from '$app/navigation';
	import { clients } from '#lib/connect.js';
	import { m } from '#lib/paraglide/messages.js';
	import type { GearView } from '#lib/gear.js';
	import GearPicker from './GearPicker.svelte';

	// Asks for the gear of an activity that has none. Saving either choice
	// (incl. "no gear" = dismiss) makes it the user's, so the prompt stays gone
	// across re-merges.
	let { activityId, type, gear }: { activityId: string; type: string; gear: GearView[] } = $props();

	let gearId = $state('');
	let busy = $state(false);
	let errMsg = $state<string | null>(null);

	async function save(id: string) {
		busy = true;
		errMsg = null;
		try {
			await clients.activity.updateActivity({ id: activityId, fieldMask: ['gear_id'], gearId: id });
			await invalidateAll();
		} catch (e) {
			errMsg = (e as Error).message;
		} finally {
			busy = false;
		}
	}
</script>

<div
	class="mt-2 flex flex-wrap items-center gap-2 rounded border border-amber-800/50 bg-amber-950/20 px-3 py-2 text-xs text-amber-200"
>
	<span
		class="rounded bg-amber-500/15 px-1.5 py-0.5 font-medium uppercase tracking-wide text-amber-300"
		>{m.gear_prompt_badge()}</span
	>
	<span>{m.gear_prompt_text()}</span>
	<div class="w-48 [&_select]:mt-0 [&_select]:py-1 [&_select]:text-xs">
		<GearPicker {gear} {type} bind:value={gearId} id="gear-prompt" disabled={busy} />
	</div>
	<button
		type="button"
		disabled={busy || !gearId}
		onclick={() => save(gearId)}
		class="rounded bg-accent-600 px-2 py-1 font-medium text-white hover:bg-accent-500 disabled:opacity-50"
	>
		{m.gear_prompt_save()}
	</button>
	<button
		type="button"
		disabled={busy}
		onclick={() => save('')}
		title={m.gear_prompt_dismiss_hint()}
		class="rounded border border-amber-700/50 px-2 py-1 hover:bg-amber-900/30 disabled:opacity-50"
	>
		{m.gear_prompt_dismiss()}
	</button>
	{#if errMsg}<span class="text-red-300">{errMsg}</span>{/if}
</div>
