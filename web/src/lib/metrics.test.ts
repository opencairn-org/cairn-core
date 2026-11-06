import { describe, expect, it, vi } from 'vitest';

vi.mock('#lib/paraglide/runtime.js', () => ({ getLocale: () => 'en' }));

import {
	formatMetricValue,
	groupMetrics,
	humanize,
	metricLabel,
	metricDay,
	metricNamespace,
	sparkPath,
	structEntries,
	type StoredMetric
} from './metrics';

describe('metricLabel', () => {
	it.each([
		['spo2_pct', 'SpO₂'],
		['hrv_rmssd_ms', 'HRV RMSSD'],
		['vo2max_ml_kg_min', 'VO₂max'],
		['threshold_pace_running_s_per_km', 'Threshold pace running'],
		['css_swimming_s_per_100m', 'CSS swimming'],
		['oura.sleep_latency_min', 'Sleep latency'],
		['oura.sleep_avg_breath_per_min', 'Sleep avg breath'],
		['polar.breathing_rate_brpm', 'Breathing rate'],
		['whoop.recovery_score', 'Recovery score'],
		['oura.average_met', 'Average MET'],
		['tss', 'TSS'],
		['sleep_stages', 'Sleep stages'],
		['_min', 'Min']
	])('%s → %s', (key, want) => {
		expect(metricLabel(key)).toBe(want);
	});
});

describe('formatMetricValue', () => {
	it.each([
		['spo2_pct', 96.44, '96.4 %'],
		['hrv_rmssd_ms', 41.6, '42 ms'],
		['resting_hr_bpm', 52, '52 bpm'],
		['weight_kg', 72.35, '72.4 kg'],
		['w_prime_kj', 18.25, '18.3 kJ'],
		['calories_burned_kcal', 2412.4, '2,412 kcal'],
		['blood_pressure_systolic_mmhg', 121, '121 mmHg'],
		['vo2max_ml_kg_min', 51.26, '51.3 ml/kg/min'],
		['oura.sleep_avg_breath_per_min', 14.25, '14.3 br/min'],
		['polar.breathing_rate_brpm', 13, '13 br/min'],
		['withings.pulse_wave_velocity_mps', 7.123, '7.12 m/s'],
		['sleep_deep_min', 45, '45 min'],
		['sleep_total_min', 452, '7 h 32 min'],
		['threshold_pace_running_s_per_km', 285, '4:45 /km'],
		['css_swimming_s_per_100m', 98.6, '1:39 /100m'],
		['oura.temperature_deviation_c', -0.24, '-0.2 °C'],
		['withings.distance_m', 8421, '8.42 km'],
		['whoop.height_m', 1.8, '1.8 m'],
		['ftp_cycling_w', 251.4, '251 W'],
		['steps', 12034, '12,034'],
		['whoop.strain', 12.345, '12.35']
	])('%s %d → %s', (key, v, want) => {
		expect(formatMetricValue(key, v)).toBe(want);
	});
});

describe('metricNamespace and humanize', () => {
	it('splits the provider off', () => {
		expect(metricNamespace('whoop.strain')).toBe('whoop');
		expect(metricNamespace('spo2_pct')).toBe('');
	});
	it('keeps acronyms and drops empty words', () => {
		expect(humanize('ans__charge_status')).toBe('ANS charge status');
		expect(humanize('')).toBe('');
		expect(humanize('total_rem_sleep')).toBe('Total REM sleep');
	});
});

describe('metricDay', () => {
	it('keeps the UTC calendar day of a day-stamped value in local time', () => {
		const day = new Date(metricDay('2026-11-06T00:00:00Z')!);
		expect([day.getFullYear(), day.getMonth(), day.getDate(), day.getHours()]).toEqual([
			2026, 10, 6, 0
		]);
	});
	it('is null for other instants', () => {
		expect(metricDay('2026-11-06T07:30:00Z')).toBeNull();
		expect(metricDay('2026-11-06T00:00:00.5Z')).toBeNull();
		expect(metricDay('nope')).toBeNull();
	});
});

