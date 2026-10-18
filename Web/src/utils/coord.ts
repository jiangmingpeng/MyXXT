/**
 * 坐标系转换工具。
 *
 * 浏览器 Geolocation API 返回的是 WGS-84（GPS 原始坐标），
 * 而高德 / 腾讯地图以及学习通的位置校验使用的都是 GCJ-02（火星坐标系）。
 * 两者在国内有几十到几百米的偏移，直接提交 WGS-84 会导致「500 米内」校验失败。
 *
 * 这里内联标准算法，避免再引入第三方坐标库。
 */

const PI = Math.PI;
const A = 6378245.0; // 克拉索夫斯基椭球长半轴
const EE = 0.006693421622965943; // 偏心率平方（与原算法常量等价的双精度最短表示）

export interface LngLat {
  lng: number;
  lat: number;
}

/** 是否在中国境外（境外不做偏移） */
function outOfChina(lng: number, lat: number): boolean {
  return lng < 72.004 || lng > 137.8347 || lat < 0.8293 || lat > 55.8271;
}

function transformLat(x: number, y: number): number {
  let ret =
    -100.0 + 2.0 * x + 3.0 * y + 0.2 * y * y + 0.1 * x * y + 0.2 * Math.sqrt(Math.abs(x));
  ret += ((20.0 * Math.sin(6.0 * x * PI) + 20.0 * Math.sin(2.0 * x * PI)) * 2.0) / 3.0;
  ret += ((20.0 * Math.sin(y * PI) + 40.0 * Math.sin((y / 3.0) * PI)) * 2.0) / 3.0;
  ret += ((160.0 * Math.sin((y / 12.0) * PI) + 320 * Math.sin((y * PI) / 30.0)) * 2.0) / 3.0;
  return ret;
}

function transformLng(x: number, y: number): number {
  let ret = 300.0 + x + 2.0 * y + 0.1 * x * x + 0.1 * x * y + 0.1 * Math.sqrt(Math.abs(x));
  ret += ((20.0 * Math.sin(6.0 * x * PI) + 20.0 * Math.sin(2.0 * x * PI)) * 2.0) / 3.0;
  ret += ((20.0 * Math.sin(x * PI) + 40.0 * Math.sin((x / 3.0) * PI)) * 2.0) / 3.0;
  ret += ((150.0 * Math.sin((x / 12.0) * PI) + 300.0 * Math.sin((x / 30.0) * PI)) * 2.0) / 3.0;
  return ret;
}

/**
 * WGS-84 -> GCJ-02。
 * 用于把浏览器定位结果转换成与高德地图 / 学习通一致的坐标。
 */
export function wgs84ToGcj02(lng: number, lat: number): LngLat {
  if (!Number.isFinite(lng) || !Number.isFinite(lat) || outOfChina(lng, lat)) {
    return { lng, lat };
  }

  const radLat = (lat / 180.0) * PI;
  let magic = Math.sin(radLat);
  magic = 1 - EE * magic * magic;
  const sqrtMagic = Math.sqrt(magic);

  const dLat =
    (transformLat(lng - 105.0, lat - 35.0) * 180.0) /
    (((A * (1 - EE)) / (magic * sqrtMagic)) * PI);
  const dLng =
    (transformLng(lng - 105.0, lat - 35.0) * 180.0) /
    ((A / sqrtMagic) * Math.cos(radLat) * PI);

  return { lng: lng + dLng, lat: lat + dLat };
}

/**
 * 坐标格式化为字符串：保留 7 位小数（约 1cm 精度），
 * 去掉浮点尾巴的同时不损失可用精度。
 */
export function formatCoord(value: number): string {
  if (!Number.isFinite(value)) return '';
  return value.toFixed(7);
}
