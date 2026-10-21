import React, { useEffect, useRef, useState } from 'react';
import { AnimatePresence, motion } from 'framer-motion';
import { Compass, Info, LocateFixed, Loader2, MapPin, Search, Target, X } from 'lucide-react';
import toast from 'react-hot-toast';
import config from '../../../config.yaml';
import { distanceMeters, formatCoord, wgs84ToGcj02 } from '../../utils/coord';
import type { LngLat } from '../../utils/coord';
import { offsetMeters, solveSignCenter } from '../../utils/trilateration';
import type { DistanceSample } from '../../utils/trilateration';

const AMAP_KEY: string = config.sign?.amap?.js_key || '';
const AMAP_SECURITY_CODE: string = config.sign?.amap?.security_code || '';
const LOCATION_PRESETS: any[] = config.sign?.location_presets || [];

// 高德 JS API 2.0 安全密钥：必须在 SDK 脚本加载之前挂到 window 上。
// 放在模块顶层（而不是异步函数里），确保任何 import() / load() 之前都已生效。
if (AMAP_SECURITY_CODE) {
  window._AMapSecurityConfig = { securityJsCode: AMAP_SECURITY_CODE };
}

const AMAP_PLUGINS = ['AMap.Geocoder', 'AMap.AutoComplete', 'AMap.PlaceSearch'];

/**
 * 圆心校准每轮需要 3 次真实提交（三边定位至少要三个不共线的点）。
 * 单次选点最多校准 2 轮；正常共 6 次提交，某个探测点偶发无距离重试时最多 12 次，
 * 仍属于低频量级，避免触发学习通风控。
 */
const CALIBRATE_MAX_RUNS = 2;
/**
 * 单个探测点最多尝试 2 次：
 * 学习通偶尔返回不带距离的 errorLocationX（提交地址为空时更常见），
 * 这种响应解不出圆心，随机间隔后重试一次往往就能拿到 errorLocation2_<米>。
 */
const PROBE_MAX_ATTEMPTS = 2;

export interface MapPickerResult {
  name: string;
  lng: string;
  lat: string;
  description: string;
  /** 本次解析到的签到圆心（GCJ-02）。后端没下发经纬度时就是地址反查的结果。 */
  signLng?: number;
  signLat?: number;
}

interface MapPickerProps {
  open: boolean;
  initialLng?: string;
  initialLat?: string;
  initialName?: string;
  initialDescription?: string;
  /** 老师设置的签到圆心（GCJ-02），用于画半径圈和"用签到点"快捷定位 */
  signLng?: number | string;
  signLat?: number | string;
  /** 老师设置的签到半径（米） */
  signRange?: number;
  signAddress?: string;
  /** 当前活动是不是刷新二维码（enc 只有几秒有效期，校准必须每次等新码） */
  refreshQr?: boolean;
  /**
   * 圆心探测：真实提交一次坐标，拿回学习通返回的“距签到圆心距离（米）”。
   * 学习通不下发圆心经纬度，只有这个距离能精确反解圆心，所以由父页面
   * 提供（它持有当前二维码 enc 与活动信息）。不传则不显示校准入口。
   */
  onProbeDistance?: (point: LngLat) => Promise<SignCenterProbe>;
  onClose: () => void;
  onConfirm: (result: MapPickerResult) => void;
}

/** 一次圆心探测的结果 */
export interface SignCenterProbe {
  /** 本次提交已经签到成功（探测点恰好落在范围内），无需继续校准 */
  signed: boolean;
  /** 距老师真实圆心的距离（米）；为空表示这次没拿到距离 */
  distance: number | null;
  /**
   * 当前是不是刷新二维码（enc 只有几秒有效期）。
   * 为 true 时父页面会在每次探测前等一张新码，这里就不能再叠加长随机等待。
   */
  refreshQr?: boolean;
  /** 本次失败重试也没用（例如根本没扫到新码），不要再叠加一次探测 */
  retryable?: boolean;
  /**
   * 手上没有可用新码，需要用户再拍 / 导入一张二维码。
   * 与「失败」不同：已采集的探测距离有效，应由地图页保留进度、暂停本轮，等下一张码回来续传。
   */
  needQr?: boolean;
  message?: string;
}

/**
 * 圆心校准会话：跨面板开关保留。
 * HTTP 手机页面没有摄像头权限，只能用「拍照 / 相册」一张一张导入二维码；
 * 刷新码每探测一次都要一张新码，所以必须把已采集的采样点保留下来，
 * 拍下一张回来后接着上次进度继续，而不是从第一次探测重新开始。
 */
interface CalibrationSession {
  /** 已经拿到距离的探测点（绝对坐标 + 距圆心米数） */
  samples: DistanceSample[];
  /** 本轮固定的三个探测点（顺序已打乱）。samples.length 就是下一个要探测的下标。 */
  probes: { label: string; point: LngLat }[];
}

/** 兼容 AMap.LngLat 对象与 { lng, lat } 普通对象两种形态 */
const readLngLat = (loc: any): LngLat | null => {
  if (!loc) return null;
  if (typeof loc === 'string') {
    const [lng, lat] = loc.split(',').map(Number);
    if (Number.isFinite(lng) && Number.isFinite(lat)) return { lng, lat };
    return null;
  }
  const lng = typeof loc.getLng === 'function' ? loc.getLng() : loc.lng;
  const lat = typeof loc.getLat === 'function' ? loc.getLat() : loc.lat;
  if (typeof lng !== 'number' || typeof lat !== 'number') return null;
  return { lng, lat };
};

const safeNumber = (raw: string | undefined, fallback: number): number => {
  if (!raw) return fallback;
  const value = Number(raw);
  return Number.isFinite(value) ? value : fallback;
};

/** 把后端返回的 number/string 经纬度转成 LngLat；0 或非法值视为没有 */
const toLngLat = (lng?: number | string, lat?: number | string): LngLat | null => {
  const lngValue = Number(lng);
  const latValue = Number(lat);
  if (!Number.isFinite(lngValue) || !Number.isFinite(latValue)) return null;
  if (lngValue === 0 || latValue === 0) return null;
  return { lng: lngValue, lat: latValue };
};

/** 半径越大，初始缩放级别越小，保证整个签到圈都在视野内 */
const zoomForRange = (range: number): number => {
  if (!Number.isFinite(range) || range <= 0) return 16;
  if (range <= 200) return 18;
  if (range <= 500) return 17;
  if (range <= 1000) return 16;
  if (range <= 2000) return 15;
  return 14;
};

type SignGeocodeHit = { pos: LngLat; keyword: string; via: 'place' | 'geocode' };

