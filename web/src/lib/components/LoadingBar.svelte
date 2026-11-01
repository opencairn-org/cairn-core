<script lang="ts">
	import { tick, untrack } from 'svelte';
	import { navigating } from '$app/state';
	import { loading } from '#lib/loading.svelte.js';
	import { m } from '#lib/paraglide/messages.js';

	// A navigation holds a slot for its whole lifetime: its load fetches are
	// tracked anyway, but route chunks arrive via import(), not fetch.
	$effect(() => {
		if (navigating.to) return loading.begin();
	});

	// There's no real progress to report, so NProgress-style: jump in, creep
	// asymptotically towards the end while work is pending, then fill and fade.
	let phase = $state<'hidden' | 'running' | 'finishing'>('hidden');
	let progress = $state(0);
	let trickle: ReturnType<typeof setInterval> | undefined;
	let fade: ReturnType<typeof setTimeout> | undefined;

	async function start() {
		clearTimeout(fade);
		if (phase === 'finishing') {
			// Restart from the left instead of animating backwards.
			phase = 'hidden';
			await tick();
		}
		phase = 'running';
		progress = 0.1;
		clearInterval(trickle);
		trickle = setInterval(() => (progress += (0.95 - progress) * 0.1), 400);
	}

	function finish() {
		clearInterval(trickle);
		if (phase !== 'running') return;
		phase = 'finishing';
		progress = 1;
		fade = setTimeout(() => (phase = 'hidden'), 450);
	}

	$effect(() => {
		const visible = loading.visible;
		untrack(() => (visible ? start() : finish()));
	});

	$effect(() => () => {
		clearInterval(trickle);
		clearTimeout(fade);
	});
</script>

<div
	class="pointer-events-none fixed inset-x-0 top-0 z-[60] h-0.5"
	hidden={phase === 'hidden'}
	role="progressbar"
	aria-label={m.loading_aria()}
>
	<div
		class="bar h-full origin-left bg-accent-400"
		class:opacity-0={phase === 'finishing'}
		style:transform="scaleX({progress})"
	></div>
</div>

<style>
	.bar {
		transition:
			transform 200ms ease-out,
			opacity 250ms ease 150ms;
	}
	@media (prefers-reduced-motion: reduce) {
		.bar {
			transition: none;
		}
	}
</style>
