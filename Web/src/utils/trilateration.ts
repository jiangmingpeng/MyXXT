/**
 * 三边定位（trilateration）：由若干「已知点 + 该点到目标的距离」反解目标坐标。
 *
 * 为什么需要它：学习通的位置签到只下发地址文案，圆心经纬度是空串，
 * 但每次提交超出范围都会返回 errorLocation2_xxx（本次提交点距真实圆心多少米）。
 * 这个距离是唯一的精确标尺，于是用三个不共线的探测点各探测一次，就能把
 * 老师的真实圆心解出来；解出来的点再用一次正常提交即可签到成功。
 *
 * 距离与坐标系无关（同一个圆在 GCJ-02 / WGS-84 下半径不同，但“点到圆心的距离”
 * 是同一个物理量），所以这里只需要保证所有点在同一坐标系内（本项目统一 GCJ-02）。
 */

import { distanceMeters } from './coord';
import type { LngLat } from './coord';

/**
 * 每纬度对应的米数。取与 coord.distanceMeters（Haversine，R=6371008.8m）一致的
 * 球面常数，避免投影尺度和测距尺度不一致带来的系统误差（几百米量级时约 1 米）。
 */
const METERS_PER_DEG_LAT = (6371008.8 * Math.PI) / 180;

export interface DistanceSample extends LngLat {
  /** 该点到真实签到圆心的距离（米），来自学习通位置校验返回 */
  distance: number;
}

export interface TrilaterationResult {
  center: LngLat;
  /** 用解出的圆心回算各样本距离的最大残差（米），越大说明探测数据越不可信 */
  maxResidual: number;
  /** 参与解算的样本数 */
  sampleCount: number;
}

/** 把 (east, north) 米的偏移量加到经纬度上（等距圆柱近似，几百米内足够精确） */
export const offsetMeters = (base: LngLat, east: number, north: number): LngLat => {
  const cosLat = Math.cos((base.lat * Math.PI) / 180) || 1;
  return {
    lng: base.lng + east / (METERS_PER_DEG_LAT * cosLat),
    lat: base.lat + north / METERS_PER_DEG_LAT,
  };
};

/**
 * 解算圆心。
 *
 * 以第一个样本为原点投影到局部平面，第 i 个样本满足
 *   (X - xi)² + (Y - yi)² = di²
 * 与第 0 个样本相减，二次项被消掉，得到线性方程
 *   2(xi - x0)·X + 2(yi - y0)·Y = d0² - di² + xi² - x0² + yi² - y0²
 * 用全部样本做最小二乘（正规方程，2x2 直接求逆），对测距误差不敏感。
 *
 * 三点近共线时系数矩阵奇异，返回 null（调用方应换一组探测点）。
 */
export const solveSignCenter = (samples: DistanceSample[]): TrilaterationResult | null => {
  const usable = samples.filter(
    (s) =>
      Number.isFinite(s.lng) &&
      Number.isFinite(s.lat) &&
      Number.isFinite(s.distance) &&
      s.distance > 0
  );
  if (usable.length < 3) return null;

  const origin = usable[0];
  const cosLat = Math.cos((origin.lat * Math.PI) / 180) || 1;
  const project = (p: LngLat) => ({
    x: (p.lng - origin.lng) * METERS_PER_DEG_LAT * cosLat,
    y: (p.lat - origin.lat) * METERS_PER_DEG_LAT,
  });

  const points = usable.map((s) => ({ ...project(s), d: s.distance }));
  const base = points[0];

  let sxx = 0;
  let sxy = 0;
  let syy = 0;
  let sxr = 0;
  let syr = 0;

  for (let i = 1; i < points.length; i += 1) {
    const p = points[i];
    const a = 2 * (p.x - base.x);
    const b = 2 * (p.y - base.y);
    const r = base.d * base.d - p.d * p.d + p.x * p.x - base.x * base.x + p.y * p.y - base.y * base.y;
    sxx += a * a;
    sxy += a * b;
    syy += b * b;
    sxr += a * r;
    syr += b * r;
  }

  const det = sxx * syy - sxy * sxy;
  // 三点（或全部样本）几乎共线：方程无唯一解。
  if (!Number.isFinite(det) || Math.abs(det) < 1e-6) return null;

  const x = (sxr * syy - sxy * syr) / det;
  const y = (sxx * syr - sxy * sxr) / det;
  if (!Number.isFinite(x) || !Number.isFinite(y)) return null;

  const center: LngLat = {
    lng: origin.lng + x / (METERS_PER_DEG_LAT * cosLat),
    lat: origin.lat + y / METERS_PER_DEG_LAT,
  };

  // 残差校验：解出来的点必须同时满足所有“点到圆心距离”，
  // 否则说明某次探测的距离不属于本次圆心（例如老师中途改了位置）。
  let maxResidual = 0;
  usable.forEach((s) => {
    const diff = Math.abs(distanceMeters(s, center) - s.distance);
    if (Number.isFinite(diff) && diff > maxResidual) maxResidual = diff;
  });

  return { center, maxResidual, sampleCount: usable.length };
};