/**
 * 把学习通下发的 locationText 拆成「整串地址」和「POI 名」。
 * 形如“湖南省…人民南路120号 吉首大学-图书馆”：前半段是格式化地址，
 * 最后一个空白之后是老师所在 POI 的名字。
 */
const splitSignAddress = (address: string) => {
  const parts = address.split(/\s+/).filter(Boolean);
  const poiName = parts.length > 1 ? parts[parts.length - 1] : '';
  // 城市用于限定 POI 搜索范围，避免“图书馆”这种名字搜到外地去
  const cityMatch = address.match(/([\u4e00-\u9fa5]{2,8}?(?:自治州|地区|市|县))/);
  return { poiName, city: cityMatch ? cityMatch[1] : '' };
};

/**
 * 学习通只下发 locationText，圆心经纬度字段是空串，这里用高德反查圆心。
 *
 * 顺序很关键：整串地址里的门牌号精度很低（一个大学校区常常都叫同一个门牌号），
 * 末尾的 POI 名（“吉首大学-图书馆”）才是老师真正所在的楼/馆，
 * 所以先按 POI 名做地点搜索，再退回整串地址做地理编码。
 */
const resolveSignCenterFromAddress = (
  AMap: any,
  address: string,
  done: (hit: SignGeocodeHit | null) => void
) => {
  const { poiName, city } = splitSignAddress(address);

  // 候选关键词：POI 名 → 去掉连字符/括号的 POI 名（“吉首大学-图书馆”常常搜不到）
  const poiKeywords: string[] = [];
  if (poiName) {
    poiKeywords.push(poiName);
    const plainPoi = poiName.replace(/[-—_·()（）]/g, '');
    if (plainPoi && plainPoi !== poiName) poiKeywords.push(plainPoi);
  }

  // 地点搜索（POI 精确）优先，地理编码（门牌号，粗）兜底
  const steps: { via: 'place' | 'geocode'; keyword: string }[] = [];
  poiKeywords.forEach((keyword) => steps.push({ via: 'place', keyword }));
  poiKeywords.forEach((keyword) => steps.push({ via: 'geocode', keyword }));
  steps.push({ via: 'geocode', keyword: address });

  let index = 0;
  let finished = false;
  const finish = (hit: SignGeocodeHit | null) => {
    if (finished) return;
    finished = true;
    done(hit);
  };

  // 逐级降级；单步超时兜底，避免网络/额度异常时一直卡在“正在反查地址”
  const runStep = () => {
    if (finished) return;
    if (index >= steps.length) {
      finish(null);
      return;
    }
    const step = steps[index++];
    let settled = false;
    const timer = window.setTimeout(() => {
      if (settled) return;
      settled = true;
      runStep();
    }, 4000);
    const accept = (pos: LngLat | null) => {
      if (settled) return;
      settled = true;
      window.clearTimeout(timer);
      if (pos) finish({ pos, keyword: step.keyword, via: step.via });
      else runStep();
    };

    try {
      if (step.via === 'place') {
        const search = new AMap.PlaceSearch({ pageSize: 1, city: city || undefined });
        search.search(step.keyword, (status: string, result: any) => {
          accept(status === 'complete' ? readLngLat(result?.poiList?.pois?.[0]?.location) : null);
        });
      } else {
        const geocoder = new AMap.Geocoder({ city: city || undefined });
        geocoder.getLocation(step.keyword, (status: string, result: any) => {
          accept(status === 'complete' ? readLngLat(result?.geocodes?.[0]?.location) : null);
        });
      }
    } catch (error) {
      console.warn('[MapPicker] 签到圆心反查异常:', step, error);
      accept(null);
    }
  };

  runStep();
};

