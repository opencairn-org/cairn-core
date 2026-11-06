import { getLocale } from '#lib/paraglide/runtime.js';
import { prefs } from '#lib/prefs.svelte.js';

// UI-side formatters. The Go server reports SI units (meters, seconds, m/s);
// this module converts to human-friendly strings, honouring the user's
// preferences (unit system + date/time format) from #lib/prefs. Reading
// `prefs.*` here makes every caller reactive to preference changes.

const DASH = '—';

const M_PER_MILE = 1609.344;
const FT_PER_M = 3.28084;

// Map paraglide locale → BCP 47 tag for Intl APIs.
export function intlLocale(): string {
	switch (getLocale()) {
		case 'de':
			return 'de-DE';
		case 'en':
			return 'en-US';
		default:
			return 'de-DE';
	}
}

function num(v: number, frac = 2): string {
	return new Intl.NumberFormat(intlLocale(), {
		minimumFractionDigits: frac,
		maximumFractionDigits: frac
	}).format(v);
}

export function formatDistance(m: number | null | undefined): string {
	if (m == null) return DASH;
	if (prefs.units === 'imperial') {
		if (m < M_PER_MILE / 10) return `${Math.round(m * FT_PER_M)} ft`;
		return `${num(m / M_PER_MILE)} mi`;
	}
	if (m < 1000) return `${Math.round(m)} m`;
	return `${num(m / 1000)} km`;
}

export function formatDuration(seconds: number | null | undefined): string {
	if (seconds == null) return DASH;
	const total = Math.round(seconds);
	const h = Math.floor(total / 3600);
	const m = Math.floor((total % 3600) / 60);
	const s = total % 60;
	if (h > 0) return `${h}:${pad(m)}:${pad(s)}`;
	return `${m}:${pad(s)}`;
}

export function formatPace(metersPerSecond: number | null | undefined): string {
	if (metersPerSecond == null || metersPerSecond <= 0) return DASH;
	if (prefs.units === 'imperial') {
		const minPerMile = M_PER_MILE / metersPerSecond / 60;
		const whole = Math.floor(minPerMile);
		const rem = Math.round((minPerMile - whole) * 60);
		return `${whole}:${pad(rem)} /mi`;
	}
	const minPerKm = 1000 / metersPerSecond / 60;
	const whole = Math.floor(minPerKm);
	const rem = Math.round((minPerKm - whole) * 60);
	return `${whole}:${pad(rem)} /km`;
}

export function formatElevation(m: number | null | undefined): string {
	if (m == null) return DASH;
	if (prefs.units === 'imperial') return `${Math.round(m * FT_PER_M)} ft`;
	return `${Math.round(m)} m`;
}

export function formatSpeed(metersPerSecond: number | null | undefined): string {
	if (metersPerSecond == null || metersPerSecond < 0) return DASH;
	if (prefs.units === 'imperial') {
		return `${num((metersPerSecond * 3600) / M_PER_MILE)} mph`;
	}
	return `${num((metersPerSecond * 3600) / 1000)} km/h`;
}

export function formatTemp(celsius: number | null | undefined): string {
	if (celsius == null) return DASH;
	if (prefs.units === 'imperial') return `${Math.round((celsius * 9) / 5 + 32)}°F`;
	return `${Math.round(celsius)}°C`;
}

export function formatHeartRate(bpm: number | null | undefined): string {
	if (bpm == null) return DASH;
	return `${bpm} bpm`;
}

export function formatPower(w: number | null | undefined): string {
	if (w == null) return DASH;
	return `${w} W`;
}

// ---- Date / time, honouring the user's explicit format choices ----

function formatDatePart(d: Date): string {
	switch (prefs.dateFormat) {
		case 'iso':
			return d.toLocaleDateString('sv-SE'); // YYYY-MM-DD
		case 'us':
			return d.toLocaleDateString('en-US'); // M/D/YYYY
		case 'eu':
			return d.toLocaleDateString('de-DE'); // DD.MM.YYYY
		default:
			return d.toLocaleDateString(intlLocale(), { dateStyle: 'medium' });
	}
}

function formatTimePart(d: Date): string {
	switch (prefs.timeFormat) {
		case '24h':
			return d.toLocaleTimeString(intlLocale(), {
				hour: '2-digit',
				minute: '2-digit',
				hour12: false
			});
		case '12h':
			return d.toLocaleTimeString(intlLocale(), {
				hour: 'numeric',
				minute: '2-digit',
				hour12: true
			});
		default:
			return d.toLocaleTimeString(intlLocale(), { timeStyle: 'short' });
	}
}

export function formatDate(iso: string): string {
	const d = new Date(iso);
	return `${formatDatePart(d)} ${formatTimePart(d)}`;
}

export function formatDateOnly(iso: string): string {
	return formatDatePart(new Date(iso));
}

export function formatRelativeDate(iso: string): string {
	const d = new Date(iso);
	const now = new Date();
	const diffSec = (now.getTime() - d.getTime()) / 1000;
	const rtf = new Intl.RelativeTimeFormat(intlLocale(), { numeric: 'auto' });
	if (diffSec < 60) return rtf.format(-Math.round(diffSec), 'second');
	if (diffSec < 3600) return rtf.format(-Math.round(diffSec / 60), 'minute');
	if (diffSec < 86400) return rtf.format(-Math.round(diffSec / 3600), 'hour');
	if (diffSec < 86400 * 7) return rtf.format(-Math.round(diffSec / 86400), 'day');
	return formatDatePart(d);
}

function pad(n: number): string {
	return n.toString().padStart(2, '0');
}
