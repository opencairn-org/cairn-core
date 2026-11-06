import { intlLocale } from './format';

// Display helpers for the metrics store (GET /api/metrics/*). Keys follow
// metrics.proto: reserved `<name>_<unit>` or provider-namespaced `<provider>.<name>_<unit>`.

export type StoredMetric = {
	id: string;
	key: string;
	ts: string;
	value_numeric: number | null;
	value_struct: unknown;
	provider: string;
	external_id: string | null;
	source_worker_name: string;
	source_worker_version: string;
};

export type MetricGroup = { namespace: string; title: string; metrics: StoredMetric[] };

type Unit = { suffix: string; keep?: string; format: (v: number) => string };

const dec = (v: number, frac: number) =>
	v.toLocaleString(intlLocale(), { maximumFractionDigits: frac });

// Seconds as m:ss.
function clock(s: number): string {
	const t = Math.round(s);
	return `${Math.floor(t / 60)}:${String(t % 60).padStart(2, '0')}`;
}

function minutes(v: number): string {
	const t = Math.round(v);
	return t < 60 ? `${t} min` : `${Math.floor(t / 60)} h ${t % 60} min`;
}

const UNIT_TABLE: Unit[] = [
	{ suffix: '_s_per_100m', format: (v) => `${clock(v)} /100m` },
	{ suffix: '_s_per_km', format: (v) => `${clock(v)} /km` },
	{ suffix: '_ml_kg_min', format: (v) => `${dec(v, 1)} ml/kg/min` },
	{ suffix: '_breath_per_min', keep: ' breath', format: (v) => `${dec(v, 1)} br/min` },
	{ suffix: '_brpm', format: (v) => `${dec(v, 1)} br/min` },
	{ suffix: '_mmhg', format: (v) => `${dec(v, 0)} mmHg` },
	{ suffix: '_kcal', format: (v) => `${dec(v, 0)} kcal` },
	{ suffix: '_bpm', format: (v) => `${dec(v, 0)} bpm` },
	{ suffix: '_pct', format: (v) => `${dec(v, 1)} %` },
	{ suffix: '_mps', format: (v) => `${dec(v, 2)} m/s` },
	{ suffix: '_min', format: minutes },
	{ suffix: '_kg', format: (v) => `${dec(v, 1)} kg` },
	{ suffix: '_kj', format: (v) => `${dec(v, 1)} kJ` },
	{ suffix: '_ms', format: (v) => `${dec(v, 0)} ms` },
	{ suffix: '_c', format: (v) => `${dec(v, 1)} °C` },
	{ suffix: '_m', format: (v) => (v >= 1000 ? `${dec(v / 1000, 2)} km` : `${dec(v, 2)} m`) },
	{ suffix: '_w', format: (v) => `${dec(v, 0)} W` }
];

// Longest suffix first, so `_ml_kg_min` wins over `_min`.
const UNITS = [...UNIT_TABLE].sort((a, b) => b.suffix.length - a.suffix.length);

const WORDS: Record<string, string> = {
	ans: 'ANS',
	atl: 'ATL',
	css: 'CSS',
	ctl: 'CTL',
	ftp: 'FTP',
	hr: 'HR',
	hrv: 'HRV',
	lthr: 'LTHR',
	met: 'MET',
	rem: 'REM',
	rmssd: 'RMSSD',
	sdnn: 'SDNN',
	spo2: 'SpO₂',
	tsb: 'TSB',
	tss: 'TSS',
	vo2max: 'VO₂max'
};

function unitOf(name: string): Unit | undefined {
	return UNITS.find((u) => name.endsWith(u.suffix) && name.length > u.suffix.length);
}

/** The provider namespace of a key, '' for a reserved key. */
export function metricNamespace(key: string): string {
	const dot = key.indexOf('.');
	return dot < 0 ? '' : key.slice(0, dot);
}

/** snake_case words as a sentence-case label, acronyms kept. */
export function humanize(words: string): string {
	const out = words
		.split(/[_\s]+/)
		.filter(Boolean)
		.map((w) => WORDS[w] ?? w)
		.join(' ');
	return out.charAt(0).toUpperCase() + out.slice(1);
}

