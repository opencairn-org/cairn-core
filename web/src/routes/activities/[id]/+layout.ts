import { error, redirect } from '@sveltejs/kit';
import { connectClients, isUnauthenticated, Code, ConnectError } from '#lib/connect.js';
import {
	type Activity,
	type ActivityStream,
	type BestEffort,
	ActivityType,
	ActivitySourceStatus,
	GearSetBy,
	BestEffortMetric,
	BestEffortWindowKind,
	Discipline,
	ReimportStatus,
	StreamChannel
} from '#lib/proto/cairn/v1/activity_pb.js';
import { enumName, protoDurationToSeconds, protoTimestampToISO } from '#lib/proto-helpers.js';
import { gearToView, type GearView } from '#lib/gear.js';
import type { LayoutLoad } from './$types';

export const load: LayoutLoad = async ({ fetch, url, params, parent }) => {
	const { user } = await parent();
	if (!user) {
		redirect(302, '/login');
	}

	const clients = connectClients(fetch, url.origin);

	let activity: Activity | undefined;
	try {
		const res = await clients.activity.getActivity({ id: params.id });
		activity = res.activity;
	} catch (err) {
		if (isUnauthenticated(err)) {
			redirect(302, '/login');
		}
		if (err instanceof ConnectError && err.code === Code.NotFound) {
			error(404, 'Activity not found');
		}
		error(500, (err as Error).message);
	}

	if (!activity) {
		error(404, 'Activity not found');
	}

	// Streams and best-efforts are independent — run them in parallel.
	const a = activity;
	const [streamResult, bestEfforts, gear] = await Promise.all([
		loadStream(clients, a),
		clients.activity
			.listBestEfforts({ activityId: a.id })
			.then((res) => res.efforts.map(bestEffortToView))
			.catch((err) => {
				console.warn('ListBestEfforts failed', err);
				return [] as BestEffortView[];
			}),
		// For the "gear not set" prompt; failure just hides it.
		clients.gear
			.listGear({ includeRetired: false })
			.then((res) => res.gear.map(gearToView))
			.catch(() => [] as GearView[])
	]);

	// Per-channel merged stream (best field from best source) — only worth
	// fetching when the activity has more than one source. Used by the charts;
	// the map stays on the single primary stream.
	const merged =
		a.sources.length > 1
			? await loadMergedStream(fetch, url.origin, a)
			: { stream: null, provenance: [] };

	return {
		activity: activityToView(a),
		stream: streamResult.stream,
		mergedStream: merged.stream,
		mergedProvenance: merged.provenance,
		// streamStatus distinguishes the blank states so the page can explain
		// itself instead of silently dropping the map/streams/elevation:
		//   'ok'     — stream loaded with samples
		//   'empty'  — primary source exists but carries no samples yet
		//   'absent' — no primary stream source at all (nothing to load)
		//   'error'  — the stream request failed (offer a retry)
		streamStatus: streamResult.status,
		bestEfforts,
		gear
	};
};

type StreamStatus = 'ok' | 'empty' | 'absent' | 'error';

async function loadStream(
	clients: ReturnType<typeof connectClients>,
	a: Activity
): Promise<{ stream: ActivityStreamView | null; status: StreamStatus }> {
	if (!a.primaryStreamSourceId) {
		return { stream: null, status: 'absent' };
	}
	try {
		const res = await clients.activity.getActivityStream({
			activitySourceId: a.primaryStreamSourceId,
			maxResolutionHz: 0.2
		});
		const view = streamToView(res.stream);
		if (!view || view.sampleCount === 0) {
			return { stream: null, status: 'empty' };
		}
		return { stream: view, status: 'ok' };
	} catch (err) {
		console.warn('GetActivityStream failed', err);
		return { stream: null, status: 'error' };
	}
}

// ChannelProvenance: which provider supplied a rendered stream channel.
export interface ChannelProvenance {
	channel: string;
	provider: string;
}

// loadMergedStream fetches the per-channel-merged, aligned stream
// (GET /api/activities/{id}/merged-stream) and projects it into the same
// ActivityStreamView the charts consume. Returns a null stream on any failure
// (the caller falls back to the single primary stream). Map fields
// (coordinates/track) are left empty — the map renders from the primary source.
async function loadMergedStream(
	fetch: typeof globalThis.fetch,
	origin: string,
	a: Activity
): Promise<{ stream: ActivityStreamView | null; provenance: ChannelProvenance[] }> {
	try {
		const res = await fetch(`${origin}/api/activities/${a.id}/merged-stream`);
		if (!res.ok) return { stream: null, provenance: [] };
		const b = (await res.json()) as {
			grid: number[];
			channels: Record<string, (number | null)[]>;
			provenance: Record<string, string>;
		};
		const grid = b.grid ?? [];
		const ch = b.channels ?? {};
		const base = grid[0];
		if (base === undefined) return { stream: null, provenance: [] };
		const offsets = grid.map((t) => t - base);
		const pick = (name: string) => (ch[name] && ch[name].length ? ch[name] : null);
		const hr = pick('heart_rate');
		const power = pick('power');
		const cadence = pick('cadence');
		const speed = pick('speed');
		const altitude = pick('altitude');
		if (!hr && !power && !cadence && !speed && !altitude) {
			return { stream: null, provenance: [] };
		}

		const stream: ActivityStreamView = {
			sourceID: '',
			resolutionHz: 0,
			sampleCount: grid.length,
			coordinates: [],
			offsets,
			hr,
			power,
			cadence,
			speed,
			altitude,
			track: []
		};

		// Map source ids → provider labels for the "merged from" caption.
		const providerBySource = new Map(a.sources.map((s) => [s.id, s.provider]));
		const provenance: ChannelProvenance[] = Object.entries(b.provenance ?? {})
			.map(([channel, sid]) => ({ channel, provider: providerBySource.get(sid) ?? 'unknown' }))
			.sort((x, y) => x.channel.localeCompare(y.channel));

		return { stream, provenance };
	} catch (err) {
		console.warn('merged-stream load failed', err);
		return { stream: null, provenance: [] };
	}
}