export const MapPicker: React.FC<MapPickerProps> = ({
  open,
  initialLng,
  initialLat,
  initialName,
  initialDescription,
  signLng,
  signLat,
  signRange,
  signAddress,
  refreshQr = false,
  onProbeDistance,
  onClose,
  onConfirm,
}) => {
  const containerRef = useRef<HTMLDivElement | null>(null);

  const mapRef = useRef<any>(null);
  const markerRef = useRef<any>(null);
  const circleRef = useRef<any>(null);
  const centerDotRef = useRef<any>(null);
  const geocoderRef = useRef<any>(null);
  const autoRef = useRef<any>(null);
  const placeSearchRef = useRef<any>(null);
  const searchTimerRef = useRef<number | null>(null);
  const signPointRef = useRef<LngLat | null>(null);
  const signAddressRef = useRef('');
  /** 画/挪签到范围圈在 map 初始化闭包里定义，这里留一份供圆心校准复用 */
  const showSignAreaRef = useRef<((pos: LngLat) => void) | null>(null);
  /** 校准取消标志：面板关闭时置 true，让正在跑的探测循环尽快退出 */
  const calibrateCancelledRef = useRef(false);
  /** 面板是否打开：异步探测循环用它判断用户是不是已经关面板去拍下一张二维码了 */
  const openRef = useRef(open);
  /** 已校准标志：防止迟到的地址反查把校准结果覆盖回偏差几百米的兜底坐标 */
  const calibratedRef = useRef(false);
  /** 用户手动移动过选点：迟到的地址反查不再抢夺选点 */
  const userMovedRef = useRef(false);

  const [ready, setReady] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [current, setCurrent] = useState<LngLat | null>(null);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [locating, setLocating] = useState(false);
  const [signPoint, setSignPoint] = useState<LngLat | null>(null);
  const [signRadius, setSignRadius] = useState(0);
  /** 圆心来源：api=后端下发经纬度，geocode=按 locationText 地址反查，calibrated=三边定位校准 */
  const [signSource, setSignSource] = useState<'' | 'api' | 'geocode' | 'calibrated'>('');
  const [signGeocoding, setSignGeocoding] = useState(false);
  /** 反查圆心时命中的关键词（POI 名或整串地址），用于底部提示与排查 */
  const [signAnchor, setSignAnchor] = useState('');
  /** 圆心校准：每步都是一次真实提交（会带回“距圆心多少米”） */
  const [calibrating, setCalibrating] = useState(false);
  const [calibrateStatus, setCalibrateStatus] = useState('');
  /** 解算残差（米）：用解出的圆心回算各次探测距离的偏差，越小越可信 */
  const [calibrateResidual, setCalibrateResidual] = useState<number | null>(null);
  /** 本次打开面板已经校准了几轮（每轮 3 次真实提交），用于限制总请求量 */
  const [calibrateRuns, setCalibrateRuns] = useState(0);
  /**
   * 圆心校准会话：放在组件级 ref 里，不随面板开关重置。
   * 拍照模式下每次拍新码都要关面板，回来后续传靠它保留已采集的采样点。
   */
  const calibrationSessionRef = useRef<CalibrationSession | null>(null);
  /** 会话的界面镜像：用于按钮显示「继续校准（n/3）」并触发重渲染 */
  const [pendingCalibration, setPendingCalibration] = useState<CalibrationSession | null>(null);

  /** 更新/清空校准会话：ref 供异步循环读写，state 供界面显示进度 */
  const setCalibrationSession = (session: CalibrationSession | null) => {
    calibrationSessionRef.current = session;
    setPendingCalibration(
      session ? { samples: [...session.samples], probes: session.probes } : null
    );
  };

  const [keyword, setKeyword] = useState('');
  const [tips, setTips] = useState<any[]>([]);
  const [searching, setSearching] = useState(false);

  /** 更新选中点，并在没有显式地址时反向地理编码补全 */
  const applyPosition = (pos: LngLat, opts?: { name?: string; address?: string }) => {
    // 用户主动选点/拖点：标记一下，避免迟到的地址反查把选点拽回老师位置。
    userMovedRef.current = true;
    setCurrent(pos);
    if (opts?.name !== undefined) setName(opts.name);
    if (opts?.address !== undefined) setDescription(opts.address);
    if (opts) return;

    geocoderRef.current?.getAddress([pos.lng, pos.lat], (status: string, result: any) => {
      if (status !== 'complete' || !result?.regeocode) return;
      setDescription(result.regeocode.formattedAddress || '');
      const poiName = result.regeocode.pois?.[0]?.name;
      if (poiName) setName(poiName);
    });
  };

  /** 把地图定位到某个 POI 并同步大头针 */
  const focusPoi = (poi: any) => {
    const pos = readLngLat(poi?.location);
    if (!pos) return;
    mapRef.current?.setZoomAndCenter(17, [pos.lng, pos.lat]);
    markerRef.current?.setPosition([pos.lng, pos.lat]);
    applyPosition(pos, {
      name: poi?.name || '',
      address: [poi?.district, poi?.address].filter(Boolean).join('') || poi?.name || '',
    });
  };

  // 打开时初始化地图；关闭时销毁，避免复用已卸载的容器
  useEffect(() => {
    openRef.current = open;
    if (!open) return;

    const fallback = LOCATION_PRESETS[0] || {};
    // 老师设置的签到圆心：有值时优先作为初始视野，方便直接贴圆心选点。
    const teacherPoint = toLngLat(signLng, signLat);
    const teacherRange = Number(signRange) > 0 ? Number(signRange) : 0;
    signPointRef.current = teacherPoint;
    signAddressRef.current = (signAddress || '').trim();
    setSignPoint(teacherPoint);
    setSignRadius(teacherRange);
    setSignSource(teacherPoint ? 'api' : '');
    setSignGeocoding(false);
    setSignAnchor('');
    setCalibrating(false);
    setCalibrateStatus('');
    setCalibrateResidual(null);
    setCalibrateRuns(0);
    calibrateCancelledRef.current = false;
    calibratedRef.current = false;
    userMovedRef.current = false;

    // 拍照 / 相册模式：上一轮校准只做了部分探测就关面板去拍新码，
    // 这里提示用户点「继续校准」接着上次，而不是从第一次重新开始。
    const pending = calibrationSessionRef.current;
    if (pending && pending.samples.length > 0) {
      toast(
        `检测到未完成的圆心校准（已完成 ${pending.samples.length}/${pending.probes.length} 次探测），点下方「继续校准」接着上次`,
        { icon: '📷', id: 'calibrate-resume', duration: 6000 }
      );
    }

    const hasInitial = !!initialLng && !!initialLat;
    const startLng = safeNumber(initialLng || fallback.lng, teacherPoint?.lng ?? 109.720849);
    const startLat = safeNumber(initialLat || fallback.lat, teacherPoint?.lat ?? 28.290107);
    // 未手动选过点时，把视野对准签到圆心并按半径选缩放级别。
    const centerLng = hasInitial ? startLng : teacherPoint?.lng ?? startLng;
    const centerLat = hasInitial ? startLat : teacherPoint?.lat ?? startLat;
    const startZoom = !hasInitial && teacherPoint ? zoomForRange(teacherRange) : 16;

    let cancelled = false;
    let completeTimer: number | null = null;

    setReady(false);
    setLoadError('');
    setCurrent({ lng: centerLng, lat: centerLat });
    setName(initialName || (teacherPoint && !hasInitial ? '老师签到点' : ''));
    setDescription(initialDescription || '');
    setLocating(false);
    setKeyword('');
    setTips([]);
    setSearching(false);

    if (!AMAP_KEY) {
      setLoadError('未配置高德地图 Key，请在 Web/config.yaml 的 sign.amap.js_key 中填写');
      return;
    }

    const run = async () => {
      try {
        // 该包是 UMD 产物，动态导入后兼容 default / 命名导出两种形态
        const loaderNamespace: any = await import('@amap/amap-jsapi-loader');
        const loader = loaderNamespace?.default ?? loaderNamespace;
        const AMap: any = await loader.load({
          key: AMAP_KEY,
          version: '2.0',
          plugins: AMAP_PLUGINS,
        });

        if (cancelled || !containerRef.current) return;

        const map = new AMap.Map(containerRef.current, {
          zoom: startZoom,
          center: [centerLng, centerLat],
          viewMode: '2D',
          resizeEnable: true,
        });
        mapRef.current = map;

        // 老师设置的签到范围：半径圈 + 圆心点，直接告诉用户该往哪选。
        // 圆心可能来自后端下发的经纬度，也可能是按 address 反查出来的。
        const showSignArea = (pos: LngLat, opts?: { recenter?: boolean }) => {
          const radius = teacherRange > 0 ? teacherRange : 500;
          if (circleRef.current) {
            circleRef.current.setCenter([pos.lng, pos.lat]);
            circleRef.current.setRadius(radius);
          } else {
            const circle = new AMap.Circle({
              center: [pos.lng, pos.lat],
              radius,
              strokeColor: '#2563eb',
              strokeWeight: 2,
              strokeOpacity: 0.9,
              strokeStyle: 'dashed',
              fillColor: '#3b82f6',
              fillOpacity: 0.1,
              zIndex: 10,
              bubble: true,
            });
            map.add(circle);
            circleRef.current = circle;
          }

          if (AMap.CircleMarker) {
            if (centerDotRef.current) {
              centerDotRef.current.setCenter([pos.lng, pos.lat]);
            } else {
              const dot = new AMap.CircleMarker({
                center: [pos.lng, pos.lat],
                radius: 5,
                strokeColor: '#ffffff',
                strokeWeight: 2,
                fillColor: '#2563eb',
                fillOpacity: 1,
                zIndex: 20,
                bubble: true,
              });
              map.add(dot);
              centerDotRef.current = dot;
            }
          }

          if (opts?.recenter) {
            map.setZoomAndCenter(zoomForRange(radius), [pos.lng, pos.lat]);
          }
        };
        // 供页面级交互（圆心校准）复用：把圈和圆心点挪到解出来的位置
        showSignAreaRef.current = showSignArea;

        if (teacherPoint) {
          showSignArea(teacherPoint);
        }

        // 首次视野：默认回到用户已选点/预设点；地址反查成功后会改写到老师圆心。
        const focus = { lng: centerLng, lat: centerLat, zoom: startZoom };

        // 瓦片加载完成后再次对齐中心点与缩放，避免首帧未生效时画面偏移
        map.on('complete', () => {
          if (completeTimer !== null) {
            window.clearTimeout(completeTimer);
            completeTimer = null;
          }
          if (cancelled) return;
          map.setZoomAndCenter(focus.zoom, [focus.lng, focus.lat]);
        });

        // 底图长时间加载不完成时给出明确提示：
        // Key / 安全密钥 / 白名单异常只会让瓦片请求失败，不会抛异常，需要主动兜底。
        completeTimer = window.setTimeout(() => {
          completeTimer = null;
          if (cancelled) return;
          toast.error('底图加载超时：请检查 Key 是否为「Web端(JS API)」、安全密钥是否匹配、域名是否在白名单内');
        }, 10000);

        const marker = new AMap.Marker({
          position: [centerLng, centerLat],
          draggable: true,
          cursor: 'move',
          anchor: 'bottom-center',
          title: '拖动选择位置',
        });
        // 等价于 marker.setMap(map)，显式加入地图实例
        map.add(marker);
        markerRef.current = marker;

        geocoderRef.current = new AMap.Geocoder({});
        autoRef.current = new AMap.AutoComplete({});
        placeSearchRef.current = new AMap.PlaceSearch({ pageSize: 1 });

        // 后端没下发圆心经纬度、但给了地址（locationText）时，按地址反查圆心。
        // 这样即使学习通把经纬度置空，前端依然能画圈、依然能一键“用签到点”。
        const willGeocodeSignAddress = !teacherPoint && signAddressRef.current !== '';
        if (willGeocodeSignAddress) {
          setSignGeocoding(true);
          resolveSignCenterFromAddress(AMap, signAddressRef.current, (hit) => {
            if (cancelled) return;
            setSignGeocoding(false);
            if (!hit) {
              console.warn('[MapPicker] 地址反查签到圆心失败:', signAddressRef.current);
              return;
            }
            const pos = hit.pos;
            // 圆心校准已经给出真实圆心：迟到的地址反查不要再把它覆盖回
            // 偏差几百米的兜底坐标（反之亦然，用户会看到刚校准的圈被拽回去）。
            if (calibratedRef.current) {
              console.info('[MapPicker] 地址反查返回时已校准，忽略该结果:', pos);
              return;
            }
            console.info(
              '[MapPicker] 地址反查签到圆心:',
              `via=${hit.via}`,
              `keyword=${hit.keyword}`,
              pos,
              `address=${signAddressRef.current}`
            );
            setSignSource('geocode');
            setSignAnchor(hit.keyword);
            setSignPoint(pos);
            signPointRef.current = pos;
            showSignArea(pos);

            // 还没有手动选点时，直接把大头针放到老师的位置，省掉一次拖拽。
            // 用户已经自己挪过点就不再抢夺。
            if (!hasInitial && !userMovedRef.current) {
              focus.lng = pos.lng;
              focus.lat = pos.lat;
              focus.zoom = zoomForRange(teacherRange);
              markerRef.current?.setPosition([pos.lng, pos.lat]);
              mapRef.current?.setZoomAndCenter(focus.zoom, [pos.lng, pos.lat]);
              setCurrent(pos);
              setName('老师签到点');
              setDescription(signAddressRef.current);
            }
          });
        }

        marker.on('dragend', (event: any) => {
          const pos = readLngLat(event?.lnglat);
          if (pos) applyPosition(pos);
        });

        map.on('click', (event: any) => {
          const pos = readLngLat(event?.lnglat);
          if (!pos) return;
          marker.setPosition([pos.lng, pos.lat]);
          applyPosition(pos);
        });

        // 已有选中点时保留原有名称/地址，只补全缺失的部分。
        // 正在按地址反查圆心时跳过：否则会用预设点的地址覆盖老师的地址。
        if (!willGeocodeSignAddress && (!initialDescription || !initialName)) {
          geocoderRef.current.getAddress([centerLng, centerLat], (status: string, result: any) => {
            if (cancelled || status !== 'complete' || !result?.regeocode) return;
            if (!initialDescription) {
              setDescription(result.regeocode.formattedAddress || '');
            }
            if (!initialName) {
              const poiName = result.regeocode.pois?.[0]?.name;
              if (poiName) setName(poiName);
            }
          });
        }

        setReady(true);
      } catch (error: any) {
        // 把原始错误打到控制台，便于定位 Key / 密钥 / 白名单问题
        console.error('[MapPicker] 高德地图加载失败:', error);
        if (!cancelled) {
          setLoadError(error?.info || error?.message || '高德地图加载失败，请检查网络或 Key 配置');
        }
      }
    };

    run();

    return () => {
      cancelled = true;
      // 面板关闭：中断正在跑的圆心校准探测，避免后台继续对学习通发请求
      calibrateCancelledRef.current = true;
      showSignAreaRef.current = null;
      if (completeTimer !== null) {
        window.clearTimeout(completeTimer);
        completeTimer = null;
      }
      if (searchTimerRef.current !== null) {
        window.clearTimeout(searchTimerRef.current);
        searchTimerRef.current = null;
      }
      if (mapRef.current) {
        mapRef.current.destroy();
        mapRef.current = null;
      }
      markerRef.current = null;
      circleRef.current = null;
      centerDotRef.current = null;
      geocoderRef.current = null;
      autoRef.current = null;
      placeSearchRef.current = null;
    };
    // 仅在打开瞬间初始化；initial* 作为打开时的初始位置
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const handleKeywordChange = (value: string) => {
    setKeyword(value);

    if (searchTimerRef.current !== null) {
      window.clearTimeout(searchTimerRef.current);
      searchTimerRef.current = null;
    }

    const auto = autoRef.current;
    if (!auto || !value.trim()) {
      setTips([]);
      setSearching(false);
      return;
    }

    setSearching(true);
    searchTimerRef.current = window.setTimeout(() => {
      auto.search(value.trim(), (status: string, result: any) => {
        setSearching(false);
        setTips(status === 'complete' ? result?.tips || [] : []);
      });
    }, 300);
  };

  const handleSelectTip = (tip: any) => {
    setTips([]);
    setKeyword(tip?.name || '');

    if (readLngLat(tip?.location)) {
      focusPoi(tip);
      return;
    }

    if (tip?.id && placeSearchRef.current) {
      placeSearchRef.current.getDetails(tip.id, (status: string, result: any) => {
        if (status === 'complete') focusPoi(result?.poiList?.pois?.[0]);
      });
    }
  };

  /** 浏览器定位返回 WGS-84，转成 GCJ-02 后再落到地图上 */
  const handleLocate = () => {
    if (!navigator.geolocation) {
      toast.error('当前浏览器不支持定位');
      return;
    }

    setLocating(true);
    navigator.geolocation.getCurrentPosition(
      (position) => {
        const gcj = wgs84ToGcj02(position.coords.longitude, position.coords.latitude);
        userMovedRef.current = true;
        setCurrent(gcj);
        mapRef.current?.setZoomAndCenter(17, [gcj.lng, gcj.lat]);
        markerRef.current?.setPosition([gcj.lng, gcj.lat]);
        geocoderRef.current?.getAddress([gcj.lng, gcj.lat], (status: string, result: any) => {
          if (status === 'complete' && result?.regeocode) {
            setDescription(result.regeocode.formattedAddress || '');
            const poiName = result.regeocode.pois?.[0]?.name;
            if (poiName) setName(poiName);
          }
        });
        setLocating(false);
        toast.success('已获取当前位置（已转换为 GCJ-02）');
      },
      (error) => {
        setLocating(false);
        toast.error(`定位失败：${error.message || '请检查是否授予定位权限'}`);
      },
      { enableHighAccuracy: true, timeout: 10000, maximumAge: 0 }
    );
  };

  /**
   * 在签到圆心附近做几米随机偏移：
   * 既保证落在老师设置的半径内，又避免所有账号提交完全相同的坐标。
   */
  const jitterNear = (point: LngLat, maxMeters = 8): LngLat => {
    const angle = Math.random() * Math.PI * 2;
    const meters = 2 + Math.random() * maxMeters; // 2~10 米
    const dLat = (meters * Math.cos(angle)) / 111320;
    const cosLat = Math.cos((point.lat * Math.PI) / 180) || 1;
    const dLng = (meters * Math.sin(angle)) / (111320 * cosLat);
    return { lng: point.lng + dLng, lat: point.lat + dLat };
  };

  /** 一键落到老师设置的签到点（加轻微随机偏移） */
  const handleUseSignPoint = () => {
    const point = signPointRef.current;
    if (!point) return;

    const target = jitterNear(point);
    userMovedRef.current = true;
    setCurrent(target);
    mapRef.current?.setZoomAndCenter(Math.max(zoomForRange(signRadius), 17), [target.lng, target.lat]);
    markerRef.current?.setPosition([target.lng, target.lat]);
    setName('老师签到点');
    setDescription(signAddressRef.current || '老师设置的签到点');
    toast.success('已定位到老师签到点附近');
  };

  /** 随机等待并读秒（可被关闭面板打断）；返回 false 表示已取消 */
  const waitRandom = async (label: string, minMs = 4000, maxMs = 9000) => {
    const end = Date.now() + Math.round(minMs + Math.random() * (maxMs - minMs));
    while (Date.now() < end) {
      if (calibrateCancelledRef.current) return false;
      const remain = Math.ceil((end - Date.now()) / 1000);
      setCalibrateStatus(`${label}，${remain} 秒后继续`);
      await new Promise<void>((resolve) => window.setTimeout(resolve, 250));
    }
    return !calibrateCancelledRef.current;
  };

  /**
   * 自动校准签到圆心（三边定位）。
   *
   * 背景：学习通的位置签到只给地址文案、不下发圆心经纬度，但每次“超出范围”的
   * 提交都会回 errorLocation2_<米>，也就是本次提交点距真实圆心的精确距离。
   * 用它当标尺，三个不共线的点各探测一次就能反解出真实圆心：
   *   1. 以当前选中点为基准，另取两个偏移点（约 320~450 米，整体随机旋转，保证不共线）；
   *   2. 依次探测，顺序打乱、每次间隔随机 4~9 秒，避免等间隔高频请求被风控；
   *      刷新码例外：enc 只有几秒有效期，间隔由「等一张新码」自己决定，不再叠加长等待；
   *   3. 最小二乘解出圆心，把半径圈和选中点一起挪过去，并显示残差。
   *
   * 可续传：手机 HTTP 页面只能拍照 / 相册导入二维码，刷新码每探测一次都要一张新码。
   * 探测返回 needQr 时本轮暂停，已采集的采样点和固定的三个探测点会保留在
   * calibrationSessionRef 里；用户拍下一张回来点「继续校准」，从上次断点接着探测。
   *
   * 探测点都在范围外，正常不会签到成功；万一某次恰好落在范围内，直接算签到成功。
   */
  const handleCalibrate = async () => {
    if (!onProbeDistance || calibrating) return;

    // 有未完成的会话就接着上次跑，不需要重新选点，也不占用新的“校准轮次”。
    let session = calibrationSessionRef.current;
    if (!session) {
      const seed = current ?? signPointRef.current;
      if (!seed) {
        toast.error('请先在地图上选一个大致的点再校准');
        return;
      }
      // 每轮校准 = 3 次真实提交。为避免对学习通造成高频请求，单次选点最多 2 轮。
      if (calibrateRuns >= CALIBRATE_MAX_RUNS) {
        toast.error('本次选点已校准过，若仍不准请关闭面板重新打开后再试', { duration: 5000 });
        return;
      }
      setCalibrateRuns((n) => n + 1);

      // 两个偏移方向正交（保证三点不共线），整体随机旋转，避免每次探测点完全一致。
      const spin = Math.random() * Math.PI * 2;
      const step = 320 + Math.random() * 130;
      const rotate = (east: number, north: number) => ({
        east: east * Math.cos(spin) - north * Math.sin(spin),
        north: east * Math.sin(spin) + north * Math.cos(spin),
      });
      const eastDir = rotate(step, 0);
      const northDir = rotate(0, step);

      const probes: { label: string; point: LngLat }[] = [
        { label: '基准点', point: seed },
        { label: '偏移点 A', point: offsetMeters(seed, eastDir.east, eastDir.north) },
        { label: '偏移点 B', point: offsetMeters(seed, northDir.east, northDir.north) },
      ];
      // 探测顺序也打乱：不只是间隔无序，整体节奏也不固定。
      for (let i = probes.length - 1; i > 0; i -= 1) {
        const j = Math.floor(Math.random() * (i + 1));
        [probes[i], probes[j]] = [probes[j], probes[i]];
      }

      session = { samples: [], probes };
      setCalibrationSession(session);
    }

    const probes = session.probes;
    // 会话本身持有采样数组：每拿到一个距离就往里 push，关面板也不会丢
    const samples = session.samples;
    // 续传时跳过已采集的点（samples.length 就是下一个待探测下标）
    const startIndex = samples.length;

    calibrateCancelledRef.current = false;
    setCalibrating(true);
    setCalibrateStatus(
      startIndex > 0
        ? `继续校准：已完成 ${startIndex}/${probes.length} 次探测，接着探测第 ${startIndex + 1} 个点`
        : '正在准备探测…'
    );
    setCalibrateResidual(null);

    try {
      // 刷新码：父页面每次探测前都会等一张新码，那本身就是天然间隔；
      // 这里再等 4~9 秒只会让 enc 白等过期，所以长等待只给静态码。
      let isRefreshing = !!refreshQr;
      for (let i = startIndex; i < probes.length; i += 1) {
        // 续传回来的第一个点不等待（用户刚拍完新码回来）；同一轮内相邻探测才需要间隔。
        if (i > startIndex) {
          if (isRefreshing) {
            // 刷新码：只做极短停顿，真正的间隔由父页面「等一张新码」决定
            await new Promise<void>((resolve) => window.setTimeout(resolve, 400));
            if (calibrateCancelledRef.current) return;
          } else if (!(await waitRandom(`第 ${i + 1} 次探测前等待`))) {
            return;
          }
        }
        if (calibrateCancelledRef.current) return;

        setCalibrateStatus(
          isRefreshing && i > startIndex
            ? `探测 ${i + 1}/${probes.length}：${probes[i].label}（等待扫到新二维码）`
            : `探测 ${i + 1}/${probes.length}：${probes[i].label}`
        );
        // 每次探测都是一次真实提交；没拿到距离就随机间隔后重试该点，避免偶发无距离直接废掉整轮校准。
        let res: SignCenterProbe | null = null;
        const maxAttempts = PROBE_MAX_ATTEMPTS;
        for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
          if (attempt > 1) {
            if (isRefreshing) {
              // 刷新码：重试前只做极短停顿，新 enc 由父页面在探测里等
              await new Promise<void>((resolve) => window.setTimeout(resolve, 400));
            } else if (!(await waitRandom(`第 ${i + 1} 次探测重试前等待`, 3000, 7000))) {
              return;
            }
            if (calibrateCancelledRef.current) return;
          }
          let once: SignCenterProbe;
          try {
            once = await onProbeDistance(probes[i].point);
          } catch (error: any) {
            setCalibrateStatus('探测请求失败');
            toast.error(error?.message || '探测请求失败');
            return;
          }
          if (calibrateCancelledRef.current) return;
          res = once;
          if (typeof once.refreshQr === 'boolean') isRefreshing = once.refreshQr;
          if (once.needQr) {
            // 手上没有可用新码（拍照 / 相册模式）：暂停本轮，保留已采集的采样点，
            // 等用户拍下一张后回来点「继续校准」，从这一点接着探测。
            setCalibrationSession(session);
            setCalibrateStatus(
              `已保存 ${samples.length}/${probes.length} 次探测，请拍摄/导入下一张二维码后回来点「继续校准」`
            );
            toast(
              `已保存 ${samples.length}/${probes.length} 次探测，请拍下一张二维码后回来继续校准`,
              { icon: '📷', duration: 6000, id: 'calibrate-need-qr' }
            );
            return;
          }
          if (once.signed) break;
          if (typeof once.distance === 'number' && Number.isFinite(once.distance) && once.distance > 0) {
            break;
          }
          // 明确不可重试（没扫到新码 / 面板已关）就不再叠加第二次探测
          if (once.retryable === false) break;
          if (attempt < maxAttempts) {
            setCalibrateStatus(`探测 ${i + 1}/${probes.length} 未返回距离，稍后重试`);
          }
        }
        if (!res) return;

        if (res.signed) {
          // 探测点正好落在签到范围内：学习通已记录签到，不需要再解算圆心。
          const target = probes[i].point;
          userMovedRef.current = true;
          setCurrent(target);
          markerRef.current?.setPosition([target.lng, target.lat]);
          setCalibrateStatus('探测时已直接签到成功');
          toast.success('探测时已直接签到成功', { duration: 5000 });
          // 已经签上了，这次校准到此结束
          setCalibrationSession(null);
          return;
        }
        if (typeof res.distance !== 'number' || !Number.isFinite(res.distance) || res.distance <= 0) {
          // 用户已经关面板去拍照了：不要再弹错误打扰扫码
          if (!openRef.current) return;
          const reason = res.message || '学习通没有返回距离，无法校准圆心';
          setCalibrateStatus(`${reason}（已采集 ${samples.length}/${probes.length}，可点「继续校准」重试）`);
          toast.error(reason, { duration: 6000 });
          // 保留已采集的采样点：偶发无距离时用户可以直接再点「继续校准」重试这一探测点
          setCalibrationSession(session);
          return;
        }
        samples.push({ lng: probes[i].point.lng, lat: probes[i].point.lat, distance: res.distance });
        // 每拿到一个距离就同步会话：即使中途关面板，已采集的进度也不会丢
        setCalibrationSession(session);
      }

      const solved = solveSignCenter(samples);
      if (!solved) {
        setCalibrateStatus('三个探测点几乎共线，无法解算圆心');
        toast.error('三个探测点几乎共线，无法解算圆心，请换个位置再试');
        // 几何形状本身有问题，下一次重新生成探测点
        setCalibrationSession(null);
        return;
      }

      const { center, maxResidual } = solved;
      console.info(
        '[MapPicker] 圆心校准完成:',
        `center=${center.lng},${center.lat}`,
        `residual=${maxResidual.toFixed(1)}m`,
        'samples=',
        samples
      );

      // 圆和圆心点挪到解算结果；选中点落在圆心附近（带几米随机偏移，避免各账号坐标一致）。
      // 同时打上“已校准”标记：迟到的地址反查不能再把圈和选点拽回偏差几百米的位置。
      calibratedRef.current = true;
      userMovedRef.current = true;
      signPointRef.current = center;
      setSignPoint(center);
      setSignSource('calibrated');
      setSignAnchor('');
      setCalibrateResidual(maxResidual);
      if (showSignAreaRef.current) {
        showSignAreaRef.current(center);
      } else {
        // 正常不会走到这里（按钮要等地图 ready 才能点）；留一条日志便于排查“圈没动”。
        console.warn('[MapPicker] 校准完成但半径圈尚未初始化，未能移动圆圈');
      }

      const target = jitterNear(center);
      setCurrent(target);
      markerRef.current?.setPosition([target.lng, target.lat]);
      mapRef.current?.setZoomAndCenter(Math.max(zoomForRange(signRadius), 17), [target.lng, target.lat]);
      setName('老师签到点（已校准）');
      setDescription(signAddressRef.current || '自动校准的签到圆心');

      setCalibrateStatus(`圆心已校准（残差约 ${Math.round(maxResidual)} 米）`);
      // 本轮校准完成：清空会话，下次点「重新校准圆心」会生成全新探测点
      setCalibrationSession(null);
      if (maxResidual > 40) {
        toast('圆心已校准，但残差偏大，建议再校准一次复核', { icon: '⚠️', duration: 6000 });
      } else {
        toast.success('圆心已校准，已把选中点放到圆心附近', { duration: 5000 });
      }
    } finally {
      setCalibrating(false);
    }
  };

  // 当前选点与签到圆心的距离：用于提示是否落在老师设置的半径内。
  const distanceToSign = signPoint && current ? distanceMeters(current, signPoint) : Number.NaN;
  const withinSignRange =
    !!signPoint && Number.isFinite(distanceToSign) && signRadius > 0 && distanceToSign <= signRadius;

  const handleConfirm = () => {
    if (!current) {
      toast.error('请先在地图上选择位置');
      return;
    }

    // 用户确认选点：这次校准（含未完成的会话）到此结束
    setCalibrationSession(null);

    // 超出半径时给出明确提示，但仍允许提交（老师坐标本身也可能有偏差）。
    if (signPoint && signRadius > 0 && Number.isFinite(distanceToSign) && distanceToSign > signRadius) {
      toast.error(
        `当前选点距签到点约 ${Math.round(distanceToSign)} 米，超出 ${signRadius} 米签到范围，签到可能失败`,
        { id: 'sign-range-warn', duration: 4000 }
      );
    }

    const lngStr = formatCoord(current.lng);
    const latStr = formatCoord(current.lat);
    const finalName = name.trim() || '地图选点';
    const finalDescription = description.trim() || finalName;

    // 把本次解析到的圆心一并回传：后端没下发经纬度时是地址反查结果，
    // 父页面可以据此判断已选点是否落在签到范围内。
    const center = signPointRef.current;
    onConfirm({
      name: finalName,
      lng: lngStr,
      lat: latStr,
      description: finalDescription,
      ...(center ? { signLng: center.lng, signLat: center.lat } : {}),
    });
  };

  return (
    <AnimatePresence>
      {open && (
        <motion.div
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          className="fixed inset-0 z-[120] bg-white flex flex-col"
        >
          <div className="flex items-center justify-between px-5 pt-[calc(12px+var(--sat))] pb-3 border-b border-slate-100 shrink-0">
            <h3 className="text-lg font-bold text-slate-900">地图选点</h3>
            <button
              type="button"
              onClick={onClose}
              className="w-9 h-9 flex items-center justify-center bg-slate-100 text-slate-400 rounded-full"
            >
              <X size={18} />
            </button>
          </div>

          <div className="px-4 py-3 shrink-0 relative z-[130]">
            <div className="flex items-center gap-2 px-4 py-3 bg-slate-100 rounded-2xl">
              <Search size={16} className="text-slate-400 shrink-0" />
              <input
                type="text"
                value={keyword}
                onChange={(event) => handleKeywordChange(event.target.value)}
                disabled={!ready}
                placeholder={ready ? '搜索地点、建筑、地址' : '地图加载中…'}
                className="flex-1 min-w-0 bg-transparent outline-none text-sm text-slate-800 placeholder:text-slate-400 disabled:cursor-not-allowed"
              />
              {searching && <Loader2 size={14} className="animate-spin text-slate-400 shrink-0" />}
            </div>

            {tips.length > 0 && (
              <div className="absolute left-4 right-4 top-full -mt-1 bg-white rounded-2xl shadow-xl border border-slate-100 max-h-64 overflow-y-auto">
                {tips.map((tip, index) => (
                  <button
                    key={tip?.id || index}
                    type="button"
                    onClick={() => handleSelectTip(tip)}
                    className="w-full text-left px-4 py-3 hover:bg-slate-50 border-b border-slate-50 last:border-0"
                  >
                    <p className="text-sm font-bold text-slate-800 truncate">{tip?.name || '未命名地点'}</p>
                    <p className="text-[11px] text-slate-400 truncate">
                      {[tip?.district, tip?.address].filter(Boolean).join(' ') || ' '}
                    </p>
                  </button>
                ))}
              </div>
            )}
          </div>

          <div className="relative flex-1 min-h-0 bg-slate-50">
            {/* 显式宽高兜底：防止 flex 布局下高度塌陷为 0 */}
            <div
              ref={containerRef}
              className="absolute inset-0"
              style={{ width: '100%', height: '100%' }}
            />

            {!ready && !loadError && (
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-slate-50 text-slate-400">
                <Loader2 size={28} className="animate-spin" />
                <span className="text-xs font-bold">正在加载高德地图…</span>
              </div>
            )}

            {loadError && (
              <div className="absolute inset-0 flex items-center justify-center bg-slate-50 px-8 text-center text-sm font-bold text-red-500">
                {loadError}
              </div>
            )}

            <button
              type="button"
              onClick={handleLocate}
              disabled={locating || !ready}
              title="获取当前真实位置"
              className="absolute right-4 bottom-4 w-12 h-12 rounded-full bg-white shadow-lg border border-slate-100 flex items-center justify-center text-blue-600 active:scale-95 transition-transform disabled:opacity-60"
            >
              {locating ? <Loader2 size={20} className="animate-spin" /> : <LocateFixed size={20} />}
            </button>

            {/* 有老师设置的签到点时，提供一键贴近圆心的入口 */}
            {signPoint && (
              <button
                type="button"
                onClick={handleUseSignPoint}
                disabled={!ready}
                title={signAddressRef.current || '老师设置的签到点'}
                className="absolute right-4 bottom-20 h-9 px-3 rounded-full bg-blue-600 text-white text-xs font-bold shadow-lg shadow-blue-600/30 active:scale-95 transition-transform disabled:opacity-60"
              >
                用签到点
              </button>
            )}
          </div>

          <div className="px-5 py-4 border-t border-slate-100 shrink-0 pb-[calc(16px+var(--sab))]">
            <div className="flex items-start gap-2 mb-3">
              <MapPin size={16} className="text-blue-500 shrink-0 mt-0.5" />
              <div className="min-w-0">
                <p className="font-bold text-slate-800 text-sm truncate">
                  {name || '拖动大头针或点击地图选点'}
                </p>
                <p className="text-[11px] text-slate-400 truncate">
                  {description ||
                    (current
                      ? `${formatCoord(current.lng)}, ${formatCoord(current.lat)}`
                      : '拖动大头针或点击地图选择位置')}
                </p>
              </div>
            </div>

            {signPoint ? (
              <div
                className={`mb-3 flex items-center gap-1.5 text-[11px] font-bold ${
                  withinSignRange ? 'text-emerald-600' : 'text-amber-600'
                }`}
              >
                <Target size={13} className="shrink-0" />
                <span className="truncate">
                  {Number.isFinite(distanceToSign)
                    ? `距签到点约 ${Math.round(distanceToSign)} 米`
                    : '正在计算与签到点的距离'}
                  {signRadius > 0 ? `（签到范围 ${signRadius} 米）` : ''}
                  {withinSignRange ? ' · 在范围内' : ' · 建议靠近圆心'}
                  {signSource === 'geocode'
                    ? ` · 按地址定位${signAnchor ? `：${signAnchor}` : ''}`
                    : ''}
                  {signSource === 'calibrated'
                    ? ` · 已自动校准${calibrateResidual !== null ? `（残差 ${Math.round(calibrateResidual)} 米）` : ''}`
                    : ''}
                </span>
              </div>
            ) : signGeocoding ? (
              <div className="mb-3 flex items-center gap-1.5 text-[11px] text-slate-400">
                <Loader2 size={13} className="shrink-0 animate-spin" />
                <span className="truncate">正在按老师下发的地址定位签到点…</span>
              </div>
            ) : signAddress ? (
              <div className="mb-3 flex items-center gap-1.5 text-[11px] font-bold text-amber-600">
                <Target size={13} className="shrink-0" />
                <span className="truncate" title={signAddress}>
                  {`老师签到点：${signAddress}${signRadius > 0 ? `（半径 ${signRadius} 米）` : ''} · 地址定位失败，请贴近该地址`}
                </span>
              </div>
            ) : (
              <div className="mb-3 flex items-center gap-1.5 text-[11px] text-slate-400">
                <Target size={13} className="shrink-0" />
                <span className="truncate">未获取到老师的签到点，请尽量贴近老师所在位置选择</span>
              </div>
            )}

            {/* 地址定位只是兜底：精度可能差几百米，提醒先校准再提交 */}
            {signSource === 'geocode' && onProbeDistance && !calibrating && (
              <div className="mb-2 flex items-start gap-1.5 rounded-xl bg-amber-50 px-3 py-2 text-[11px] font-bold text-amber-700">
                <Info size={13} className="mt-0.5 shrink-0" />
                <span>地址定位精度有限（实测可能偏差数百米），建议先点下方「自动校准圆心」再提交。</span>
              </div>
            )}

            {/* 刷新码：enc 只有几秒有效期，校准要连续探测，必须摄像头一直能扫到 */}
            {refreshQr && onProbeDistance && signSource !== 'api' && (
              <div className="mb-2 flex items-start gap-1.5 rounded-xl bg-slate-50 px-3 py-2 text-[11px] font-bold text-slate-500">
                <Info size={13} className="mt-0.5 shrink-0" />
                <span>
                  这是刷新二维码：校准期间请让摄像头一直对准二维码，每次探测会自动等一张新码（约 5~10 秒）。
                  若是拍照 / 相册导入，每探测一次都要再拍一张新码，已采集的进度会自动保留，拍完点「继续校准」即可。
                </span>
              </div>
            )}

            {/* 圆心校准：学习通不下发圆心经纬度，用 errorLocation2_<米> 的距离反馈反解真实圆心 */}
            {onProbeDistance && signSource !== 'api' && (
              <button
                type="button"
                onClick={handleCalibrate}
                disabled={
                  !ready ||
                  calibrating ||
                  locating ||
                  !current ||
                  (!pendingCalibration && calibrateRuns >= CALIBRATE_MAX_RUNS)
                }
                title="用三次真实探测反解老师设置的签到圆心（探测都在范围外，不会误签到）"
                className="w-full mb-2 py-3 rounded-xl border-2 border-blue-100 bg-blue-50/60 text-blue-700 font-bold text-sm flex items-center justify-center gap-2 active:scale-[0.98] transition-transform disabled:opacity-60 disabled:active:scale-100"
              >
                {calibrating ? <Loader2 size={15} className="animate-spin" /> : <Compass size={15} />}
                <span className="truncate">
                  {calibrating
                    ? calibrateStatus || '正在校准圆心…'
                    : pendingCalibration
                      ? pendingCalibration.samples.length > 0
                        ? `继续校准（已完成 ${pendingCalibration.samples.length}/${pendingCalibration.probes.length} 次探测）`
                        : '继续校准（等待新二维码）'
                      : calibrateRuns >= CALIBRATE_MAX_RUNS
                        ? '本次选点已校准，需重开面板才能再校准'
                        : signSource === 'calibrated'
                          ? '重新校准圆心'
                          : '自动校准圆心（3 次探测）'}
                </span>
              </button>
            )}
            {pendingCalibration && !calibrating && (
              <>
                <button
                  type="button"
                  onClick={onClose}
                  className="w-full mb-2 py-2.5 rounded-xl border-2 border-amber-100 bg-amber-50 text-amber-700 font-bold text-[11px] active:scale-[0.98] transition-transform"
                >
                  关闭面板去拍下一张二维码（校准进度已保留）
                </button>
                {pendingCalibration.samples.length > 0 && (
                  <button
                    type="button"
                    onClick={() => {
                      setCalibrationSession(null);
                      setCalibrateStatus('');
                    }}
                    className="w-full mb-2 py-2 rounded-xl text-[11px] font-bold text-slate-400 active:scale-[0.98] transition-transform"
                  >
                    重新开始校准（丢弃已采集的 {pendingCalibration.samples.length} 次探测）
                  </button>
                )}
              </>
            )}
            {!calibrating && calibrateStatus && (
              <p className="mb-2 text-[11px] font-medium text-slate-400 text-center">{calibrateStatus}</p>
            )}

            <button
              type="button"
              onClick={handleConfirm}
              disabled={!ready}
              className="w-full py-3.5 rounded-xl bg-blue-600 text-white font-bold text-sm shadow-lg shadow-blue-600/25 active:scale-[0.98] transition-transform disabled:opacity-60 disabled:active:scale-100"
            >
              确认选择
            </button>
          </div>
        </motion.div>
      )}
    </AnimatePresence>
  );
};