/** A key's label without namespace and unit: `oura.sleep_latency_min` → "Sleep latency". */
export function metricLabel(key: string): string {
	const name = key.slice(key.indexOf('.') + 1);
	const unit = unitOf(name);
	return humanize(unit ? name.slice(0, -unit.suffix.length) + (unit.keep ?? '') : name);
}

/** A numeric value with the unit its key's suffix names. */
export function formatMetricValue(key: string, v: number): string {
	const unit = unitOf(key.slice(key.indexOf('.') + 1));
	return unit ? unit.format(v) : dec(v, Number.isInteger(v) ? 0 : 2);
}

const MAX_ENTRIES = 12;

// Minutes per stage of a sleep_stages value ({stages: [{start, end, stage}]}).
function stageTotals(stages: unknown[]): [string, string][] {
	const total = new Map<string, number>();
	for (const s of stages) {
		const { start, end, stage } = (s ?? {}) as Record<string, unknown>;
		const ms = Date.parse(String(end)) - Date.parse(String(start));
		if (typeof stage !== 'string' || !(ms >= 0)) continue;
		total.set(stage, (total.get(stage) ?? 0) + ms / 60000);
	}
	return [...total].map(([stage, min]) => [humanize(stage), minutes(min)]);
}

function entry(key: string, v: unknown): string | null {
	if (typeof v === 'number') return formatMetricValue(key, v);
	if (typeof v === 'string') return v;
	if (typeof v === 'boolean') return v ? 'yes' : 'no';
	if (Array.isArray(v)) return `${v.length} entries`;
	return null;
}

/** Label/value pairs of a structured value: stage totals, else top-level fields with one nested level. */
export function structEntries(value: unknown): [string, string][] {
	if (value == null) return [];
	if (typeof value !== 'object' || Array.isArray(value)) {
		const v = entry('', value);
		return v == null ? [] : [['', v]];
	}
	const obj = value as Record<string, unknown>;
	if (Array.isArray(obj.stages)) return stageTotals(obj.stages).slice(0, MAX_ENTRIES);
	const out: [string, string][] = [];
	for (const [k, v] of Object.entries(obj)) {
		if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
			for (const [ck, cv] of Object.entries(v)) {
				const s = entry(ck, cv);
				if (s != null) out.push([`${humanize(k)} · ${humanize(ck).toLowerCase()}`, s]);
			}
			continue;
		}
		const s = entry(k, v);
		if (s != null) out.push([metricLabel(k), s]);
	}
	return out.slice(0, MAX_ENTRIES);
}

/** Reserved keys first ("Standard"), then one group per provider namespace; labels sorted. */
export function groupMetrics(metrics: StoredMetric[]): MetricGroup[] {
	const by = new Map<string, StoredMetric[]>();
	for (const m of metrics) {
		const ns = metricNamespace(m.key);
		by.set(ns, [...(by.get(ns) ?? []), m]);
	}
	return [...by]
		.sort(([a], [b]) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)))
		.map(([ns, ms]) => ({
			namespace: ns,
			title: ns === '' ? 'Standard' : humanize(ns),
			metrics: ms.sort((a, b) => metricLabel(a.key).localeCompare(metricLabel(b.key)))
		}));
}

/** The local-midnight ISO of a value stamped at 00:00 UTC (a day's value), else null. */
export function metricDay(ts: string): string | null {
	const d = new Date(ts);
	if (Number.isNaN(d.getTime()) || d.getTime() % 86_400_000 !== 0) return null;
	return new Date(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate()).toISOString();
}

/** An SVG sparkline path over a 100×30 box, '' below two points. */
export function sparkPath(values: number[]): string {
	if (values.length < 2) return '';
	const min = Math.min(...values);
	const span = Math.max(...values) - min || 1;
	return values
		.map((v, i) => {
			const x = (i / (values.length - 1)) * 100;
			const y = 30 - ((v - min) / span) * 28 - 1;
			return `${i === 0 ? 'M' : 'L'}${x.toFixed(1)},${y.toFixed(1)}`;
		})
		.join(' ');
}
