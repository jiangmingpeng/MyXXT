import { useEffect, useRef, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { motion, AnimatePresence } from 'framer-motion';
import { ChevronLeft, Camera, EyeOff, Eye, MapPin, CheckCircle2, ImagePlus, Loader2 } from 'lucide-react';
import { Html5Qrcode, type CameraDevice } from 'html5-qrcode';
import toast from 'react-hot-toast';
import client from '../api/client';
import { useAuthStore } from '../store/auth';
import { useSignStore } from '../store/sign';
import { distanceMeters, formatCoord } from '../utils/coord';
import { ProgressCard } from '../components/sign/ProgressCard';
import { MapPicker } from '../components/sign/MapPicker';
import type { MapPickerResult } from '../components/sign/MapPicker';
import type { ApiResponse, SignStatusMessage, Classmate, User, SignExecuteResult } from '../types';
import { SIGN_ERR_QR_EXPIRED } from '../constants';
import scanCursor from '../assets/scan_cursor.png';
import config from '../../config.yaml';

const LOCATION_PRESETS = config.sign?.location_presets || [];

interface QrData {
  enc: string;
  c: string;
  dbStrategy: string;
  strategyPara: string;
  timestamp: number;
}

type NativeCameraBridge = {
  isReady?: () => boolean;
  getCameraState?: () => string;
  setScannerActive?: (active: boolean) => void;
  setLensFacing?: (mode: 'user' | 'environment') => void;
  zoomByPinchDelta?: (delta: number) => number;
  syncPunchHole?: (left: number, top: number, width: number, height: number) => void;
};

type WindowWithBridge = Window & {
  XBTCameraBridge?: NativeCameraBridge;
};

/**
 * 判断已选位置是否落在老师设置的签到半径之外。
 * 学习通对二维码签到做距离校验（常见 500/1000 米），
 * 超出范围时提交一定会失败，需要先让用户重新选点。
 */
const isLocationOutsideSignRange = (
  loc: { lng: string | number; lat: string | number } | null | undefined,
  centerLat?: number,
  centerLng?: number,
  range?: number
): boolean => {
  if (!loc || !centerLat || !centerLng || !range || range <= 0) return false;
  const lng = Number(loc.lng);
  const lat = Number(loc.lat);
  if (!Number.isFinite(lng) || !Number.isFinite(lat)) return false;
  const distance = distanceMeters({ lng, lat }, { lng: centerLng, lat: centerLat });
  return Number.isFinite(distance) && distance > range;
};

const getFriendlyCameraLabel = (camera: CameraDevice, index: number) => {
  const raw = (camera.label || '').trim();
  const lower = raw.toLowerCase();
  if (/back|rear|environment|后置|后面|背面/.test(lower)) return '后置摄像头';
  if (/front|user|前置|自拍/.test(lower)) return '前置摄像头';
  if (/external|usb|外接/.test(lower)) return '外接摄像头';
  if (raw) return raw;
  return `摄像头 ${index + 1}`;
};

// 懒加载 zxing-wasm（原生 ZXing 的 WASM 版本），仅在“从相册导入”时加载。
// wasm 通过 Vite 的 ?url 自托管，避免运行时依赖 jsdelivr CDN。
let zxingReaderPromise: Promise<typeof import('zxing-wasm/reader')> | null = null;
const getZxingReader = () => {
  if (!zxingReaderPromise) {
    zxingReaderPromise = (async () => {
      const [reader, wasm] = await Promise.all([
        import('zxing-wasm/reader'),
        import('zxing-wasm/reader/zxing_reader.wasm?url'),
      ]);
      reader.prepareZXingModule({ overrides: { locateFile: () => wasm.default } });
      return reader;
    })().catch((err) => {
      zxingReaderPromise = null;
      throw err;
    });
  }
  return zxingReaderPromise;
};

const FullScanner = () => {
  const navigate = useNavigate();
  const location = useLocation();
  const { user: currentUser } = useAuthStore();
  
  const [isExecuting, setIsExecuting] = useState(false);
  const [latestQrData, setLatestQrData] = useState<QrData | null>(null);
  // 上一次签到因二维码 enc 失效而被终止，需要提示用户重新扫码/粘贴
  const [qrExpired, setQrExpired] = useState(false);
  const [signStatuses, setSignStatuses] = useState<Record<number, Partial<SignStatusMessage>>>({});
  
  const [cameras, setCameras] = useState<CameraDevice[]>([]);
  const [selectedDeviceId, setSelectedDeviceId] = useState<string | null>(null);
  const [showCameraList, setShowCameraList] = useState(false);
  const [isStealthMode, setIsStealthMode] = useState(false);
  const [isCameraReady, setIsCameraReady] = useState(false);
  const [showLoadingOverlay, setShowLoadingOverlay] = useState(true);
  // 相机不可用/启动失败时的兜底状态：提前关闭加载遮罩，露出「相册」导入入口
  const [cameraError, setCameraError] = useState<string | null>(null);

  useEffect(() => {
    if (isCameraReady || cameraError) {
      const timer = setTimeout(() => {
        setShowLoadingOverlay(false);
      }, 5);
      return () => clearTimeout(timer);
    } else {
      setShowLoadingOverlay(true);
    }
  }, [isCameraReady, cameraError]);

  // 签到位置：内存 store 负责组件同步，localStorage 负责下次进入页面直接复用
  const pickedLocation = useSignStore((state) => state.location);
  const locationDecided = useSignStore((state) => state.decided);
  const setPickedLocation = useSignStore((state) => state.setLocation);
  const clearPickedLocation = useSignStore((state) => state.clearLocation);
  const [isLocationPickerOpen, setIsLocationPickerOpen] = useState(false);
  const [isMapPickerOpen, setIsMapPickerOpen] = useState(false);
  const isMapPickerOpenRef = useRef(false);
  const isLocationSheetOpenRef = useRef(false);
  // 扫码时还没选点：暂存二维码，用户选完点后立即提交
  const pendingQrRef = useRef<QrData | null>(null);
  // 用户主动取消过地图选点后，不再自动弹窗，避免每次都打断扫码
  const skipAutoLocationPickerRef = useRef(false);
  const lastScanTimeRef = useRef<number>(0);
  const lastScanDetectedRef = useRef<number>(0);
  const lastScanFailToastRef = useRef<number>(0);
  const scanStartTimeRef = useRef<number>(Date.now());
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [isDecodingImage, setIsDecodingImage] = useState(false);

  useEffect(() => {
    isMapPickerOpenRef.current = isMapPickerOpen;
    isLocationSheetOpenRef.current = isLocationPickerOpen;
  }, [isMapPickerOpen, isLocationPickerOpen]);
  
  useEffect(() => {
    const handleFullScreen = async () => {
      try {
        if (isStealthMode) {
          if (!document.fullscreenElement) {
            await document.documentElement.requestFullscreen();
          }
        } else {
          if (document.fullscreenElement) {
            await document.exitFullscreen();
          }
        }
      } catch (err) {
        console.warn("Fullscreen toggle failed:", err);
      }
    };
    handleFullScreen();
  }, [isStealthMode]);

  useEffect(() => {
    return () => {
      if (document.fullscreenElement) {
        document.exitFullscreen().catch(() => {});
      }
    };
  }, []);

  const lastTouchDistance = useRef<number | null>(null);
  const pinchLockedByMultiTouch = useRef(false);
  const currentZoom = useRef<number>(1);
  const [displayZoom, setDisplayZoom] = useState(1);
  const [showZoomOverlay, setShowZoomOverlay] = useState(false);
  const zoomHideTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const readerRef = useRef<HTMLDivElement | null>(null);
  const scannerRef = useRef<HTMLDivElement>(null);

  // 窗口状态变化时强制重置，防止手势中断导致 UI 锁定
  useEffect(() => {
    const handleReset = () => {
      lastTouchDistance.current = null;
      setShowZoomOverlay(false);
    };
    window.addEventListener('blur', handleReset);
    window.addEventListener('visibilitychange', handleReset);
    return () => {
      window.removeEventListener('blur', handleReset);
      window.removeEventListener('visibilitychange', handleReset);
    };
  }, []);

  const isNativeBridgeModeRef = useRef(false);
  const [isNativeBridgeMode, setIsNativeBridgeMode] = useState(false);
  const [nativePreviewReady, setNativePreviewReady] = useState(false);
  const nativePreviewReadyRef = useRef(false);
  const selectedDeviceIdRef = useRef<string | null>(null);

  const { activity, selectedUids, classmates } = location.state || {};

  // 老师设置的签到圆心/半径（后端解析 getPPTActiveInfo 得到）。
  // 有值时地图会画半径圈；已选点若落在圈外，扫码后先引导重新选点。
  const signCenterLat = Number(activity?.location_latitude) || 0;
  const signCenterLng = Number(activity?.location_longitude) || 0;
  const signRangeMeters = Number(activity?.location_range) || 0;
  // 学习通只下发地址（locationText）时，圆心由地图选点页按地址反查得到，
  // 确认选点时通过 MapPickerResult.signLat/signLng 回传，用它做半径校验。
  const [resolvedSignCenter, setResolvedSignCenter] = useState<{ lat: number; lng: number } | null>(null);
  const centerLat = signCenterLat || resolvedSignCenter?.lat || 0;
  const centerLng = signCenterLng || resolvedSignCenter?.lng || 0;
  const pickedLocationOutOfRange = isLocationOutsideSignRange(
    pickedLocation,
    centerLat,
    centerLng,
    signRangeMeters
  );
  const latestQrDataRef = useRef<QrData | null>(null);
  /** 上一次圆心探测用掉的二维码 enc（刷新码的 enc 时效很短，每次探测都必须换一张新码） */
  const probeLastEncRef = useRef<string>('');
  /**
   * 当前是否真的有实时码流（原生相机，或 H5 摄像头可用且没报错）。
   * HTTP 手机页面拿不到摄像头权限时这里恒为 false，只能用「拍照 / 相册」一张一张导入，
   * 刷新码校准就不可能等到新码——探测要立刻暂停，让地图页保留进度等用户拍下一张。
   */
  const hasLiveCameraRef = useRef(false);
  const qrExpiredRef = useRef(false);
  const handlePasteRef = useRef<(event: ClipboardEvent) => void>(() => {});
  const isExecutingRef = useRef(false);
  const abortControllerRef = useRef<AbortController | null>(null);
  const scanSuccessHandlerRef = useRef<(decodedText: string) => void>(() => {});

  const getNativeBridge = (): NativeCameraBridge | null => {
    const bridge = (window as WindowWithBridge).XBTCameraBridge;
    if (!bridge) return null;
    try {
      if (typeof bridge.isReady === 'function' && !bridge.isReady()) return null;
    } catch {
      return null;
    }
    return bridge;
  };

  const syncReaderPunchHole = () => {
    const bridge = getNativeBridge();
    const reader = readerRef.current;
    if (!bridge || !reader || typeof bridge.syncPunchHole !== 'function') return;
    const rect = reader.getBoundingClientRect();
    bridge.syncPunchHole(rect.left, rect.top, rect.width, rect.height);
  };

  const setNativeLensFacing = (deviceId: string | null) => {
    const bridge = getNativeBridge();
    if (!bridge || typeof bridge.setLensFacing !== 'function' || !deviceId) return;
    const selected = cameras.find((c) => c.id === deviceId);
    const label = selected?.label?.toLowerCase() ?? '';
    const facing: 'user' | 'environment' = /front|user|前置|自拍/.test(label) ? 'user' : 'environment';
    bridge.setLensFacing(facing);
  };

  const orderedTargetUids = [currentUser?.uid, ...(selectedUids || [])].filter(Boolean) as number[];

  useEffect(() => {
    latestQrDataRef.current = latestQrData;
  }, [latestQrData]);

  useEffect(() => {
    qrExpiredRef.current = qrExpired;
  }, [qrExpired]);

  useEffect(() => {
    isExecutingRef.current = isExecuting;
  }, [isExecuting]);

  useEffect(() => {
    nativePreviewReadyRef.current = nativePreviewReady;
  }, [nativePreviewReady]);

  useEffect(() => {
    selectedDeviceIdRef.current = selectedDeviceId;
  }, [selectedDeviceId]);

  useEffect(() => {
    // 有没有实时码流：原生相机，或 H5 摄像头可用且没报错。
    // HTTP 手机页面拿不到摄像头权限时这里恒为 false，刷新码校准只能靠每次拍照导入新码。
    hasLiveCameraRef.current = isNativeBridgeMode || (cameras.length > 0 && !cameraError);
  }, [isNativeBridgeMode, cameras, cameraError]);

  useEffect(() => {
    setNativeLensFacing(selectedDeviceId);
  }, [selectedDeviceId, cameras]);

  useEffect(() => {
    if (!activity) return;
    let disposed = false;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let retryCount = 0;
    const maxRetry = 30;

    const activateNativeScanner = () => {
      if (disposed) return;
      const bridge = getNativeBridge();
      if (!bridge || typeof bridge.setScannerActive !== 'function') {
        if (retryCount < maxRetry) {
          retryCount += 1;
          timer = setTimeout(activateNativeScanner, 120);
        }
        return;
      }

      bridge.setScannerActive(true);
      setNativeLensFacing(selectedDeviceIdRef.current);

      if (!nativePreviewReadyRef.current && retryCount < maxRetry) {
        retryCount += 1;
        timer = setTimeout(activateNativeScanner, 150);
      }
    };

    activateNativeScanner();
    return () => {
      disposed = true;
      if (timer) clearTimeout(timer);
      const bridge = getNativeBridge();
      bridge?.setScannerActive?.(false);
    };
  }, [activity]);

  useEffect(() => {
    return () => {
      if (isExecutingRef.current) {
        isExecutingRef.current = false;
        abortControllerRef.current?.abort();
      }
    };
  }, []);

  const closePopup = () => {
    setIsExecuting(false);
    isExecutingRef.current = false;
    abortControllerRef.current?.abort();
    qrExpiredRef.current = false;
    setQrExpired(false);
    lastScanTimeRef.current = Date.now(); // 2s protection after close
  };

  useEffect(() => {
    if (isNativeBridgeModeRef.current) {
      setSelectedDeviceId('__native_environment__');
      setCameras([
        { id: '__native_environment__', label: '后置摄像头' } as CameraDevice,
        { id: '__native_user__', label: '前置摄像头' } as CameraDevice,
      ]);
      return;
    }

    Html5Qrcode.getCameras().then(devices => {
      if (devices && devices.length > 0) {
        const normalizedDevices = devices.map((d, index) => ({
          ...d,
          label: getFriendlyCameraLabel(d, index),
        }));
        setCameras(normalizedDevices);
        const backCamera = normalizedDevices.find(d =>
          /back|rear|environment|后置|后面|背面/.test(d.label.toLowerCase())
        );
        setSelectedDeviceId(backCamera ? backCamera.id : (normalizedDevices.length > 1 ? normalizedDevices[1].id : normalizedDevices[0].id));
      } else {
        setCameraError('未检测到摄像头，请点左下角「相册」导入二维码');
      }
    }).catch(err => {
      console.error("Error getting cameras", err);
      setCameraError('无法访问摄像头，请点左下角「相册」导入二维码');
    });
  }, []);

  useEffect(() => {
    const bridge = getNativeBridge();
    isNativeBridgeModeRef.current = !!bridge;
    setIsNativeBridgeMode(!!bridge);
    setNativePreviewReady(false);
    if (bridge) {
      setSelectedDeviceId('__native_environment__');
      setCameras([
        { id: '__native_environment__', label: '后置摄像头' } as CameraDevice,
        { id: '__native_user__', label: '前置摄像头' } as CameraDevice,
      ]);
    }
  }, []);

  useEffect(() => {
    if (!isNativeBridgeMode) return;

    const scannerRoot = readerRef.current?.parentElement ?? null;
    const touchedElements: Array<{ el: HTMLElement; bg: string }> = [];
    let cursor = scannerRoot;
    while (cursor) {
      touchedElements.push({ el: cursor, bg: cursor.style.backgroundColor });
      cursor.style.backgroundColor = 'transparent';
      cursor = cursor.parentElement;
    }

    const root = document.getElementById('root');
    const prevHtmlBg = document.documentElement.style.backgroundColor;
    const prevBodyBg = document.body.style.backgroundColor;
    const prevRootBg = root?.style.backgroundColor ?? '';

    document.documentElement.style.backgroundColor = 'transparent';
    document.body.style.backgroundColor = 'transparent';
    if (root) root.style.backgroundColor = 'transparent';

    return () => {
      touchedElements.forEach(({ el, bg }) => {
        el.style.backgroundColor = bg;
      });
      document.documentElement.style.backgroundColor = prevHtmlBg;
      document.body.style.backgroundColor = prevBodyBg;
      if (root) root.style.backgroundColor = prevRootBg;
    };
  }, [isNativeBridgeMode]);

  useEffect(() => {
    const onCameraState = (event: Event) => {
      if (!isNativeBridgeModeRef.current) return;
      const customEvent = event as CustomEvent<{ active?: boolean; error?: string }>;
      const isActive = !!customEvent.detail?.active;
      const error = customEvent.detail?.error?.trim() || '';
      if (!isActive && error === 'inactive') return;
      setNativePreviewReady(isActive);
      if (isActive || (error && error !== 'inactive')) setIsCameraReady(true);
    };
    window.addEventListener('xbt-native-camera-state', onCameraState);
    return () => {
      window.removeEventListener('xbt-native-camera-state', onCameraState);
    };
  }, []);

  // 兜底：部分机型在极端快切场景下可能丢失 camera-state 事件，轮询原生状态避免一直卡加载。
  useEffect(() => {
    if (!isNativeBridgeMode || nativePreviewReady) return;
    let disposed = false;
    let tickCount = 0;
    const maxTicks = 24; // 约 3.6s
    const timer = setInterval(() => {
      if (disposed) return;
      const bridge = getNativeBridge();
      if (!bridge || typeof bridge.getCameraState !== 'function') return;
      try {
        const raw = bridge.getCameraState();
        if (!raw) return;
        const state = JSON.parse(raw) as { active?: boolean; error?: string };
        const active = !!state.active;
        const error = (state.error || '').trim();
        if (active) {
          setNativePreviewReady(true);
          setIsCameraReady(true);
          return;
        }
        if (error && error !== 'inactive') {
          setIsCameraReady(true);
        }
      } catch {
        // ignore malformed payload
      }
      tickCount += 1;
      if (tickCount >= maxTicks) {
        clearInterval(timer);
      }
    }, 150);

    return () => {
      disposed = true;
      clearInterval(timer);
    };
  }, [isNativeBridgeMode, nativePreviewReady]);

  const activeScannerRef = useRef<Html5Qrcode | null>(null);
  const transitionPromise = useRef<Promise<void>>(Promise.resolve());
  const scannerSessionKey = isNativeBridgeMode ? '__native__' : (selectedDeviceId || '');

  useEffect(() => {
    if (!activity) {
      if (!activity) navigate('/');
      return;
    }
    if (!selectedDeviceId && !isNativeBridgeModeRef.current) return;

    const bridge = getNativeBridge();
    if (bridge) {
      setNativePreviewReady(false);
      setIsCameraReady(false);
      setNativeLensFacing(selectedDeviceId);
    }

    let isMounted = true;
    const safeStart = async () => {
      setIsCameraReady(false);
      transitionPromise.current = transitionPromise.current.then(async () => {
        try {
          const useNativeScanner = !!bridge;
          if (activeScannerRef.current) {
            const scanner = activeScannerRef.current;
            if (scanner.isScanning) await scanner.stop();
            const container = document.getElementById("reader");
            if (container) container.innerHTML = "";
            activeScannerRef.current = null;
          }

          if (!isMounted) return;
          if (useNativeScanner) {
            syncReaderPunchHole();
            return;
          }
          if (!selectedDeviceId) return;

          const html5QrCode = new Html5Qrcode("reader");
          activeScannerRef.current = html5QrCode;

          await html5QrCode.start(
            selectedDeviceId,
            {
              fps: 12,
              // 只解码取景框区域，避免整帧背景干扰，提升识别速度与准确率
              qrbox: (viewfinderWidth: number, viewfinderHeight: number) => {
                const edge = Math.floor(Math.min(viewfinderWidth, viewfinderHeight) * 0.6);
                return { width: edge, height: edge };
              },
              aspectRatio: 1.777778,
              videoConstraints: {
                deviceId: { exact: selectedDeviceId },
                width: { ideal: 1280 },
                height: { ideal: 720 },
                advanced: [{ focusMode: "continuous" } as any]
              }
            },
            onScanSuccess,
            onScanFailure
          );
          
          setIsCameraReady(true);
          setCameraError(null);

          if ('BarcodeDetector' in window) {
            const detector = new (window as any).BarcodeDetector({ formats: ['qr_code'] });
            const videoElement = document.querySelector("#reader video") as HTMLVideoElement;
            const nativeScanLoop = async () => {
              if (!isMounted || !activeScannerRef.current) return;
              if (videoElement && videoElement.readyState >= 2) {
                try {
                  const barcodes = await detector.detect(videoElement);
                  if (barcodes.length > 0) onScanSuccess(barcodes[0].rawValue);
                } catch (e) {}
              }
              requestAnimationFrame(nativeScanLoop);
            };
            nativeScanLoop();
          }
        } catch (err) {
          if (isMounted) {
            if (!String(err).includes("transition")) toast.error("相机启动失败，请重试");
            setCameraError('相机启动失败，请点左下角「相册」导入二维码');
          }
        }
      });
    };

    safeStart();
    return () => {
      isMounted = false;
      setNativePreviewReady(false);
      transitionPromise.current = transitionPromise.current.then(async () => {
        if (activeScannerRef.current) {
          const scanner = activeScannerRef.current;
          try { if (scanner.isScanning) await scanner.stop(); } catch (e) {}
          activeScannerRef.current = null;
          const container = document.getElementById("reader");
          if (container) container.innerHTML = "";
        }
      });
    };
  }, [activity, navigate, scannerSessionKey]);

  useEffect(() => {
    if (!isNativeBridgeModeRef.current) return;
    syncReaderPunchHole();
    const reader = readerRef.current;
    if (!reader) return;

    const observer = new ResizeObserver(() => syncReaderPunchHole());
    observer.observe(reader);
    window.addEventListener('resize', syncReaderPunchHole);
    window.addEventListener('scroll', syncReaderPunchHole, { passive: true });

    // 缩放
    let lastTouchDist = 0;
    const getTouchDist = (touches: TouchList) => {
      return Math.hypot(
        touches[0].clientX - touches[1].clientX,
        touches[0].clientY - touches[1].clientY
      );
    };

    const handleTouchStartNative = (e: TouchEvent) => {
      if (e.touches.length === 2) {
        lastTouchDist = getTouchDist(e.touches);
      }
    };

    const handleTouchMoveNative = (e: TouchEvent) => {
      if (e.touches.length === 2 && lastTouchDist > 0) {
        const currentDistance = getTouchDist(e.touches);
        const delta = (currentDistance - lastTouchDist) * 0.01;
        
        const bridge = getNativeBridge();
        if (bridge && typeof bridge.zoomByPinchDelta === 'function') {
          bridge.zoomByPinchDelta(delta);
        }
        lastTouchDist = currentDistance;
      }
    };

    const handleTouchEndNative = () => {
      lastTouchDist = 0;
    };

    reader.addEventListener('touchstart', handleTouchStartNative, { passive: true });
    reader.addEventListener('touchmove', handleTouchMoveNative, { passive: true });
    reader.addEventListener('touchend', handleTouchEndNative, { passive: true });

    return () => {
      observer.disconnect();
      window.removeEventListener('resize', syncReaderPunchHole);
      window.removeEventListener('scroll', syncReaderPunchHole);
      reader.removeEventListener('touchstart', handleTouchStartNative);
      reader.removeEventListener('touchmove', handleTouchMoveNative);
      reader.removeEventListener('touchend', handleTouchEndNative);
    };
  }, [isCameraReady]);

  useEffect(() => {
    if (!isNativeBridgeMode) return;
    const onNativeQr = (event: Event) => {
      const customEvent = event as CustomEvent<{ text?: string }>;
      const text = customEvent.detail?.text;
      if (typeof text === 'string' && text.length > 0) {
        scanSuccessHandlerRef.current(text);
      }
    };
    window.addEventListener('xbt-native-qr', onNativeQr);
    return () => {
      window.removeEventListener('xbt-native-qr', onNativeQr);
    };
  }, [isNativeBridgeMode]);

  const parseQrText = (text: string): QrData | null => {
    if (!text.includes("mobilelearn.chaoxing.com")) return null;
    try {
      const url = new URL(text);
      const enc = url.searchParams.get('enc');
      const c = url.searchParams.get('c');
      // 定时刷新二维码（widget/sign/e）会带查询策略，静态码没有。
      const dbStrategy = url.searchParams.get('DB_STRATEGY') || '';
      const strategyPara = url.searchParams.get('STRATEGY_PARA') || '';
      if (enc) return { enc, c: c || '', dbStrategy, strategyPara, timestamp: Date.now() };
    } catch (e) {
      const encMatch = text.match(/[?&]enc=([^&]+)/);
      const cMatch = text.match(/[?&]c=([^&]+)/);
      const dbMatch = text.match(/[?&]DB_STRATEGY=([^&]+)/);
      const paraMatch = text.match(/[?&]STRATEGY_PARA=([^&]+)/);
      if (encMatch) return {
        enc: encMatch[1],
        c: cMatch ? cMatch[1] : '',
        dbStrategy: dbMatch ? dbMatch[1] : '',
        strategyPara: paraMatch ? paraMatch[1] : '',
        timestamp: Date.now(),
      };
    }
    return null;
  };

  const handleExecute = async (initialQr: QrData) => {
    if (isExecutingRef.current) return;
    setIsExecuting(true);
    isExecutingRef.current = true;
    const controller = new AbortController();
    abortControllerRef.current = controller;

    // 扫码成功的瞬间同步读取已确认的签到位置：
    // 不依赖 React 渲染，也不发起任何定位请求，拿到 enc 后立即提交，抢在过期前完成。
    const submitLocation = useSignStore.getState().location;

    const initialStatuses: Record<number, any> = {};
    orderedTargetUids.forEach(uid => initialStatuses[uid] = { status: 'pending', message: '等待中' });
    setSignStatuses(initialStatuses);

    try {
      // 1. Check current sign status first
      const checkResp = await client.post<ApiResponse<{ items: any[] }>>('/sign/check', {
        activity_id: activity.active_id,
        user_ids: orderedTargetUids
      }, {
        signal: abortControllerRef.current?.signal
      });

      const checkItems = checkResp.data.data.items;
      const signedUids = new Set(checkItems.filter(item => item.signed).map(item => item.user_id));
      
      checkItems.forEach(item => {
        if (item.signed) {
          setSignStatuses(prev => ({ ...prev, [item.user_id]: { status: 'success', message: item.message || '已签到' } }));
        }
      });

      const toSignUids = orderedTargetUids.filter(uid => !signedUids.has(uid));
      if (toSignUids.length === 0) return;

      // 2. Concurrent execution with Retry Logic
      await Promise.all(toSignUids.map(async (uid) => {
        const MAX_RETRIES = 15;
        let lastError = '';
        for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
          if (!isExecutingRef.current) return;

          const currentQr = latestQrDataRef.current || initialQr;
          setSignStatuses(prev => ({ 
            ...prev, 
            [uid]: { 
              ...prev[uid], 
              status: attempt === 0 ? 'signing' : 'retrying', 
              attempt, 
              message: attempt === 0 ? '正在尝试签到' : (prev[uid]?.message || `准备重试(${attempt})`)
            } 
          }));
          try {
            const special_params: Record<string, any> = { enc: currentQr.enc, c: currentQr.c };
            // 定时刷新二维码需要沿用链接里的查询策略（PRIMARY_KEY + id），静态码为空则不带。
            if (currentQr.dbStrategy) special_params.db_strategy = currentQr.dbStrategy;
            if (currentQr.strategyPara) special_params.strategy_para = currentQr.strategyPara;
            // 直接提交用户提前在地图上确认的坐标（GCJ-02），
            // 不再依赖误差较大的自动定位。
            if (submitLocation) {
              special_params.latitude = submitLocation.lat;
              special_params.longitude = submitLocation.lng;
              special_params.description = submitLocation.description;
            }
            // 排查定位参数：打印本次真正提交给 /sign/execute 的 special_params。
            if (attempt === 0) {
              console.log('[sign] execute payload', JSON.stringify({
                uid,
                sign_type: 2,
                special_params,
                pickedLocation: submitLocation,
              }));
            }
            const execResp = await client.post<ApiResponse<SignExecuteResult>>('/sign/execute', {
              activity_id: activity.active_id, target_uid: uid, sign_type: 2,
              course_id: activity.course_id, class_id: activity.class_id, if_refresh_ewm: activity.if_refresh_ewm,
              special_params
            }, {
              signal: abortControllerRef.current?.signal
            });
            const res = execResp.data.data;
            // 二维码已失效：立即终止该同学的重试循环，避免用旧 enc 空转。
            if (res.code === SIGN_ERR_QR_EXPIRED) {
              qrExpiredRef.current = true;
              setQrExpired(true);
              lastError = res.message || '二维码已过期，请重新扫码或粘贴';
              setSignStatuses(prev => ({ ...prev, [uid]: { status: 'failed', message: lastError } }));
              toast.error('二维码已过期，请重新粘贴或扫码', { id: 'qr-expired', duration: 5000 });
              return;
            }
            if (res.success || res.already_signed) {
              setSignStatuses(prev => ({ ...prev, [uid]: { status: 'success', message: res.message || '成功' } }));
              return;
            }
            lastError = res.message || '失败';
            setSignStatuses(prev => ({ ...prev, [uid]: { ...prev[uid], message: lastError } }));
          } catch (err: any) { 
            if (err.name === 'CanceledError' || err.name === 'AbortError') return;
            lastError = err.message || '异常'; 
            setSignStatuses(prev => ({ ...prev, [uid]: { ...prev[uid], message: lastError } }));
          }
          if (attempt < MAX_RETRIES) {
            // 定时刷新二维码的 enc 只有几秒有效期：固定等待 1~2s 会直接吃掉刷新窗口。
            // 刷新码立即用下一次读数里的最新 enc 重试（留 150ms 防止空转）；
            // 静态码保持原有退避，避免对学习通造成无意义压力。
            const isRefreshQr = !!currentQr.c || !!activity.if_refresh_ewm;
            const delay = isRefreshQr ? 150 : (attempt < 3 ? 1000 : 2000);
            for (let i = 0; i < delay; i += 100) {
              if (!isExecutingRef.current) return;
              await new Promise(resolve => setTimeout(resolve, 100));
            }
          } else setSignStatuses(prev => ({ ...prev, [uid]: { status: 'failed', message: lastError } }));
        }
      }));
    } catch (error: any) {
      if (error.name !== 'CanceledError' && error.name !== 'AbortError') {
        toast.error(error.message || '执行过程出错');
      }
    } finally {
      // 仅清理本轮自己的 controller，避免把新一轮的 controller 置空
      if (abortControllerRef.current === controller) {
        abortControllerRef.current = null;
      }
    }
  };

  // 地图选点确认：写入 store（同时落到 localStorage）；
  // 若扫码时因尚未选点被挂起，选完立即提交，缩短 enc 的过期窗口。
  const handleMapLocationConfirm = (result: MapPickerResult) => {
    skipAutoLocationPickerRef.current = false;
    if (Number(result.signLat) > 0 && Number(result.signLng) > 0) {
      // 后端没下发经纬度、由地址反查得到的圆心，留在这里继续做半径校验
      setResolvedSignCenter({ lat: Number(result.signLat), lng: Number(result.signLng) });
    }
    setPickedLocation({
      name: result.name,
      lat: result.lat,
      lng: result.lng,
      description: result.description,
    });
    setIsMapPickerOpen(false);
    setIsLocationPickerOpen(false);
    toast.success('已设置签到位置', { id: 'sign-location' });

    const pending = pendingQrRef.current;
    pendingQrRef.current = null;
    if (pending) {
      handleExecute(pending);
    }
  };

  /**
   * 兼容旧后端：从返回文案里兜底解析“距签到圆心多少米”。
   * 新后端会直接给 location_distance_meters；旧后端只有原文
   * （errorLocation2_1137.0）或翻译后的“距签到点约 1137 米”。
   */
  const parseDistanceFromText = (text?: string): number | null => {
    if (!text) return null;
    const raw = /errorLocation\d*_(\d+(?:\.\d+)?)/i.exec(text);
    if (raw) {
      const value = Number(raw[1]);
      if (Number.isFinite(value) && value > 0) return value;
    }
    const translated = /距签到点约\s*(\d+)\s*米/.exec(text);
    if (translated) {
      const value = Number(translated[1]);
      if (Number.isFinite(value) && value > 0) return value;
    }
    return null;
  };

  /**
   * 等摄像头扫到一张「还没用过」的二维码（刷新码校准专用）。
   * 刷新码每张新码都会换 enc，且 enc 只有约 5~10 秒有效期；三边定位要连续提交
   * 三次，只能每次都用一张刚扫到的新码。
   *
   * 返回 null 表示「手上没有可用新码」，由上层决定暂停续传还是放弃：
   *   - 面板已关闭；
   *   - 没有实时码流（HTTP 手机只能拍照 / 相册，每次拍完才有一张新码）；
   *   - 有实时码流但超时内没扫到新码。
   * 拍照模式下重新打开面板时，latestQrDataRef 已经是刚导入的那张新码，会直接命中。
   */
  const waitForFreshEnc = async (excludeEnc: string, timeoutMs = 15000): Promise<QrData | null> => {
    if (!isMapPickerOpenRef.current) return null;

    // 先看手上这张是不是「还没用过的新码」：拍照 / 相册导入后重新打开面板时，
    // 最新二维码已经换了 enc，直接用它，不必再等实时码流。
    const seen = latestQrDataRef.current || pendingQrRef.current;
    if (seen?.enc && seen.enc !== excludeEnc) return seen;

    // 没有实时码流（HTTP 手机只能用拍照 / 相册）：等不到新码，
    // 交给上层暂停本轮校准，用户拍下一张后回来续传。
    if (!hasLiveCameraRef.current) return null;

    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      if (!isMapPickerOpenRef.current) return null;
      const qr = latestQrDataRef.current || pendingQrRef.current;
      if (qr?.enc && qr.enc !== excludeEnc) return qr;
      await new Promise<void>((resolve) => window.setTimeout(resolve, 300));
    }
    return null;
  };

  /**
   * 圆心探测：真实提交一次坐标，只为拿到学习通返回的“距签到圆心多少米”
   * （errorLocation2_<米>，后端已解析成 location_distance_meters）。
   *
   * 学习通不下发圆心经纬度，这个距离是唯一精确标尺；地图选点里的
   * “自动校准圆心”用它做三边定位反解真实圆心。探测点一般都在范围外，
   * 万一恰好落在范围内就等于直接签到成功，如实返回 signed。
   *
   * 刷新码（if_refresh_ewm，或二维码带 c 参数）特殊处理：enc 只能撑几秒，
   * 每次探测前都等一张新码，遇到 2001 二维码失效就换新码重试，否则第三次
   * 探测必然拿着过期 enc 提交、整轮校准作废。
   */
  const probeSignCenterDistance = async (point: { lng: number; lat: number }) => {
    const targetUid = orderedTargetUids[0];
    if (!activity || !targetUid) {
      return { signed: false, distance: null, message: '还没有拿到二维码或账号信息，无法校准' };
    }
    const isRefreshQr = !!activity.if_refresh_ewm || !!latestQrDataRef.current?.c;
    const maxAttempts = isRefreshQr ? 2 : 1;
    let lastMessage = '';

    /** 用当前手中的二维码真实提交一次，只取回「是否已签到 / 距圆心多少米」 */
    const submitWith = async (qr: QrData) => {
      const params: Record<string, any> = {
        enc: qr.enc,
        latitude: formatCoord(point.lat),
        longitude: formatCoord(point.lng),
        // 必须带上地址：学习通对空地址偶尔只回不带距离的 errorLocationX，
        // 那样就解不出圆心；正常能回 errorLocation2_<米> 的提交都带完整地址。
        description: activity.location_address || '',
      };
      if (qr.c) params.c = qr.c;
      if (qr.dbStrategy) params.db_strategy = qr.dbStrategy;
      if (qr.strategyPara) params.strategy_para = qr.strategyPara;

      const resp = await client.post<ApiResponse<SignExecuteResult>>('/sign/execute', {
        activity_id: activity.active_id,
        target_uid: targetUid,
        sign_type: 2,
        course_id: activity.course_id,
        class_id: activity.class_id,
        if_refresh_ewm: activity.if_refresh_ewm,
        special_params: params,
      });
      return resp.data.data;
    };

    for (let attempt = 1; attempt <= maxAttempts; attempt += 1) {
      let qr: QrData | null = null;
      if (isRefreshQr) {
        // 排除上一次用过（或已过期）的 enc，等一张新码。
        qr = await waitForFreshEnc(probeLastEncRef.current);
        if (!qr?.enc) {
          // 面板已关闭：不要再补一次真实提交
          if (!isMapPickerOpenRef.current) {
            return { signed: false, distance: null, refreshQr: true, retryable: false, message: '已取消校准' };
          }
          // 手上没有可用新码（拍照 / 相册模式，或摄像头没对着二维码）：
          // 暂停本轮校准，让地图页保留已采集的采样点，用户拍下一张后回来续传。
          return {
            signed: false,
            distance: null,
            refreshQr: true,
            retryable: false,
            needQr: true,
            message: '需要一张新的二维码，请拍摄/导入下一张后继续校准',
          };
        }
      } else {
        qr = latestQrDataRef.current || pendingQrRef.current;
      }
      if (!qr?.enc) {
        return { signed: false, distance: null, refreshQr: isRefreshQr, message: '还没有拿到二维码或账号信息，无法校准' };
      }
      probeLastEncRef.current = qr.enc;

      const res = await submitWith(qr);
      const signed = !!(res.success || res.already_signed);
      const distance =
        typeof res.location_distance_meters === 'number'
          ? res.location_distance_meters
          : parseDistanceFromText(res.message);
      console.info(
        '[sign] 圆心探测:',
        `${point.lng},${point.lat}`,
        `signed=${signed}`,
        `distance=${distance}`,
        res.message
      );

      // 刷新码：这张 enc 已过期，换一张新码再来（过期提交不会被记成签到）
      if (isRefreshQr && res.code === SIGN_ERR_QR_EXPIRED) {
        lastMessage = res.message || '二维码已过期';
        if (attempt < maxAttempts) {
          console.info('[sign] 圆心探测遇到二维码过期，换新码重试:', attempt);
          continue;
        }
        // 已换过一次新码仍过期（或没有实时码流拿不到新码）：暂停，等下一张照片再续传。
        return {
          signed: false,
          distance: null,
          refreshQr: true,
          retryable: false,
          needQr: true,
          message: '二维码已过期，请拍摄/导入一张新的二维码后继续校准',
        };
      }

      if (signed) {
        // 探测顺带签上了：同步面板状态，避免用户再提交一次。
        setSignStatuses((prev) => ({
          ...prev,
          [targetUid]: { status: 'success', message: res.message || '签到成功' },
        }));
      }
      return { signed, distance, refreshQr: isRefreshQr, message: res.message };
    }

    return {
      signed: false,
      distance: null,
      refreshQr: true,
      retryable: false,
      needQr: true,
      message: lastMessage || '二维码已过期，请拍摄/导入一张新的二维码后继续校准',
    };
  };

  // 统一入口：已明确选择过位置（含“不使用位置”）直接提交；
  // 否则暂存二维码并弹出地图选点，确认后立即提交。
  // 位置校验类活动（有老师圆心+半径）额外检查：已选点超出半径时先让用户重选，
  // 避免用必然失败的坐标反复重试。
  const ensureLocationAndExecute = (qr: QrData) => {
    const state = useSignStore.getState();
    const outsideRange = isLocationOutsideSignRange(
      state.location,
      centerLat,
      centerLng,
      signRangeMeters
    );

    if (state.decided && !outsideRange) {
      handleExecute(qr);
      return;
    }

    if (outsideRange) {
      skipAutoLocationPickerRef.current = false;
      toast(`上次选的位置超出本次签到范围，请重新选点`, { icon: '📍', id: 'sign-location-range' });
    }

    // 选点期间持续扫描到的新 enc 只更新挂起数据，避免重复弹窗
    pendingQrRef.current = qr;
    latestQrDataRef.current = qr;
    setLatestQrData(qr);

    // 用户取消过、或正在手动选择位置时，不再自动弹出地图
    if (skipAutoLocationPickerRef.current || isMapPickerOpenRef.current || isLocationSheetOpenRef.current) return;

    // 不清 probeLastEncRef：保留上一次探测用过的 enc，
    // 这样「没有新码就重新打开面板」会被识别为 needQr，避免拿同一张码重复提交。
    isMapPickerOpenRef.current = true;
    setIsMapPickerOpen(true);
  };

  // 从图片文件中解码二维码文本（相册导入与剪贴板图片共用）
  const decodeQrTextFromFile = async (file: File): Promise<string | null> => {
    const reader = await getZxingReader();
    const results = await reader.readBarcodes(file, {
      formats: ['QRCode'],
      tryHarder: true,
      tryRotate: true,
      tryInvert: true,
      tryDownscale: true,
      maxNumberOfSymbols: 1,
    });
    return results.find((r) => r.isValid && r.text)?.text || results.find((r) => r.text)?.text || null;
  };

  // 统一的二维码入口：扫码、相册导入、剪贴板粘贴都走这里。
  // 执行中只同步最新 enc（供重试循环使用）；若上一轮因 enc 失效被终止，则允许立即重开。
  const applyQrData = async (qr: QrData) => {
    const wasExpired = qrExpiredRef.current;
    qrExpiredRef.current = false;
    setQrExpired(false);

    lastScanTimeRef.current = 0;
    latestQrDataRef.current = qr;
    setLatestQrData(qr);

    if (isExecutingRef.current) {
      if (!wasExpired) return;
      abortControllerRef.current?.abort();
      setIsExecuting(false);
      isExecutingRef.current = false;
    }
    ensureLocationAndExecute(qr);
  };

  const onScanSuccess = (decodedText: string) => {
    const now = Date.now();
    const qr = parseQrText(decodedText);
    if (!qr) return;

    // 记录最后一次成功识别的时间，用于失败提示节流
    lastScanDetectedRef.current = now;

    // 关键修复：执行中也必须同步最新的 enc。
    // 重试循环每次都会读取 latestQrDataRef，二维码定时刷新（if_refresh_ewm）时
    // 若这里直接 return，新的 enc 永远进不来，重试只会一直使用旧 enc。
    if (!latestQrDataRef.current || latestQrDataRef.current.enc !== qr.enc) {
      const wasExpired = qrExpiredRef.current;
      latestQrDataRef.current = qr;
      setLatestQrData(qr);

      // 上一轮因 enc 失效被终止时，扫到新的 enc 就自动重开一轮
      if (wasExpired && isExecutingRef.current) {
        qrExpiredRef.current = false;
        setQrExpired(false);
        abortControllerRef.current?.abort();
        setIsExecuting(false);
        isExecutingRef.current = false;
        lastScanTimeRef.current = now;
        ensureLocationAndExecute(qr);
        return;
      }
    }

    // 只在非执行态、且满足 2s 节流时才触发一次新的签到流程
    if (isExecuting || isExecutingRef.current || (now - lastScanTimeRef.current < 2000)) return;
    lastScanTimeRef.current = now;
    ensureLocationAndExecute(qr);
  };

  useEffect(() => {
    scanSuccessHandlerRef.current = onScanSuccess;
  }, [onScanSuccess]);

  // 连续识别失败时的友好提示：前 6 秒不打扰，之后最多每 12 秒提示一次（同一 id 覆盖，不堆叠）
  const onScanFailure = () => {
    const now = Date.now();
    const sinceLastSuccess = now - Math.max(lastScanDetectedRef.current, scanStartTimeRef.current);
    if (sinceLastSuccess < 6000) return;
    if (now - lastScanFailToastRef.current < 12000) return;
    lastScanFailToastRef.current = now;
    toast('未识别到二维码：请对准取景框、靠近屏幕，或点左下角「相册」导入图片', {
      id: 'scan-fail-hint',
      icon: '🔍',
      duration: 4000,
    });
  };

  // 从相册导入二维码图片：用 zxing-wasm（原生 ZXing 的 WASM 版本）解码，
  // 对模糊、反光、复杂背景的鲁棒性远好于 jsQR。
  const handleImageImport = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = ''; // 允许重复选择同一张图片
    if (!file || isDecodingImage) return;

    setIsDecodingImage(true);
    const loadingToastId = toast.loading('正在识别图片中的二维码...');
    try {
      const text = await decodeQrTextFromFile(file);
      toast.dismiss(loadingToastId);

      if (!text) {
        toast.error('图片中未识别到二维码，请换一张更清晰的照片');
        return;
      }

      const qr = parseQrText(text);
      if (!qr) {
        toast.error('这不是有效的学习通签到二维码');
        return;
      }

      await applyQrData(qr);
    } catch (err) {
      toast.dismiss(loadingToastId);
      console.error('Image QR decode failed', err);
      const message = err instanceof Error ? err.message : '';
      toast.error(message || '图片识别失败，请重试');
    } finally {
      setIsDecodingImage(false);
    }
  };

  // 剪贴板粘贴：支持两种形态
  // 1) 直接复制二维码链接（text/plain）
  // 2) 截图/复制图片（image/*），用 zxing 解码
  const handlePaste = async (event: ClipboardEvent) => {
    const clipboard = event.clipboardData;
    if (!clipboard) return;

    const text = clipboard.getData('text/plain')?.trim();
    if (text) {
      const qr = parseQrText(text);
      if (qr) {
        event.preventDefault();
        toast.success('已从剪贴板识别二维码', { id: 'paste-qr' });
        await applyQrData(qr);
        return;
      }
    }

    const items = Array.from(clipboard.items || []);
    const imageItem = items.find((item) => item.kind === 'file' && item.type.startsWith('image/'));
    const file = imageItem?.getAsFile();
    if (!file) return;

    event.preventDefault();
    const loadingToastId = toast.loading('正在识别剪贴板图片...');
    try {
      const decoded = await decodeQrTextFromFile(file);
      toast.dismiss(loadingToastId);

      const qr = decoded ? parseQrText(decoded) : null;
      if (!qr) {
        toast.error('剪贴板图片中未识别到有效二维码', { id: 'paste-qr' });
        return;
      }
      toast.success('已从剪贴板图片识别二维码', { id: 'paste-qr' });
      await applyQrData(qr);
    } catch (err) {
      toast.dismiss(loadingToastId);
      console.error('Clipboard image decode failed', err);
      toast.error('剪贴板图片识别失败，请重试', { id: 'paste-qr' });
    }
  };

  useEffect(() => {
    handlePasteRef.current = handlePaste;
  }, [handlePaste]);

  // 注册一次全局 paste 监听，通过 ref 调用最新的处理函数，避免闭包读到旧状态。
  useEffect(() => {
    const onPaste = (event: ClipboardEvent) => handlePasteRef.current(event);
    window.addEventListener('paste', onPaste);
    return () => window.removeEventListener('paste', onPaste);
  }, []);

  const onTouchStart = (e: React.TouchEvent) => {
    // 如果上一轮被三指打断，新的触摸序列自动解锁，避免永久失效
    if (pinchLockedByMultiTouch.current && e.touches.length <= 2) {
      pinchLockedByMultiTouch.current = false;
      lastTouchDistance.current = null;
    }

    if (e.touches.length >= 3) {
      pinchLockedByMultiTouch.current = true;
      lastTouchDistance.current = null;
      return;
    }
    if (!pinchLockedByMultiTouch.current && e.touches.length === 2) {
      lastTouchDistance.current = Math.hypot(e.touches[0].pageX - e.touches[1].pageX, e.touches[0].pageY - e.touches[1].pageY);
    }
  };

  const onTouchMove = async (e: React.TouchEvent) => {
    if (e.touches.length >= 3) {
      pinchLockedByMultiTouch.current = true;
      lastTouchDistance.current = null;
      return;
    }
    if (pinchLockedByMultiTouch.current) {
      return;
    }

    if (e.touches.length === 2) {
      const distance = Math.hypot(e.touches[0].pageX - e.touches[1].pageX, e.touches[0].pageY - e.touches[1].pageY);
      
      // 自动校准初始距离
      if (lastTouchDistance.current === null) {
        lastTouchDistance.current = distance;
        return;
      }

      const zoomStep = (distance - lastTouchDistance.current) / 80;
      const bridge = getNativeBridge();

      try {
        if (bridge && typeof bridge.zoomByPinchDelta === 'function') {
          const nativeZoom = bridge.zoomByPinchDelta(zoomStep);
          if (Number.isFinite(nativeZoom)) {
            currentZoom.current = nativeZoom;
            setDisplayZoom(nativeZoom);
            setShowZoomOverlay(true);
            if (zoomHideTimerRef.current) clearTimeout(zoomHideTimerRef.current);
            lastTouchDistance.current = distance;
          }
          return;
        }

        const videoElement = document.querySelector("#reader video") as HTMLVideoElement;
        const stream = videoElement?.srcObject as MediaStream;
        const track = stream?.getVideoTracks()[0];

        if (!track) return;
        const capabilities = track.getCapabilities() as any;
        if (!capabilities.zoom) return;

        let newZoom = currentZoom.current + zoomStep;
        newZoom = Math.max(capabilities.zoom.min, Math.min(capabilities.zoom.max, newZoom));
        await track.applyConstraints({ advanced: [{ zoom: newZoom } as any] });
        currentZoom.current = newZoom;
        setDisplayZoom(newZoom);
        setShowZoomOverlay(true);
        if (zoomHideTimerRef.current) clearTimeout(zoomHideTimerRef.current);
        lastTouchDistance.current = distance;
      } catch (err) {
        console.error("Zoom apply failed", err);
      }
    } else {
      lastTouchDistance.current = null;
    }
  };

  const onTouchEnd = (e: React.TouchEvent) => {
    if (e.touches.length === 0) {
      pinchLockedByMultiTouch.current = false;
    }
    if (e.touches.length < 2) {
      lastTouchDistance.current = null;
      if (showZoomOverlay) {
        if (zoomHideTimerRef.current) clearTimeout(zoomHideTimerRef.current);
        zoomHideTimerRef.current = setTimeout(() => {
          setShowZoomOverlay(false);
        }, 500);
      }
    }
  };

  const getStatusColor = (status: string | undefined) => {
    switch (status) {
      case 'success': return '#10b981';
      case 'failed': return '#ff4d4d';
      case 'signing': return '#3399ff';
      case 'retrying': return '#ffcc00';
      default: return '#cbd5e1';
    }
  };

  const isAllSuccess = orderedTargetUids.length > 0 && orderedTargetUids.every(uid => signStatuses[uid]?.status === 'success');
  const formattedTime = (() => {
    const now = new Date();
    return `${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')} ${String(now.getHours()).padStart(2, '0')}:${String(now.getMinutes()).padStart(2, '0')}`;
  })();

  const stats = {
    signing: Object.values(signStatuses).filter(s => s.status === 'signing').length,
    success: Object.values(signStatuses).filter(s => s.status === 'success').length,
    retry: Object.values(signStatuses).filter(s => s.status === 'retrying').length,
    failed: Object.values(signStatuses).filter(s => s.status === 'failed').length
  };

  return (
    <div
      ref={scannerRef}
      className={`fixed inset-0 z-[100] flex flex-col overflow-hidden ${isNativeBridgeMode && nativePreviewReady ? 'bg-transparent' : 'bg-black'}`}
      onTouchStart={onTouchStart}
      onTouchMove={onTouchMove}
      onTouchEnd={onTouchEnd}
      onTouchCancel={onTouchEnd}
      data-native-bridge={isNativeBridgeMode ? '1' : '0'}
      data-native-ready={nativePreviewReady ? '1' : '0'}
    >
      <div id="reader" ref={readerRef} className="w-full h-full" />

      {/* 取景框：与 html5-qrcode 的 qrbox（短边 60%）保持对齐 */}
      <div className="absolute inset-0 z-10 flex items-center justify-center pointer-events-none">
        <div className="relative w-[60vmin] h-[60vmin]">
          <span className="absolute top-0 left-0 w-8 h-8 border-t-4 border-l-4 border-white/90 rounded-tl-lg" />
          <span className="absolute top-0 right-0 w-8 h-8 border-t-4 border-r-4 border-white/90 rounded-tr-lg" />
          <span className="absolute bottom-0 left-0 w-8 h-8 border-b-4 border-l-4 border-white/90 rounded-bl-lg" />
          <span className="absolute bottom-0 right-0 w-8 h-8 border-b-4 border-r-4 border-white/90 rounded-br-lg" />
        </div>
      </div>

      <input
        ref={fileInputRef}
        type="file"
        accept="image/*"
        className="hidden"
        onChange={handleImageImport}
      />

      {/* Zoom Multiplier Overlay */}
      <AnimatePresence>
        {showZoomOverlay && (
          <motion.div 
            initial={{ opacity: 0, scale: 0.9 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.9 }}
            transition={{ duration: 0.3 }}
            className="absolute top-[calc(24px+var(--sat))] right-4 z-[100] bg-black/40 backdrop-blur-sm border border-white/20 text-white font-bold py-1.5 px-3 rounded-full flex items-center shadow-lg"
          >
            <span className="text-[14px] tracking-widest">{displayZoom.toFixed(1)}x</span>
          </motion.div>
        )}
      </AnimatePresence>

      {/* Camera Loading Overlay */}
      <AnimatePresence>
        {showLoadingOverlay && (
          <motion.div 
            initial={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.3 }}
            className="absolute inset-0 z-[110] bg-slate-900 flex flex-col items-center justify-center"
          >
            <div className="relative">
              <div className="w-20 h-20 border-4 border-blue-500/20 border-t-blue-500 rounded-full animate-spin" />
              <Camera className="absolute inset-0 m-auto text-blue-500" size={32} />
            </div>
            <p className="mt-6 text-blue-400 font-bold tracking-widest text-sm animate-pulse px-8 text-center">正在启动相机...</p>
          </motion.div>
        )}
      </AnimatePresence>

      <div className="absolute top-0 left-0 right-0 z-20 px-4 pt-[calc(24px+var(--sat))] pb-6 flex items-center justify-center text-white pointer-events-none">
        <button onClick={() => navigate(-1)} className="top-[calc(24px+var(--sat))] left-4 absolute active:opacity-60 transition-opacity pointer-events-auto">
          <ChevronLeft size={32} strokeWidth={1.5} strokeLinecap="square" strokeLinejoin="miter" />
        </button>
        <h2 className="text-[20px] font-normal tracking-widest">扫一扫</h2>
      </div>

      {/* 当前签到位置：进入页面先确认，扫码成功后立即随 enc 提交 */}
      {!isStealthMode && !isExecuting && (
        <div className="absolute top-[calc(84px+var(--sat))] left-1/2 -translate-x-1/2 z-40 w-[88%]">
          <button
            type="button"
            onClick={() => {
              skipAutoLocationPickerRef.current = false;
              pendingQrRef.current = null;
              setIsLocationPickerOpen(true);
            }}
            className={`w-full flex items-center gap-2.5 px-4 py-2 rounded-2xl backdrop-blur-md border shadow-lg active:scale-[0.99] transition-transform ${pickedLocation ? 'bg-black/50 border-white/15 text-white' : 'bg-amber-500/95 border-amber-200/40 text-white'}`}
          >
            <MapPin size={18} className="shrink-0" />
            <span className="flex-1 min-w-0 text-left">
              <span className="block text-[9px] font-bold tracking-widest opacity-75 leading-tight">当前签到位置</span>
              <span className="block text-[13px] font-bold truncate leading-tight">
                {pickedLocation ? pickedLocation.name : (locationDecided ? '不使用位置' : '未设置，点击选择')}
              </span>
              {pickedLocationOutOfRange && (
                <span className="block text-[10px] font-bold text-amber-300 leading-tight">
                  超出 {signRangeMeters} 米签到范围，点击重选
                </span>
              )}
            </span>
            <span className="text-[10px] font-bold opacity-80 shrink-0">{pickedLocation ? '更换' : '选择'}</span>
          </button>
        </div>
      )}

      {/* 相机不可用提示：引导使用相册导入 */}
      <AnimatePresence>
        {cameraError && (
          <motion.div
            initial={{ opacity: 0, y: -8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -8 }}
            transition={{ duration: 0.2 }}
            className="absolute top-[calc(132px+var(--sat))] left-1/2 -translate-x-1/2 z-30 px-4 py-2 rounded-full bg-black/60 backdrop-blur-sm border border-white/15 text-white text-xs font-bold tracking-wide text-center max-w-[88%] pointer-events-none"
          >
            {cameraError}
          </motion.div>
        )}
      </AnimatePresence>

      {/* 二维码失效提示：引导用户重新粘贴/扫码 */}
      <AnimatePresence>
        {qrExpired && (
          <motion.div
            initial={{ opacity: 0, y: -8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -8 }}
            transition={{ duration: 0.2 }}
            className={`absolute left-1/2 -translate-x-1/2 z-40 w-[88%] rounded-2xl bg-rose-500/95 backdrop-blur-sm px-4 py-2.5 text-white shadow-lg pointer-events-none ${cameraError ? 'top-[calc(180px+var(--sat))]' : 'top-[calc(132px+var(--sat))]'}`}
          >
            <p className="text-xs font-bold">二维码已过期</p>
            <p className="text-[11px] opacity-90 mt-0.5">请重新复制二维码链接或截图后粘贴，也可点左下角「相册」导入</p>
          </motion.div>
        )}
      </AnimatePresence>

      <AnimatePresence>
        {!(isStealthMode && isAllSuccess) && (
          <motion.div 
            initial={{ opacity: 0 }} 
            animate={{ opacity: 1 }} 
            exit={{ opacity: 0 }} 
            className="absolute inset-0 pointer-events-none z-30 flex justify-center"
          >
            <motion.img 
              src={scanCursor} 
              className="w-full absolute" 
              initial={{ top: '5%', opacity: 0 }} 
              animate={{ 
                top: ['5%', '70%'], 
                opacity: [0, 1, 1, 0] 
              }} 
              transition={{ 
                top: {
                  duration: 2.5,
                  repeat: Infinity,
                  ease: "linear",
                },
                opacity: {
                  duration: 2.5,
                  repeat: Infinity,
                  times: [0, 0.1, 0.9, 1],
                  ease: "linear",
                }
              }} 
            />
          </motion.div>
        )}
      </AnimatePresence>

      <AnimatePresence initial={false}>
        {isExecuting && !isStealthMode && (
          <motion.div initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} className="fixed inset-0 z-30 flex items-end justify-center bg-black/20" onClick={closePopup}>
            <motion.div initial={{ y: '100%' }} animate={{ y: 0 }} exit={{ y: '100%' }} transition={{ type: 'spring', damping: 25, stiffness: 200 }} className="bg-white/70 backdrop-blur-md w-full rounded-t-[2rem] shadow-[0_-10px_40px_rgba(0,0,0,0.4)] border-t border-white/20 flex flex-col max-h-[40vh] overflow-hidden" onClick={e => e.stopPropagation()}>
              <div className="p-5 pt-7 shrink-0 flex items-center justify-between border-b border-black/5">
                <div className="flex flex-col">
                  <div className="flex items-center space-x-2">
                    <h3 className="text-lg font-black text-slate-900 tracking-tight">签到进度</h3>
                    <div className="flex items-center space-x-1">
                      <AnimatePresence>
                        {stats.signing > 0 && (
                          <motion.span 
                            initial={{ scale: 0.8, opacity: 0 }} 
                            animate={{ scale: 1, opacity: 1 }} 
                            exit={{ scale: 0.8, opacity: 0 }} 
                            className="text-[13px] font-black px-1.5 py-0 rounded-md border border-[#2563eb] text-[#2563eb] bg-white/80 whitespace-nowrap"
                          >
                            签到中 {stats.signing}
                          </motion.span>
                        )}
                        {stats.success > 0 && (
                          <motion.span 
                            initial={{ scale: 0.8, opacity: 0 }} 
                            animate={{ scale: 1, opacity: 1 }} 
                            exit={{ scale: 0.8, opacity: 0 }} 
                            className="text-[13px] font-black px-1.5 py-0 rounded-md border border-[#059669] text-[#059669] bg-white/80 whitespace-nowrap"
                          >
                            成功 {stats.success}
                          </motion.span>
                        )}
                        {stats.retry > 0 && (
                          <motion.span 
                            initial={{ scale: 0.8, opacity: 0 }} 
                            animate={{ scale: 1, opacity: 1 }} 
                            exit={{ scale: 0.8, opacity: 0 }} 
                            className="text-[13px] font-black px-1.5 py-0 rounded-md border border-[#d97706] text-[#d97706] bg-white/80 whitespace-nowrap"
                          >
                            重试 {stats.retry}
                          </motion.span>
                        )}
                        {stats.failed > 0 && (
                          <motion.span 
                            initial={{ scale: 0.8, opacity: 0 }} 
                            animate={{ scale: 1, opacity: 1 }} 
                            exit={{ scale: 0.8, opacity: 0 }} 
                            className="text-[13px] font-black px-1.5 py-0 rounded-md border border-[#e11d48] text-[#e11d48] bg-white/80 whitespace-nowrap"
                          >
                            失败 {stats.failed}
                          </motion.span>
                        )}
                      </AnimatePresence>
                    </div>
                  </div>
                  {latestQrData && <p className="text-[9px] text-blue-600 font-bold mt-0.5 tracking-tighter text-ellipsis">Enc: {latestQrData.enc}</p>}
                </div>
                <button 
                  onClick={() => isAllSuccess ? navigate(-1) : closePopup()} 
                  className={isAllSuccess ? "px-4 py-1.5 bg-blue-600 text-white rounded-full text-sm font-bold shadow-sm" : "w-8 h-8 flex items-center justify-center bg-slate-900/5 text-slate-400 rounded-full hover:bg-slate-900/10 transition-colors"}
                >
                  {isAllSuccess ? "完成" : "✕"}
                </button>
              </div>
              <div className="flex-1 overflow-y-auto p-5 pb-[calc(20px+var(--sab))] space-y-1 custom-scrollbar">
                {orderedTargetUids.map(uid => {
                  const isHost = uid === currentUser?.uid;
                  const student = isHost ? currentUser : (classmates as Classmate[])?.find(m => m.uid === uid);
                  return <ProgressCard key={uid} name={student?.name || "本人"} avatar={student?.avatar} mobile={isHost ? (student as User)?.mobile : (student as Classmate)?.mobile_masked || ""} isHost={isHost} statusObj={signStatuses[uid]} />;
                })}
              </div>
            </motion.div>
          </motion.div>
        )}
      </AnimatePresence>

      <AnimatePresence initial={false}>
        {isExecuting && isStealthMode && (
          <motion.div initial={{ opacity: 0, y: 10 }} animate={{ opacity: 1, y: 0 }} exit={{ opacity: 0, y: 10 }} className="absolute bottom-10 left-0 right-0 z-40 flex justify-center items-center space-x-4">
            {orderedTargetUids.map(uid => (
              <motion.div key={uid} animate={{ backgroundColor: getStatusColor(signStatuses[uid]?.status), scale: signStatuses[uid]?.status === 'signing' || signStatuses[uid]?.status === 'retrying' ? [1, 1.4, 1] : 1 }} transition={{ repeat: signStatuses[uid]?.status === 'signing' || signStatuses[uid]?.status === 'retrying' ? Infinity : 0, duration: 1 }} className="w-1 h-1 rounded-full shadow-[0_0_5px_rgba(0,0,0,0.5)]" />
            ))}
          </motion.div>
        )}
      </AnimatePresence>

      <div className="absolute bottom-[calc(48px+var(--sab))] left-8 right-8 z-20 flex items-center justify-between pointer-events-none">
        <div className="flex items-center space-x-4">
          <AnimatePresence initial={false}>
            {!isStealthMode && !isExecuting && (
              <>
                <motion.button 
                  initial={{ x: -120, opacity: 0 }}
                  animate={{ x: 0, opacity: 1 }}
                  exit={{ x: -120, opacity: 0 }}
                  transition={{ ease: "easeInOut", duration: 0.3, delay: 0.1 }}
                  type="button" 
                  onClick={() => {
                    skipAutoLocationPickerRef.current = false;
                    pendingQrRef.current = null;
                    setIsLocationPickerOpen(true);
                  }} 
                  className="flex flex-col items-center space-y-2 group pointer-events-auto active:scale-95 transition-transform"
                >
                  <div className={`w-14 h-14 rounded-full flex items-center justify-center backdrop-blur-md border border-white/10 shadow-lg transition-colors ${pickedLocation ? 'bg-blue-600 text-white' : 'bg-black/40 text-white'}`}>
                    <MapPin size={22} />
                  </div>
                  <span className="text-[10px] text-white/80 font-bold tracking-wider truncate max-w-[60px]">
                    {pickedLocation ? pickedLocation.name : (locationDecided ? "不使用" : "位置")}
                  </span>
                </motion.button>

                <motion.button 
                  initial={{ x: -120, opacity: 0 }}
                  animate={{ x: 0, opacity: 1 }}
                  exit={{ x: -120, opacity: 0 }}
                  transition={{ ease: "easeInOut", duration: 0.3 }}
                  type="button" 
                  onClick={() => setShowCameraList(true)} 
                  className="flex flex-col items-center space-y-2 group pointer-events-auto active:scale-95 transition-transform"
                >
                  <div className="w-14 h-14 bg-black/40 rounded-full flex items-center justify-center backdrop-blur-md border border-white/10 text-white shadow-lg"><Camera size={22} /></div>
                  <span className="text-[10px] text-white/80 font-bold tracking-wider">切换</span>
                </motion.button>

                <motion.button
                  initial={{ x: -120, opacity: 0 }}
                  animate={{ x: 0, opacity: 1 }}
                  exit={{ x: -120, opacity: 0 }}
                  transition={{ ease: "easeInOut", duration: 0.3 }}
                  type="button"
                  onClick={() => fileInputRef.current?.click()}
                  disabled={isDecodingImage}
                  aria-label="从相册导入二维码"
                  className="flex flex-col items-center space-y-2 group pointer-events-auto active:scale-95 transition-transform disabled:opacity-50"
                >
                  <div className="w-14 h-14 bg-black/40 rounded-full flex items-center justify-center backdrop-blur-md border border-white/10 text-white shadow-lg">
                    {isDecodingImage ? <Loader2 size={22} className="animate-spin" /> : <ImagePlus size={22} />}
                  </div>
                  <span className="text-[10px] text-white/80 font-bold tracking-wider">相册</span>
                </motion.button>
              </>
            )}
          </AnimatePresence>
        </div>
        <button type="button" onClick={() => setIsStealthMode(!isStealthMode)} className="flex flex-col items-center space-y-2 transition-all duration-500 pointer-events-auto active:scale-95" style={{ opacity: isStealthMode ? 0.1 : 1 }}>
          <div className="w-14 h-14 bg-black/40 rounded-full flex items-center justify-center backdrop-blur-md border border-white/10 text-white shadow-lg">{isStealthMode ? <Eye size={22} /> : <EyeOff size={22} />}</div>
          <span className="text-[10px] text-white/80 font-bold tracking-wider">{isStealthMode ? "显示" : "隐藏"}</span>
        </button>
      </div>

      <AnimatePresence>
        {isLocationPickerOpen && (
          <motion.div initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} className="fixed inset-0 z-[105] flex items-end justify-center bg-slate-900/60 backdrop-blur-md p-0" onClick={() => setIsLocationPickerOpen(false)}>
            <motion.div initial={{ y: '100%' }} animate={{ y: 0 }} exit={{ y: '100%' }} transition={{ type: 'spring', damping: 28, stiffness: 250 }} className="bg-white w-full max-w-[480px] rounded-t-[3rem] p-8 shadow-2xl overflow-hidden flex flex-col max-h-[80vh]" onClick={(e) => e.stopPropagation()}>
              <div className="w-12 h-1.5 bg-slate-200 rounded-full mx-auto mb-8 shrink-0" />
              <div className="flex items-center justify-between mb-6 shrink-0"><h3 className="text-xl font-bold text-slate-900">选择签到位置</h3><button onClick={() => setIsLocationPickerOpen(false)} className="w-8 h-8 flex items-center justify-center bg-slate-100 text-slate-400 rounded-full">✕</button></div>
              <button
                type="button"
                onClick={() => {
                  skipAutoLocationPickerRef.current = false;
                  // 不清 probeLastEncRef：续传校准要用它判断手上这张码是不是「新码」
                  setIsMapPickerOpen(true);
                }}
                className="w-full mb-4 py-3 rounded-2xl bg-blue-600 text-white font-bold text-sm flex items-center justify-center gap-2 shadow-lg shadow-blue-600/20 active:scale-[0.98] transition-transform shrink-0"
              >
                <MapPin size={18} />
                高德地图选点
              </button>
              <div className="flex-1 overflow-y-auto space-y-3 pr-1 pb-[calc(40px+var(--sab))] custom-scrollbar px-1">
                <motion.div
                  whileTap={{ scale: 0.98 }}
                  onClick={() => { clearPickedLocation(); setIsLocationPickerOpen(false); }}
                  className={`p-5 rounded-[1.5rem] border-2 transition-all cursor-pointer flex items-center justify-between ${locationDecided && !pickedLocation ? 'border-blue-500 bg-blue-50/30' : 'border-slate-50 bg-slate-100/50 hover:bg-white'}`}
                >
                  <div className="flex-1 min-w-0 pr-4">
                    <div className="font-bold text-slate-800 text-sm">不使用位置</div>
                    <div className="text-[10px] text-slate-400 font-medium">不发送地理位置信息进行签到</div>
                  </div>
                  {locationDecided && !pickedLocation && <CheckCircle2 size={20} className="text-blue-600 shrink-0" />}
                </motion.div>

                {LOCATION_PRESETS.map((p: any, i: number) => {
                  const isSelected = pickedLocation?.lat === p.lat && pickedLocation?.lng === p.lng;
                  return (
                    <motion.div key={i} whileTap={{ scale: 0.98 }} onClick={() => { setPickedLocation({ name: p.name, lat: p.lat, lng: p.lng, description: p.description }); setIsLocationPickerOpen(false); }} className={`p-5 rounded-[1.5rem] border-2 transition-all cursor-pointer flex items-center justify-between ${isSelected ? 'border-blue-500 bg-blue-50/30' : 'border-slate-50 bg-slate-50/50 hover:bg-white'}`}>
                      <div className="flex-1 min-w-0 pr-4"><div className="font-bold text-slate-800 mb-0.5 text-sm">{p.name}</div><div className="text-[10px] text-slate-400 font-medium truncate">{p.description}</div></div>
                      {isSelected && <CheckCircle2 size={20} className="text-blue-600 shrink-0" />}
                    </motion.div>
                  );
                })}</div>
            </motion.div>
          </motion.div>
        )}
      </AnimatePresence>

      <MapPicker
        open={isMapPickerOpen}
        initialLng={pickedLocationOutOfRange ? undefined : pickedLocation?.lng}
        initialLat={pickedLocationOutOfRange ? undefined : pickedLocation?.lat}
        initialName={pickedLocationOutOfRange ? undefined : pickedLocation?.name}
        initialDescription={pickedLocationOutOfRange ? undefined : pickedLocation?.description}
        signLng={activity?.location_longitude}
        signLat={activity?.location_latitude}
        signRange={activity?.location_range}
        signAddress={activity?.location_address}
        refreshQr={!!activity?.if_refresh_ewm || !!latestQrData?.c}
        onProbeDistance={probeSignCenterDistance}
        onClose={() => {
          setIsMapPickerOpen(false);
          // 取消兜底选点：丢弃挂起的二维码，避免用旧 enc 继续提交，
          // 同时不再自动弹窗，等用户主动点「位置」再选。
          skipAutoLocationPickerRef.current = true;
          pendingQrRef.current = null;
        }}
        onConfirm={handleMapLocationConfirm}
      />

      <AnimatePresence initial={false}>
        {isStealthMode && isAllSuccess && (
          <motion.div 
            initial={{ opacity: 1 }} 
            animate={{ opacity: 1 }} 
            exit={{ opacity: 0 }}
            transition={{ duration: 0 }}
            className="fixed inset-0 bg-white z-[200] flex flex-col font-sans pt-[var(--sat)] pb-[var(--sab)]"
          >
            <div className="bg-white h-[44px] flex items-center px-1 shrink-0">
              <button onClick={() => navigate(-1)} className="p-2">
                <ChevronLeft size={33} className="text-[#333]" strokeWidth={1.3} />
              </button>
              <div className="flex-1 text-center text-[21px] text-[#333] pr-10">
                签到
              </div>
            </div>

            <div className="flex-1 bg-white flex flex-col pt-0 justify-center">
              <div className="flex-1"></div>
              <div className="flex-4 bg-white px-5 pt-12 pb-8 flex flex-col items-center">
                <div className="w-[50px] h-[50px] mb-5">
                  <img 
                    src="https://mobilelearn-static.chaoxing.com/mobilelearn/front/mobile/sign/images/sign-icon-succeed-green.png" 
                    className="w-full h-full object-contain"
                    alt="success"
                  />
                </div>
                <h1 className="text-[26px] text-[#666666] mb-2 leading-none">
                  签到成功
                </h1>
                <p className="text-[17px] text-[#B3B3B3] signtime pt-1">{formattedTime}</p>
              </div>
            </div>
          </motion.div>
        )}
      </AnimatePresence>

      <AnimatePresence initial={false}>
        {showCameraList && (
          <motion.div initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} className="fixed inset-0 z-[120] flex items-end justify-center bg-black/60 backdrop-blur-sm p-4 pointer-events-auto" onClick={() => setShowCameraList(false)}>
            <motion.div initial={{ y: '100%' }} animate={{ y: 0 }} exit={{ y: '100%' }} className="bg-white w-full max-w-sm rounded-[2rem] p-6 shadow-2xl relative" onClick={e => e.stopPropagation()}>
              <h3 className="text-lg font-black mb-4 text-slate-900">选择摄像头</h3>
              <div className="space-y-2 max-h-[40vh] overflow-y-auto custom-scrollbar">
                {cameras.map(camera => (
                  <button key={camera.id} type="button" onClick={() => { setSelectedDeviceId(camera.id); setShowCameraList(false); }} className={`w-full p-4 rounded-xl text-left font-bold transition-all flex items-center justify-between ${selectedDeviceId === camera.id ? 'bg-blue-600 text-white' : 'bg-slate-100 text-slate-700 hover:bg-slate-200'}`}>
                    <span className="truncate">{camera.label || `摄像头 ${camera.id.substring(0, 5)}`}</span>
                    {selectedDeviceId === camera.id && <div className="w-2 h-2 bg-white rounded-full" />}
                  </button>
                ))}
              </div>
              <button type="button" onClick={() => setShowCameraList(false)} className="w-full mt-4 py-3 text-slate-400 font-bold">取消</button>
            </motion.div>
          </motion.div>
        )}
      </AnimatePresence>

      <style>{`
        #reader__dashboard, #reader__status_span, #reader img { display: none !important; }
        #reader video { object-fit: cover !important; width: 100% !important; height: 100% !important; }
        #reader { background: black !important; }
        [data-native-bridge="1"][data-native-ready="1"] #reader { background: transparent !important; }
        [data-native-bridge="1"][data-native-ready="1"] #reader video { opacity: 0 !important; }
        .custom-scrollbar::-webkit-scrollbar { width: 0px; }
      `}</style>
    </div>
  );
};

export default FullScanner;
