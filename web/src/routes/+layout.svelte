<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { m } from '#lib/paraglide/messages.js';
	import { setPrefs } from '#lib/prefs.svelte.js';
	import { formatDate } from '#lib/format.js';
	import LoadingBar from '#lib/components/LoadingBar.svelte';

	let { children, data } = $props();

	// Email-verification banner (logged-in users with an unverified email).
	let resendState = $state<'idle' | 'sending' | 'sent' | 'error'>('idle');
	async function resendVerification() {
		resendState = 'sending';
		try {
			const res = await fetch('/auth/email/verify/send', { method: 'POST' });
			resendState = res.ok ? 'sent' : 'error';
		} catch {
			resendState = 'error';
		}
	}

	// Seed the site-wide format preferences from the logged-in user, and keep
	// them in sync whenever the user data changes (e.g. after editing settings).
	$effect(() => {
		if (data.user) {
			setPrefs({
				units: data.user.units,
				dateFormat: data.user.dateFormat,
				timeFormat: data.user.timeFormat
			});
		}
	});

	// Mobile off-canvas sidebar drawer. Closed on every navigation.
	let mobileNavOpen = $state(false);
	$effect(() => {
		void page.url.pathname;
		mobileNavOpen = false;
	});

	let build = $state<{ version: string; commit: string; build_time: string } | null>(null);
	onMount(async () => {
		try {
			const res = await fetch('/api/version');
			if (res.ok) build = await res.json();
		} catch {
			/* ignore */
		}
	});
	const buildLabel = $derived(
		build
			? [
					build.version && `v${build.version}`,
					build.commit,
					build.build_time && `built ${formatDate(build.build_time)}`
				]
					.filter(Boolean)
					.join(' · ')
			: ''
	);

	// Icons are single multi-subpath `d` strings (24×24, stroked).
	const icons = {
		overview: 'M3 11.5 12 4l9 7.5 M5 10v10h5v-6h4v6h5V10',
		activities: 'M8 6h13 M8 12h13 M8 18h13 M3.5 6h.01 M3.5 12h.01 M3.5 18h.01',
		heatmap:
			'M12 22c4.4 0 7-2.9 7-6.5 0-3-1.7-5-3.5-7-.5 2-1.5 3-2.5 3.5.5-3-1-6.5-4-9 .3 3-1 4.5-2.5 6.5C5 11.5 5 13.7 5 15.5 5 19.1 7.6 22 12 22z',
		analysis: 'M3 3v18h18 M7 14l4-4 3 3 5-6',
		stats: 'M6 20v-8 M12 20V6 M18 20v-5',
		records:
			'M8 21h8 M12 17v4 M7 4h10v5a5 5 0 0 1-10 0z M7 6H4.5a2.5 2.5 0 0 0 2.6 4 M17 6h2.5a2.5 2.5 0 0 1-2.6 4',
		segments: 'm8 3 4 8 5-5 5 15H2L8 3',
		feed: 'M4 11a9 9 0 0 1 9 9 M4 4a16 16 0 0 1 16 16 M5 19h.01',
		clubs:
			'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8 M22 21v-2a4 4 0 0 0-3-3.87 M16 3.13a4 4 0 0 1 0 7.75',
		notifications: 'M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9 M13.7 21a2 2 0 0 1-3.4 0',
		athlete: 'M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2 M12 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8',
		health:
			'M19.5 12.6 12 20l-7.5-7.4A5 5 0 1 1 12 6.3a5 5 0 1 1 7.5 6.3 M6.7 12h1.5l1.3-2 2.5 4.5 1.3-2h2',
		connections:
			'M10 13a5 5 0 0 0 7.5.5l3-3a5 5 0 0 0-7.1-7.1L12 4.8 M14 11a5 5 0 0 0-7.5-.5l-3 3a5 5 0 0 0 7.1 7.1L12 19.2',
		gear: 'M5.5 17.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z M18.5 17.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z M5.5 14l3-7h7l3 7 M8.5 7l1.5 3h6 M12 14V7',
		review:
			'M22 12h-6l-2 3h-4l-2-3H2 M5.4 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.4-6.9A2 2 0 0 0 16.8 4H7.2a2 2 0 0 0-1.8 1.1z',
		settings: 'M4 21v-7 M4 10V3 M12 21v-9 M12 8V3 M20 21v-5 M20 12V3 M2 14h4 M10 8h4 M18 16h4',
		admin: 'M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z',
		moderation: 'M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z M4 22v-7',
		logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4 M16 17l5-5-5-5 M21 12H9'
	};

	type NavItem = { href: string; label: string; icon: string; badge?: number };
	type NavGroup = { title: string; items: NavItem[] };

	const unread = $derived(data.unreadCount ?? 0);
	const isAdmin = $derived(data.permissions?.includes('admin') ?? false);
	const canModerate = $derived(data.permissions?.includes('moderate') ?? false);

	const groups = $derived<NavGroup[]>([
		{
			title: '',
			items: [
				{ href: '/', label: m.nav_overview(), icon: icons.overview },
				{ href: '/activities', label: m.nav_activities(), icon: icons.activities },
				{ href: '/heatmap', label: 'Heatmap', icon: icons.heatmap },
				{ href: '/analysis', label: m.nav_analysis(), icon: icons.analysis },
				{ href: '/stats', label: m.nav_stats(), icon: icons.stats },
				{ href: '/records', label: 'Records', icon: icons.records },
				{ href: '/segments', label: m.nav_segments(), icon: icons.segments }
			]
		},
		{
			title: 'Social',
			items: [
				{ href: '/feed', label: 'Feed', icon: icons.feed },
				{ href: '/clubs', label: 'Clubs', icon: icons.clubs }
			]
		},
		{
			title: 'Account',
			items: [
				{
					href: '/notifications',
					label: m.notifications_aria(),
					icon: icons.notifications,
					badge: unread
				},
				{ href: '/athlete', label: m.nav_athlete(), icon: icons.athlete },
				{ href: '/health', label: 'Health', icon: icons.health },
				{ href: '/gear', label: m.nav_gear(), icon: icons.gear },
				{ href: '/connections', label: m.nav_connections(), icon: icons.connections },
				{ href: '/review', label: 'Review queue', icon: icons.review },
				{ href: '/settings', label: m.nav_settings(), icon: icons.settings }
			]
		},
		...(isAdmin
			? [{ title: 'Admin', items: [{ href: '/admin', label: m.nav_admin(), icon: icons.admin }] }]
			: canModerate
				? [
						{
							title: 'Admin',
							items: [{ href: '/admin/moderation', label: 'Moderation', icon: icons.moderation }]
						}
					]
				: [])
	]);

	function isActive(href: string): boolean {
		return href === '/' ? page.url.pathname === '/' : page.url.pathname.startsWith(href);
	}

	const initials = $derived(
		(data.user?.displayName ?? '')
			.split(/\s+/)
			.map((w: string) => w[0])
			.slice(0, 2)
			.join('')
			.toUpperCase()
	);
	const roleLabel = $derived(isAdmin ? 'Admin' : canModerate ? 'Moderator' : '');