describe('structEntries', () => {
	it('sums sleep stages per stage in first-seen order', () => {
		const v = {
			stages: [
				{ start: '2026-11-05T22:00:00Z', end: '2026-11-05T23:30:00Z', stage: 'light' },
				{ start: '2026-11-05T23:30:00Z', end: '2026-11-06T00:15:00Z', stage: 'deep' },
				{ start: '2026-11-06T00:15:00Z', end: '2026-11-06T00:45:00Z', stage: 'light' },
				{ start: 'bad', end: '2026-11-06T01:00:00Z', stage: 'rem' },
				{ start: '2026-11-06T01:00:00Z', end: '2026-11-06T00:00:00Z', stage: 'awake' },
				{ start: '2026-11-06T01:00:00Z', end: '2026-11-06T01:10:00Z', stage: 3 },
				null
			]
		};
		expect(structEntries(v)).toEqual([
			['Light', '2 h 0 min'],
			['Deep', '45 min']
		]);
	});

	it('lists fields with one nested level', () => {
		const v = {
			level: 'solid',
			score: 82,
			recovered: true,
			naps: [1, 2],
			contributors: { sleep_recovery: 80.5, note: 'ok', deeper: { x: 1 } },
			missing: null
		};
		expect(structEntries(v)).toEqual([
			['Level', 'solid'],
			['Score', '82'],
			['Recovered', 'yes'],
			['Naps', '2 entries'],
			['Contributors · sleep recovery', '80.5'],
			['Contributors · note', 'ok']
		]);
		expect(structEntries({ off: false })).toEqual([['Off', 'no']]);
	});

	it('formats nested values by their own key', () => {
		expect(structEntries({ stage_summary: { total_in_bed_min: 480 } })).toEqual([
			['Stage summary · total in bed min', '8 h 0 min']
		]);
	});

	it('caps the entries', () => {
		const v = Object.fromEntries(Array.from({ length: 20 }, (_, i) => [`k${i}`, i]));
		expect(structEntries(v)).toHaveLength(12);
	});

	it('shows a bare scalar and ignores nothing', () => {
		expect(structEntries('restored')).toEqual([['', 'restored']]);
		expect(structEntries(null)).toEqual([]);
		expect(structEntries(undefined)).toEqual([]);
		expect(structEntries([1, 2, 3])).toEqual([['', '3 entries']]);
		expect(structEntries(Symbol.for('x'))).toEqual([]);
	});
});

const m = (key: string): StoredMetric => ({
	id: key,
	key,
	ts: '2026-11-06T00:00:00Z',
	value_numeric: 1,
	value_struct: null,
	provider: 'p',
	external_id: null,
	source_worker_name: 'w',
	source_worker_version: '1'
});

describe('groupMetrics', () => {
	it('puts reserved keys first, providers alphabetically, labels sorted', () => {
		const groups = groupMetrics([
			m('whoop.strain'),
			m('spo2_pct'),
			m('oura.readiness_score'),
			m('body_fat_pct'),
			m('whoop.recovery_score')
		]);
		expect(groups.map((g) => [g.title, g.metrics.map((x) => x.key)])).toEqual([
			['Standard', ['body_fat_pct', 'spo2_pct']],
			['Oura', ['oura.readiness_score']],
			['Whoop', ['whoop.recovery_score', 'whoop.strain']]
		]);
	});

	it('sorts a reserved group that comes later', () => {
		expect(groupMetrics([m('oura.a'), m('steps')]).map((g) => g.namespace)).toEqual(['', 'oura']);
		expect(groupMetrics([])).toEqual([]);
	});
});

describe('sparkPath', () => {
	it('needs two points', () => {
		expect(sparkPath([])).toBe('');
		expect(sparkPath([5])).toBe('');
	});
	it('scales into the box, flat series centred low', () => {
		expect(sparkPath([0, 10])).toBe('M0.0,29.0 L100.0,1.0');
		expect(sparkPath([3, 3, 3])).toBe('M0.0,29.0 L50.0,29.0 L100.0,29.0');
	});
});