// ---------------------------------------------------------------------------
// View types — what the .svelte file consumes. Projecting here keeps the
// component free of bigint timestamps and @bufbuild/protobuf machinery.
// ---------------------------------------------------------------------------

export interface ActivityView {
	id: string;
	title: string;
	description: string;
	type: string;
	discipline: string;
	isVirtual: boolean;
	isEbike: boolean;
	isCommute: boolean;
	isRace: boolean;
	customSubtype: string;
	startTime: string;
	timezone: string;
	movingDurationS: number;
	elapsedDurationS: number;
	primaryStreamSourceId: string | null;
	startPlace: string;
	gearId: string;
	gearName: string;
	gearSetBy: string;
	sources: SourceView[];
	summary: {
		distanceM: number | null;
		elevationGainM: number | null;
		elevationLossM: number | null;
		minElevationM: number | null;
		maxElevationM: number | null;
		avgSpeedMps: number | null;
		maxSpeedMps: number | null;
		avgHeartRateBpm: number | null;
		maxHeartRateBpm: number | null;
		avgPowerW: number | null;
		maxPowerW: number | null;
		normalizedPowerW: number | null;
		avgCadence: number | null;
		maxCadence: number | null;
		avgTemperatureC: number | null;
		caloriesKcal: number | null;
		tss: number | null;
		intensityFactor: number | null;
	};
	hasStream: boolean;
}

export interface SourceView {
	id: string;
	provider: string;
	externalId: string;
	status: string; // enum name, e.g. "active" | "detached"
	statusReason: string;
	reimportStatus: string;
	importedAt: string;
	isPrimary: boolean;
	hasRawBlob: boolean;
}

export interface ActivityStreamView {
	sourceID: string;
	resolutionHz: number;
	sampleCount: number;
	coordinates: [number, number][];
	offsets: number[];
	hr: (number | null)[] | null;
	power: (number | null)[] | null;
	cadence: (number | null)[] | null;
	speed: (number | null)[] | null;
	altitude: (number | null)[] | null;
	// One entry per GPS-bearing sample, index-aligned with `coordinates`. Carries
	// the time offset + per-point values so the map can snap to the route, show a
	// value overlay, and sync a hover position with the streams chart.
	track: TrackPoint[];
}

// TrackPoint is a single GPS vertex with its time offset and the channel values
// at that point. `t` (seconds offset) is the shared key the map and chart hover
// on. Null = channel not present / missing at this sample.
export interface TrackPoint {
	t: number;
	lon: number;
	lat: number;
	distM: number | null;
	eleM: number | null;
	speedKmh: number | null;
	hr: number | null;
	power: number | null;
	cadence: number | null;
}

export interface BestEffortView {
	id: string;
	metric: string;
	windowKind: string;
	windowValue: number;
	achievedValue: number;
	distanceM?: number | undefined;
	durationS: number;
	startOffsetS: number;
}