</script>

{#snippet brand()}
	<a href="/" class="flex items-center gap-2 font-semibold tracking-tight">
		<svg viewBox="0 0 64 64" class="h-7 w-7 shrink-0" role="img" aria-label="Cairn logo">
			<rect width="64" height="64" rx="14" fill="#1f2937" />
			<rect x="13" y="45" width="38" height="11" rx="5.5" fill="#d6d3d1" />
			<rect x="17.5" y="33.5" width="29" height="10" rx="5" fill="#a8a29e" />
			<rect x="19.5" y="23" width="24" height="9" rx="4.5" fill="#cfcbc4" />
			<rect x="24" y="13.5" width="16" height="8" rx="4" fill="#2dd4bf" />
		</svg>
		<span class="text-accent-400">Cairn</span>
	</a>
{/snippet}

{#snippet navList()}
	{#each groups as group (group.title)}
		{#if group.title}
			<div class="px-3 pb-1 pt-4 text-[11px] font-medium uppercase tracking-wider text-zinc-500">
				{group.title}
			</div>
		{/if}
		{#each group.items as item (item.href)}
			{@const active = isActive(item.href)}
			<a
				href={item.href}
				class="flex items-center gap-2.5 rounded px-3 py-1.5 text-sm transition-colors"
				class:bg-zinc-800={active}
				class:text-zinc-100={active}
				class:text-zinc-400={!active}
				class:hover:text-zinc-100={!active}
			>
				<svg
					class="h-[18px] w-[18px] shrink-0"
					class:text-accent-400={active}
					viewBox="0 0 24 24"
					fill="none"
					stroke="currentColor"
					stroke-width="1.8"
					stroke-linecap="round"
					stroke-linejoin="round"
				>
					<path d={item.icon} />
				</svg>
				<span class="truncate">{item.label}</span>
				{#if item.badge}
					<span
						class="ml-auto flex h-4 min-w-4 items-center justify-center rounded-full bg-accent-500 px-1 text-[10px] font-semibold text-zinc-950"
					>
						{item.badge > 9 ? '9+' : item.badge}
					</span>
				{/if}
			</a>
		{/each}
	{/each}
{/snippet}

{#snippet userCard()}
	<div class="border-t border-zinc-800 p-3">
		<div class="flex items-center gap-2.5 rounded-md bg-zinc-800/60 px-2.5 py-2">
			<span
				class="flex h-8 w-8 shrink-0 items-center justify-center rounded-full bg-accent-500/20 text-xs font-semibold text-accent-300"
			>
				{initials}
			</span>
			<span class="min-w-0 flex-1">
				<span class="block truncate text-sm text-zinc-200">{data.user?.displayName}</span>
				{#if roleLabel}
					<span class="block text-[11px] text-zinc-500">{roleLabel}</span>
				{/if}
			</span>
			<form method="POST" action="/auth/logout">
				<button
					type="submit"
					class="rounded p-1.5 text-zinc-500 transition-colors hover:text-accent-300"
					aria-label={m.menu_logout()}
					title={m.menu_logout()}
				>
					<svg
						class="h-4 w-4"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
						stroke-linejoin="round"
					>
						<path d={icons.logout} />
					</svg>
				</button>
			</form>
		</div>
	</div>
{/snippet}

<LoadingBar />

<div class="flex min-h-screen">
	{#if data.user}
		<!-- Desktop sidebar -->
		<aside
			class="sticky top-0 flex h-dvh w-56 shrink-0 flex-col border-r border-zinc-800 bg-zinc-900/80 max-md:hidden"
		>
			<div class="flex items-center px-4 py-4">
				{@render brand()}
			</div>
			<nav class="flex-1 overflow-y-auto px-2 pb-4">
				{@render navList()}
			</nav>
			{@render userCard()}
		</aside>

		<!-- Mobile drawer -->
		{#if mobileNavOpen}
			<div class="fixed inset-0 z-40 md:hidden">
				<button
					type="button"
					class="absolute inset-0 cursor-default bg-zinc-950/60"
					aria-label="Close menu"
					onclick={() => (mobileNavOpen = false)}
				></button>
				<aside
					class="absolute left-0 top-0 flex h-full w-64 flex-col border-r border-zinc-800 bg-zinc-900"
				>
					<div class="flex items-center justify-between px-4 py-4">
						{@render brand()}
						<button
							type="button"
							class="rounded p-2 text-zinc-400 hover:text-zinc-100"
							aria-label="Close menu"
							onclick={() => (mobileNavOpen = false)}
						>
							<svg
								class="h-5 w-5"
								viewBox="0 0 24 24"
								fill="none"
								stroke="currentColor"
								stroke-width="2"
								stroke-linecap="round"
							>
								<path d="M6 6l12 12M18 6L6 18" />
							</svg>
						</button>
					</div>
					<nav class="flex-1 overflow-y-auto px-2 pb-4">
						{@render navList()}
					</nav>
					{@render userCard()}
				</aside>
			</div>
		{/if}
	{/if}

	<div class="flex min-w-0 flex-1 flex-col">
		{#if data.user}
			<!-- Mobile top bar — hamburger + brand + notifications. Never rendered ≥md. -->
			<header
				class="flex items-center gap-3 border-b border-zinc-800 bg-zinc-900/80 px-4 py-3 backdrop-blur md:hidden"
			>
				<button
					type="button"
					class="rounded p-2 text-zinc-400 hover:text-zinc-100"
					aria-label="Menu"
					aria-expanded={mobileNavOpen}
					onclick={() => (mobileNavOpen = !mobileNavOpen)}
				>
					<svg
						class="h-5 w-5"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="2"
						stroke-linecap="round"
					>
						<path d="M4 7h16M4 12h16M4 17h16" />
					</svg>
				</button>
				{@render brand()}
				<a
					href="/notifications"
					class="relative ml-auto rounded p-1.5 text-zinc-400 transition-colors hover:text-zinc-100"
					class:text-zinc-100={unread > 0}
					aria-label={m.notifications_aria()}
					title={m.notifications_aria()}
				>
					<svg
						class="h-5 w-5"
						viewBox="0 0 24 24"
						fill="none"
						stroke="currentColor"
						stroke-width="1.8"
						stroke-linecap="round"
						stroke-linejoin="round"
					>
						<path d={icons.notifications} />
					</svg>
					{#if unread > 0}
						<span
							class="absolute -right-0.5 -top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-accent-500 px-1 text-[10px] font-semibold text-zinc-950"
						>
							{unread > 9 ? '9+' : unread}
						</span>
					{/if}
				</a>
			</header>
		{:else}
			<!-- Logged-out header (login, signup, shared/public pages) -->
			<header class="border-b border-zinc-800 bg-zinc-900/80 backdrop-blur">
				<div class="mx-auto flex max-w-6xl items-center gap-3 px-6 py-3 max-md:px-4">
					{@render brand()}
					<span class="text-xs text-zinc-500 max-md:hidden">{m.app_subtitle()}</span>
					<a
						href="/login"
						class="ml-auto rounded border border-accent-500 bg-accent-500/20 px-3 py-1.5 text-xs text-accent-300 hover:bg-accent-500/30"
					>
						{m.menu_signin()}
					</a>
				</div>
			</header>
		{/if}

		{#if data.user && data.user.email && !data.user.emailVerified}
			<div class="border-b border-amber-700/40 bg-amber-950/30 px-6 py-2 text-xs text-amber-200">
				<div class="mx-auto flex w-full max-w-6xl flex-wrap items-center gap-x-3 gap-y-1">
					<span>Please verify your email address ({data.user.email}).</span>
					{#if resendState === 'sent'}
						<span class="text-emerald-300">Verification email sent — check your inbox.</span>
					{:else if resendState === 'error'}
						<span class="text-red-300">Couldn't send — try again shortly.</span>
					{:else}
						<button
							type="button"
							disabled={resendState === 'sending'}
							onclick={resendVerification}
							class="rounded border border-amber-700/50 px-2 py-0.5 text-amber-200 hover:border-amber-500 hover:text-amber-100 disabled:opacity-50"
						>
							{resendState === 'sending' ? 'Sending…' : 'Resend verification'}
						</button>
					{/if}
				</div>
			</div>
		{/if}

		<main class="mx-auto w-full max-w-6xl flex-1 px-6 py-8 max-md:px-4 max-md:py-5">
			{@render children()}
		</main>
		<footer
			class="flex flex-wrap items-center justify-between gap-2 border-t border-zinc-800 px-6 py-4 text-xs text-zinc-500"
		>
			<span>{m.footer_tagline()}</span>
			{#if buildLabel}
				<span class="font-mono text-[10px] text-zinc-600" title="Build info">{buildLabel}</span>
			{/if}
		</footer>
	</div>
</div>
