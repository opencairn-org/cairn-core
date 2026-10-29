<script lang="ts">
	import { onMount } from 'svelte';
	import { m } from '#lib/paraglide/messages.js';
	import { page } from '$app/state';
	import LocaleSwitcher from '#lib/components/LocaleSwitcher.svelte';
	import PasskeyManager from '#lib/components/PasskeyManager.svelte';
	import TokenManager from '#lib/components/TokenManager.svelte';
	import AuthorizedApps from '#lib/components/AuthorizedApps.svelte';
	import WebhookManager from '#lib/components/WebhookManager.svelte';
	import PrivacySharingSettings from '#lib/components/PrivacySharingSettings.svelte';
	import MergePolicyEditor from '#lib/components/MergePolicyEditor.svelte';
	import { prefs, setPrefs } from '#lib/prefs.svelte.js';
	import { formatDate, formatDistance, formatElevation } from '#lib/format.js';
	import { connectClients } from '#lib/connect.js';
	import { DateFormat, TimeFormat, UnitSystem } from '#lib/proto/cairn/v1/auth_pb.js';

	let { data } = $props();

	// Seeded from the shared store (which the layout filled from the user).
	let dateFormat = $state(prefs.dateFormat);
	let timeFormat = $state(prefs.timeFormat);
	let units = $state(prefs.units);
	let saved = $state(false);
	let error = $state<string | null>(null);

	// --- notification email preferences ---
	type NotifPref = { event_type: number; key: string; label: string; email_enabled: boolean };
	let notifPrefs = $state<NotifPref[]>([]);
	let emailChannelAvailable = $state(false);
	// Activity quota usage (only surfaced when a limit applies).
	let quota = $state<{ used: number; limit: number; unlimited: boolean } | null>(null);
	onMount(async () => {
		try {
			const q = await fetch('/api/quota');
			if (q.ok) {
				const b = await q.json();
				quota = { used: b.activities_used, limit: b.activities_limit, unlimited: b.unlimited };
			}
		} catch {
			/* ignore */
		}
		try {
			const res = await fetch('/api/notifications/preferences');
			if (res.ok) {
				const body = await res.json();
				notifPrefs = body.preferences ?? [];
				emailChannelAvailable = !!body.email_enabled;
			}
		} catch {
			/* ignore */
		}
	});
	async function toggleNotif(p: NotifPref) {
		const next = !p.email_enabled;
		p.email_enabled = next; // optimistic
		try {
			await fetch('/api/notifications/preferences', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ event_type: p.event_type, channel: 'email', enabled: next })
			});
		} catch {
			p.email_enabled = !next; // revert on error
		}
	}

	// --- quiet hours ---
	type QuietHours = {
		enabled: boolean;
		start_minute: number;
		end_minute: number;
		days_of_week: number[];
		tz: string;
	};
	let quiet = $state<QuietHours>({
		enabled: false,
		start_minute: 1320,
		end_minute: 420,
		days_of_week: [],
		tz: 'UTC'
	});
	let quietLoaded = $state(false);
	let quietSaved = $state(false);
	const dayNames = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
	onMount(async () => {
		try {
			const res = await fetch('/api/notifications/quiet-hours');
			if (res.ok) {
				quiet = await res.json();
				// default the tz to the browser's on first setup
				if (!quiet.enabled && quiet.tz === 'UTC') {
					quiet.tz = Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
				}
			}
		} catch {
			/* ignore */
		} finally {
			quietLoaded = true;
		}
	});
	function minToHHMM(min: number): string {
		const h = Math.floor(min / 60);
		const m = min % 60;
		return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`;
	}
	function hhmmToMin(s: string): number {
		const [h, m] = s.split(':').map(Number);
		return (h || 0) * 60 + (m || 0);
	}
	function toggleDay(d: number) {
		quiet.days_of_week = quiet.days_of_week.includes(d)
			? quiet.days_of_week.filter((x) => x !== d)
			: [...quiet.days_of_week, d].sort();
	}
	async function saveQuiet() {
		try {
			const res = await fetch('/api/notifications/quiet-hours', {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(quiet)
			});
			if (res.ok) {
				quietSaved = true;
				setTimeout(() => (quietSaved = false), 1500);
			}
		} catch {
			/* ignore */
		}
	}

	// --- notification email test ---
	let testingEmail = $state(false);
	let emailMsg = $state<string | null>(null);
	let emailOk = $state(false);
	async function sendTestEmail() {
		testingEmail = true;
		emailMsg = null;
		try {
			const res = await fetch('/api/notifications/test-email', { method: 'POST' });
			const txt = (await res.text()).trim();
			emailOk = res.ok;
			emailMsg = res.ok ? `Sent to ${data.user.email}. Check your inbox.` : txt || res.statusText;
		} catch (e) {
			emailOk = false;
			emailMsg = (e as Error).message;
		} finally {
			testingEmail = false;
		}
	}

	function dateEnum(s: string): DateFormat {
		return s === 'iso'
			? DateFormat.ISO
			: s === 'us'
				? DateFormat.US
				: s === 'eu'
					? DateFormat.EU
					: DateFormat.UNSPECIFIED;
	}
	function timeEnum(s: string): TimeFormat {
		return s === '24h'
			? TimeFormat.TIME_FORMAT_24H
			: s === '12h'
				? TimeFormat.TIME_FORMAT_12H
				: TimeFormat.TIME_FORMAT_UNSPECIFIED;
	}
	function unitEnum(s: string): UnitSystem {
		return s === 'imperial' ? UnitSystem.IMPERIAL : UnitSystem.METRIC;
	}

	// Persist + apply immediately site-wide on any change.
	async function save() {
		error = null;
		setPrefs({ dateFormat, timeFormat, units });
		try {
			const clients = connectClients(fetch, page.url.origin);
			await clients.auth.updateUserPreferences({
				dateFormat: dateEnum(dateFormat),
				timeFormat: timeEnum(timeFormat),
				units: unitEnum(units)
			});
			saved = true;
			setTimeout(() => (saved = false), 1500);
		} catch (e) {
			error = (e as Error).message;
		}
	}

	const sampleIso = '2026-06-02T14:30:00Z';
</script>

<div class="space-y-8">
	<header>
		<h1 class="text-2xl font-semibold tracking-tight">{m.settings_title()}</h1>
		<p class="mt-1 text-sm text-zinc-400">{m.settings_intro()}</p>
	</header>

	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
		<h2 class="mb-4 text-sm font-medium text-zinc-300">{m.settings_account_section()}</h2>
		<dl class="grid grid-cols-1 gap-3 text-sm sm:grid-cols-[160px_1fr]">
			<dt class="text-zinc-500">{m.settings_field_display_name()}</dt>
			<dd class="text-zinc-200">{data.user.displayName}</dd>
			<dt class="text-zinc-500">{m.settings_field_username()}</dt>
			<dd class="text-zinc-200">{data.user.username}</dd>
			<dt class="text-zinc-500">{m.settings_field_email()}</dt>
			<dd class="text-zinc-200">{data.user.email || m.placeholder_dash()}</dd>
			{#if quota && !quota.unlimited}
				<dt class="text-zinc-500">Activity quota</dt>
				<dd class="text-zinc-200">
					{quota.used} / {quota.limit}
					{#if quota.used >= quota.limit}<span class="ml-2 text-red-400">(limit reached)</span>{/if}
				</dd>
			{/if}
		</dl>
		{#if data.user.email}
			<div class="mt-4 flex items-center gap-3 border-t border-zinc-800 pt-4">
				<button
					type="button"
					disabled={testingEmail}
					onclick={sendTestEmail}
					class="rounded border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 hover:border-accent-500 hover:text-accent-300 disabled:opacity-50"
				>
					{testingEmail ? 'Sending…' : 'Send test email'}
				</button>
				{#if emailMsg}
					<span class="text-xs {emailOk ? 'text-emerald-400' : 'text-red-400'}">{emailMsg}</span>
				{/if}
			</div>
		{/if}
	</section>

	{#if notifPrefs.length > 0}
		<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
			<h2 class="mb-1 text-sm font-medium text-zinc-300">Notification emails</h2>
			<p class="mb-4 text-xs text-zinc-500">
				The in-app feed always shows everything. Choose which also email you{#if !emailChannelAvailable}<span
						class="text-amber-400"
					>
						(email isn't configured on this instance, so these won't send)</span
					>{/if}.
			</p>
			<ul class="divide-y divide-zinc-800 rounded border border-zinc-800">
				{#each notifPrefs as p (p.event_type)}
					<li class="flex items-center justify-between gap-4 px-4 py-2.5 text-sm">
						<span class="text-zinc-300">{p.label}</span>
						<button
							type="button"
							role="switch"
							aria-checked={p.email_enabled}
							aria-label={p.label}
							onclick={() => toggleNotif(p)}
							class="relative h-5 w-9 shrink-0 rounded-full transition-colors {p.email_enabled
								? 'bg-accent-500'
								: 'bg-zinc-700'}"
						>
							<span
								class="absolute top-0.5 h-4 w-4 rounded-full bg-white transition-all {p.email_enabled
									? 'left-4'
									: 'left-0.5'}"
							></span>
						</button>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	{#if quietLoaded}
		<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
			<div class="mb-1 flex items-center gap-3">
				<h2 class="text-sm font-medium text-zinc-300">Quiet hours</h2>
				{#if quietSaved}<span class="text-xs text-emerald-400">saved</span>{/if}
			</div>
			<p class="mb-4 text-xs text-zinc-500">
				Hold back email &amp; webhook notifications during these hours. The in-app feed and urgent
				alerts (connection broken, worker offline) always come through.
			</p>
			<label class="mb-4 flex items-center gap-2 text-sm text-zinc-300">
				<input
					type="checkbox"
					bind:checked={quiet.enabled}
					onchange={saveQuiet}
					class="accent-accent-500"
				/>
				Enable quiet hours
			</label>
			{#if quiet.enabled}
				<div class="flex flex-wrap items-end gap-4">
					<div>
						<span class="mb-1 block text-xs text-zinc-500">From</span>
						<input
							type="time"
							value={minToHHMM(quiet.start_minute)}
							onchange={(e) => {
								quiet.start_minute = hhmmToMin((e.target as HTMLInputElement).value);
								saveQuiet();
							}}
							class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
						/>
					</div>
					<div>
						<span class="mb-1 block text-xs text-zinc-500">To</span>
						<input
							type="time"
							value={minToHHMM(quiet.end_minute)}
							onchange={(e) => {
								quiet.end_minute = hhmmToMin((e.target as HTMLInputElement).value);
								saveQuiet();
							}}
							class="rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
						/>
					</div>
					<div class="flex-1">
						<span class="mb-1 block text-xs text-zinc-500">Timezone</span>
						<input
							bind:value={quiet.tz}
							onchange={saveQuiet}
							placeholder="Europe/Berlin"
							class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
						/>
					</div>
				</div>
				<div class="mt-3">
					<span class="mb-1 block text-xs text-zinc-500">Days (none = every day)</span>
					<div class="flex flex-wrap gap-1.5">
						{#each dayNames as d, i (d)}
							<button
								type="button"
								onclick={() => {
									toggleDay(i);
									saveQuiet();
								}}
								class="rounded border px-2.5 py-1 text-xs {quiet.days_of_week.includes(i)
									? 'border-accent-500 bg-accent-500/15 text-accent-200'
									: 'border-zinc-700 text-zinc-400 hover:border-zinc-600'}"
							>
								{d}
							</button>
						{/each}
					</div>
				</div>
			{/if}
		</section>
	{/if}

	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
		<div class="mb-4 flex items-center gap-3">
			<h2 class="text-sm font-medium text-zinc-300">Format &amp; units</h2>
			{#if saved}<span class="text-xs text-emerald-400">saved</span>{/if}
			{#if error}<span class="text-xs text-red-400">{error}</span>{/if}
		</div>
		<p class="mb-4 text-xs text-zinc-500">Applied everywhere, on every device you sign in on.</p>

		<div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
			<div>
				<label for="date-fmt" class="mb-1 block text-xs text-zinc-500">Date format</label>
				<select
					id="date-fmt"
					bind:value={dateFormat}
					onchange={save}
					class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				>
					<option value="">Locale default</option>
					<option value="iso">ISO — 2026-06-02</option>
					<option value="us">US — 06/02/2026</option>
					<option value="eu">EU — 02.06.2026</option>
				</select>
			</div>
			<div>
				<label for="time-fmt" class="mb-1 block text-xs text-zinc-500">Time format</label>
				<select
					id="time-fmt"
					bind:value={timeFormat}
					onchange={save}
					class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				>
					<option value="">Locale default</option>
					<option value="24h">24-hour — 14:30</option>
					<option value="12h">12-hour — 2:30 PM</option>
				</select>
			</div>
			<div>
				<label for="unit-fmt" class="mb-1 block text-xs text-zinc-500">Units</label>
				<select
					id="unit-fmt"
					bind:value={units}
					onchange={save}
					class="w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm focus:border-accent-400 focus:outline-none"
				>
					<option value="metric">Metric — km, m</option>
					<option value="imperial">Imperial — mi, ft</option>
				</select>
			</div>
		</div>

		<p class="mt-4 text-xs text-zinc-500">
			Preview: <span class="text-zinc-300">{formatDate(sampleIso)}</span> ·
			<span class="text-zinc-300">{formatDistance(10500)}</span> ·
			<span class="text-zinc-300">{formatElevation(420)}</span>
		</p>
	</section>

	<section class="rounded-lg border border-zinc-800 bg-zinc-900/40 p-5">
		<h2 class="mb-4 text-sm font-medium text-zinc-300">{m.settings_language_section()}</h2>
		<LocaleSwitcher />
	</section>

	<PrivacySharingSettings username={data.user.username} />

	<MergePolicyEditor />

	<PasskeyManager />

	<TokenManager />

	<AuthorizedApps />

	<WebhookManager />
</div>