function activityToView(a: Activity): ActivityView {
	return {
		id: a.id,
		title: a.title,
		description: a.description,
		type: enumName(ActivityType, a.type),
		discipline: enumName(Discipline, a.discipline),
		isVirtual: a.isVirtual,
		isEbike: a.isEbike,
		isCommute: a.isCommute,
		isRace: a.isRace,
		customSubtype: a.customSubtype,
		startTime: protoTimestampToISO(a.startTime),
		timezone: a.timezone,
		movingDurationS: protoDurationToSeconds(a.movingDuration),
		elapsedDurationS: protoDurationToSeconds(a.elapsedDuration),
		primaryStreamSourceId: a.primaryStreamSourceId || null,
		startPlace: a.startPlace,
		gearId: a.gearId,
		gearName: a.gearName,
		gearSetBy: enumName(GearSetBy, a.gearSetBy) || 'merge',
		sources: a.sources.map((s) => ({
			id: s.id,
			provider: s.provider,
			externalId: s.externalId,
			status: enumName(ActivitySourceStatus, s.status),
			statusReason: s.statusReason,
			reimportStatus: enumName(ReimportStatus, s.reimportStatus),
			importedAt: protoTimestampToISO(s.importedAt),
			isPrimary: !!a.primaryStreamSourceId && s.id === a.primaryStreamSourceId,
			hasRawBlob: !!s.rawBlobId
		})),
		summary: {
			distanceM: a.summary?.distanceM ?? null,
			elevationGainM: a.summary?.elevationGainM ?? null,
			elevationLossM: a.summary?.elevationLossM ?? null,
			minElevationM: a.summary?.minElevationM ?? null,
			maxElevationM: a.summary?.maxElevationM ?? null,
			avgSpeedMps: a.summary?.avgSpeedMps ?? null,
			maxSpeedMps: a.summary?.maxSpeedMps ?? null,
			avgHeartRateBpm: a.summary?.avgHeartRateBpm ?? null,
			maxHeartRateBpm: a.summary?.maxHeartRateBpm ?? null,
			avgPowerW: a.summary?.avgPowerW ?? null,
			maxPowerW: a.summary?.maxPowerW ?? null,
			normalizedPowerW: a.summary?.normalizedPowerW ?? null,
			avgCadence: a.summary?.avgCadence ?? null,
			maxCadence: a.summary?.maxCadence ?? null,
			avgTemperatureC: a.summary?.avgTemperatureC ?? null,
			caloriesKcal: a.summary?.caloriesKcal ?? null,
			tss: a.summary?.tss ?? null,
			intensityFactor: a.summary?.intensityFactor ?? null
		},
		hasStream: !!a.primaryStreamSourceId
	};
}

function streamToView(s: ActivityStream | undefined): ActivityStreamView | null {
	if (!s) return null;
	const channels = new Set<StreamChannel>(s.channels);
	const hasGPS = channels.has(StreamChannel.LATITUDE) && channels.has(StreamChannel.LONGITUDE);
	const hasDist = channels.has(StreamChannel.DISTANCE);
	const hasEle = channels.has(StreamChannel.ALTITUDE);
	const hasSpeed = channels.has(StreamChannel.SPEED);
	const hasHR = channels.has(StreamChannel.HEART_RATE);
	const hasPower = channels.has(StreamChannel.POWER);
	const hasCad = channels.has(StreamChannel.CADENCE);

	const coordinates: [number, number][] = [];
	const track: TrackPoint[] = [];
	// floats use NaN for missing (but 0 is valid, e.g. distance at start);
	// ints use 0 for missing.
	// Out-of-range samples count as missing too.
	const nf = (v: number | undefined) => (v === undefined || Number.isNaN(v) ? null : v);
	const nz = (v: number | undefined) => (v === undefined || v === 0 ? null : v);
	if (hasGPS) {
		for (let i = 0; i < s.sampleCount; i++) {
			const lat = s.latitude[i];
			const lon = s.longitude[i];
			const t = s.timeS[i];
			if (lat === undefined || lon === undefined || t === undefined) continue;
			if (!Number.isFinite(lat) || !Number.isFinite(lon)) continue;
			coordinates.push([lon, lat]);
			const speedMps = hasSpeed ? nf(s.speedMps[i]) : null;
			track.push({
				t,
				lon,
				lat,
				distM: hasDist ? nf(s.distanceM[i]) : null,
				eleM: hasEle ? nf(s.altitudeM[i]) : null,
				speedKmh: speedMps == null ? null : Math.round(speedMps * 3.6 * 10) / 10,
				hr: hasHR ? nz(s.heartRateBpm[i]) : null,
				power: hasPower ? nz(s.powerW[i]) : null,
				cadence: hasCad ? nz(s.cadence[i]) : null
			});
		}
	}
	return {
		sourceID: s.activitySourceId,
		resolutionHz: s.resolutionHz,
		sampleCount: s.sampleCount,
		coordinates,
		track,
		offsets: s.timeS,
		hr: channels.has(StreamChannel.HEART_RATE) ? intColToNullable(s.heartRateBpm) : null,
		power: channels.has(StreamChannel.POWER) ? intColToNullable(s.powerW) : null,
		cadence: channels.has(StreamChannel.CADENCE) ? intColToNullable(s.cadence) : null,
		speed: channels.has(StreamChannel.SPEED) ? floatColToNullable(s.speedMps) : null,
		altitude: channels.has(StreamChannel.ALTITUDE) ? floatColToNullable(s.altitudeM) : null
	};
}

function bestEffortToView(b: BestEffort): BestEffortView {
	return {
		id: b.id,
		metric: enumName(BestEffortMetric, b.metric),
		windowKind: enumName(BestEffortWindowKind, b.windowKind),
		windowValue: b.windowValue,
		achievedValue: b.achievedValue,
		distanceM: b.distanceM,
		durationS: protoDurationToSeconds(b.duration),
		startOffsetS: b.startOffset
	};
}

// In the proto column-oriented stream format int columns use 0 for
// "missing" (repeated int32 has no in-band null). For chart rendering we
// want real nulls so uPlot draws gaps; map 0 → null.
function intColToNullable(col: number[]): (number | null)[] {
	return col.map((v) => (v === 0 ? null : v));
}

// Float columns use NaN for missing.
function floatColToNullable(col: number[]): (number | null)[] {
	return col.map((v) => (Number.isNaN(v) ? null : v));
}
