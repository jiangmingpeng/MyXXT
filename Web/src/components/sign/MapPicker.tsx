import React, { useEffect, useRef, useState } from 'react';
import { AnimatePresence, motion } from 'framer-motion';
import { LocateFixed, Loader2, MapPin, Search, X } from 'lucide-react';
import toast from 'react-hot-toast';
import config from '../../../config.yaml';
import { formatCoord, wgs84ToGcj02 } from '../../utils/coord';
import type { LngLat } from '../../utils/coord';

const AMAP_KEY: string = config.sign?.amap?.js_key || '';
const AMAP_SECURITY_CODE: string = config.sign?.amap?.security_code || '';
const LOCATION_PRESETS: any[] = config.sign?.location_presets || [];

// 高德 JS API 2.0 安全密钥：必须在 SDK 脚本加载之前挂到 window 上。
// 放在模块顶层（而不是异步函数里），确保任何 import() / load() 之前都已生效。
if (AMAP_SECURITY_CODE) {
  window._AMapSecurityConfig = { securityJsCode: AMAP_SECURITY_CODE };
}

const AMAP_PLUGINS = ['AMap.Geocoder', 'AMap.AutoComplete', 'AMap.PlaceSearch'];

export interface MapPickerResult {
  name: string;
  lng: string;
  lat: string;
  description: string;
}

interface MapPickerProps {
  open: boolean;
  initialLng?: string;
  initialLat?: string;
  initialName?: string;
  initialDescription?: string;
  onClose: () => void;
  onConfirm: (result: MapPickerResult) => void;
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

export const MapPicker: React.FC<MapPickerProps> = ({
  open,
  initialLng,
  initialLat,
  initialName,
  initialDescription,
  onClose,
  onConfirm,
}) => {
  const containerRef = useRef<HTMLDivElement | null>(null);

  const mapRef = useRef<any>(null);
  const markerRef = useRef<any>(null);
  const geocoderRef = useRef<any>(null);
  const autoRef = useRef<any>(null);
  const placeSearchRef = useRef<any>(null);
  const searchTimerRef = useRef<number | null>(null);

  const [ready, setReady] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [current, setCurrent] = useState<LngLat | null>(null);
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [locating, setLocating] = useState(false);

  const [keyword, setKeyword] = useState('');
  const [tips, setTips] = useState<any[]>([]);
  const [searching, setSearching] = useState(false);

  /** 更新选中点，并在没有显式地址时反向地理编码补全 */
  const applyPosition = (pos: LngLat, opts?: { name?: string; address?: string }) => {
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
    if (!open) return;

    const fallback = LOCATION_PRESETS[0] || {};
    const startLng = safeNumber(initialLng || fallback.lng, 109.720849);
    const startLat = safeNumber(initialLat || fallback.lat, 28.290107);

    let cancelled = false;
    let completeTimer: number | null = null;

    setReady(false);
    setLoadError('');
    setCurrent({ lng: startLng, lat: startLat });
    setName(initialName || '');
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
          zoom: 16,
          center: [startLng, startLat],
          viewMode: '2D',
          resizeEnable: true,
        });
        mapRef.current = map;

        // 瓦片加载完成后再次对齐中心点与缩放，避免首帧未生效时画面偏移
        map.on('complete', () => {
          if (completeTimer !== null) {
            window.clearTimeout(completeTimer);
            completeTimer = null;
          }
          if (cancelled) return;
          map.setZoomAndCenter(16, [startLng, startLat]);
        });

        // 底图长时间加载不完成时给出明确提示：
        // Key / 安全密钥 / 白名单异常只会让瓦片请求失败，不会抛异常，需要主动兜底。
        completeTimer = window.setTimeout(() => {
          completeTimer = null;
          if (cancelled) return;
          toast.error('底图加载超时：请检查 Key 是否为「Web端(JS API)」、安全密钥是否匹配、域名是否在白名单内');
        }, 10000);

        const marker = new AMap.Marker({
          position: [startLng, startLat],
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

        // 已有选中点时保留原有名称/地址，只补全缺失的部分
        if (!initialDescription || !initialName) {
          geocoderRef.current.getAddress([startLng, startLat], (status: string, result: any) => {
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

  const handleConfirm = () => {
    if (!current) {
      toast.error('请先在地图上选择位置');
      return;
    }

    const lngStr = formatCoord(current.lng);
    const latStr = formatCoord(current.lat);
    const finalName = name.trim() || '地图选点';
    const finalDescription = description.trim() || finalName;

    onConfirm({ name: finalName, lng: lngStr, lat: latStr, description: finalDescription });
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
