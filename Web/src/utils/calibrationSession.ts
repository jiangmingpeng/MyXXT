/**
 * 圆心校准会话的本地持久化（localStorage）。
 *
 * 为什么需要落盘：手机上「拍照 / 相册」导入二维码时，系统相机、原生 WebView
 * 或 iOS Safari 都可能把当前页面挂到后台甚至回收，回来时页面被重新加载，
 * 内存里的 ref 全部丢失。而刷新码每探测一次就要一张新码，用户已经拍了两张、
 * 只差一个探测点，重载后从第一次重新开始非常伤（每次都要再拍一张新码）。
 *
 * 落盘内容只包含「本轮固定的 3 个探测点 + 已拿到的距离」，圆心由
 * trilateration 重新解算即可，不需要存结果；活动 id 变化或超过有效期就丢弃，
 * 避免把上一场活动的距离套用到别的签到圆心上。
 */

import config from '../../config.yaml';
import type { LngLat } from './coord';
import type { DistanceSample } from './trilateration';

/** 本轮固定的一个探测点 */
export interface CalibrationProbe {
  label: string;
  /** 探测点绝对坐标（GCJ-02）；samples 里存的距离就是到老师圆心的米数 */
  point: LngLat;
}

/**
 * 圆心校准会话：跨面板开关、跨页面重载保留。
 */
export interface CalibrationSession {
  /** 已经拿到距离的探测点（顺序必须与 probes 的前缀一致） */
  samples: DistanceSample[];
  /** 本轮固定的三个探测点（顺序已打乱）。samples.length 就是下一个要探测的下标 */
  probes: CalibrationProbe[];
  /** 本次选点已经校准过的轮次（每轮 3 次真实提交），用于限制总请求量 */
  runs: number;
}

const STORAGE_KEY = 'sign_calibration_session';
const SCHEMA_VERSION = 1;
/** 三边定位至少要三个不共线的点 */
const PROBE_COUNT = 3;
/** 两个坐标相差小于该值视为同一个点（约 0.1 米） */
const COORD_EPSILON = 1e-6;
const DEFAULT_TTL_MINUTES = 120;

const readTtlMs = (): number => {
  const raw = Number((config.sign as { calibration_session_ttl_minutes?: unknown })?.calibration_session_ttl_minutes);
  const minutes = Number.isFinite(raw) && raw > 0 ? raw : DEFAULT_TTL_MINUTES;
  return minutes * 60 * 1000;
};

const isFiniteCoord = (value: unknown, limit: number): boolean =>
  typeof value === 'number' && Number.isFinite(value) && Math.abs(value) <= limit;

const isLngLat = (value: unknown): value is LngLat => {
  if (!value || typeof value !== 'object') return false;
  const point = value as { lng?: unknown; lat?: unknown };
  // 经纬度合法区间：纬度 ±90、经度 ±180
  return isFiniteCoord(point.lng, 180) && isFiniteCoord(point.lat, 90);
};

const parseProbes = (raw: unknown): CalibrationProbe[] | null => {
  if (!Array.isArray(raw) || raw.length !== PROBE_COUNT) return null;
  const probes: CalibrationProbe[] = [];
  for (const item of raw) {
    if (!item || typeof item !== 'object') return null;
    const { label, point } = item as { label?: unknown; point?: unknown };
    if (!isLngLat(point)) return null;
    probes.push({ label: typeof label === 'string' && label.trim() ? label : '探测点', point });
  }
  return probes;
};

/**
 * 采样点必须严格对应 probes 的前缀：坐标一致、距离为正数。
 * 这样即使 localStorage 被手工改过，也不会拿错位的距离去解算出一个虚假圆心。
 */
const parseSamples = (raw: unknown, probes: CalibrationProbe[]): DistanceSample[] | null => {
  if (!Array.isArray(raw)) return null;
  if (raw.length > probes.length) return null;
  const samples: DistanceSample[] = [];
  for (let i = 0; i < raw.length; i += 1) {
    const item = raw[i];
    if (!item || typeof item !== 'object') return null;
    const { lng, lat, distance } = item as { lng?: unknown; lat?: unknown; distance?: unknown };
    if (!isLngLat({ lng, lat })) return null;
    if (typeof distance !== 'number' || !Number.isFinite(distance) || distance <= 0) return null;
    const probe = probes[i].point;
    if (Math.abs(probe.lng - (lng as number)) > COORD_EPSILON) return null;
    if (Math.abs(probe.lat - (lat as number)) > COORD_EPSILON) return null;
    samples.push({ lng: lng as number, lat: lat as number, distance });
  }
  return samples;
};

/** 缓存不可用时（隐私模式 / 存储被禁用）静默降级为“只有内存会话” */
const readRaw = (): string | null => {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
};

export const clearCalibrationSession = (): void => {
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // 忽略：读不出来也不影响正常运行
  }
};

/**
 * 读取某场活动未完成的校准会话。
 * 活动不匹配、已过期、结构损坏时一律返回 null，并顺手清掉脏数据。
 */
export const loadCalibrationSession = (activityKey: string): CalibrationSession | null => {
  const key = String(activityKey || '').trim();
  if (!key) return null;

  const raw = readRaw();
  if (!raw) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    clearCalibrationSession();
    return null;
  }

  if (!parsed || typeof parsed !== 'object') {
    clearCalibrationSession();
    return null;
  }

  const stored = parsed as {
    v?: unknown;
    activityKey?: unknown;
    savedAt?: unknown;
    runs?: unknown;
    probes?: unknown;
    samples?: unknown;
  };

  if (stored.v !== SCHEMA_VERSION || stored.activityKey !== key) {
    // 结构版本不兼容，或属于另一场活动：直接丢弃，避免串用别人的圆心
    clearCalibrationSession();
    return null;
  }

  const savedAt = typeof stored.savedAt === 'number' ? stored.savedAt : 0;
  const age = Date.now() - savedAt;
  if (!Number.isFinite(age) || age < 0 || age > readTtlMs()) {
    clearCalibrationSession();
    return null;
  }

  const probes = parseProbes(stored.probes);
  if (!probes) {
    clearCalibrationSession();
    return null;
  }
  const samples = parseSamples(stored.samples, probes);
  if (!samples) {
    clearCalibrationSession();
    return null;
  }

  const rawRuns = typeof stored.runs === 'number' && Number.isFinite(stored.runs) ? stored.runs : 0;
  return { samples, probes, runs: Math.max(0, Math.floor(rawRuns)) };
};

/** 保存（或被之后的每一步覆盖）当前校准会话 */
export const saveCalibrationSession = (activityKey: string, session: CalibrationSession | null): void => {
  const key = String(activityKey || '').trim();
  if (!key) return;

  if (!session || session.samples.length === 0) {
    // 还没拿到任何距离就不用落盘：重载后从零开始即可，也不会误显示“继续校准”
    clearCalibrationSession();
    return;
  }

  try {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        v: SCHEMA_VERSION,
        activityKey: key,
        savedAt: Date.now(),
        runs: session.runs,
        probes: session.probes,
        samples: session.samples,
      })
    );
  } catch {
    // 配额满 / 存储被禁用：退化成只有内存会话，不影响本次校准
  }
};

/**
 * 是否存在「可以接着跑」的校准会话（至少已经拿到一个探测距离）。
 * 扫码页用它决定要不要强制弹出地图面板，避免重载后直接拿未校准的点提交。
 */
export const hasResumableCalibration = (activityKey: string): boolean => {
  const session = loadCalibrationSession(activityKey);
  return !!session && session.samples.length > 0;
};
