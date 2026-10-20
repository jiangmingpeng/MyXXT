import { create } from 'zustand';

/**
 * 已确认的签到位置（GCJ-02）。
 *
 * 学习通的二维码 enc 失效极快，所以位置必须在扫码前就准备好：
 * - 内存 store 负责组件之间的同步读取（handleExecute 里用 getState() 同步取）；
 * - localStorage 负责下次进入扫码页时直接复用上次选好的点，
 *   同一间教室无需重复选点。
 */
export interface SignLocation {
  name: string;
  lat: string;
  lng: string;
  description: string;
}

const STORAGE_KEY = 'sign_location';

interface StoredSignLocation {
  location: SignLocation | null;
  /** 用户是否已经明确做过选择（选点，或明确选择“不使用位置”） */
  decided: boolean;
}

const loadStoredLocation = (): StoredSignLocation => {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { location: null, decided: false };

    const parsed = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object') return { location: null, decided: false };

    // 明确选择“不使用位置”
    if (parsed.disabled === true) return { location: null, decided: true };

    const lat = typeof parsed.lat === 'string' ? parsed.lat.trim() : '';
    const lng = typeof parsed.lng === 'string' ? parsed.lng.trim() : '';
    if (!lat || !lng || !Number.isFinite(Number(lat)) || !Number.isFinite(Number(lng))) {
      return { location: null, decided: false };
    }

    const name = typeof parsed.name === 'string' && parsed.name.trim() ? parsed.name.trim() : '地图选点';
    const description =
      typeof parsed.description === 'string' && parsed.description.trim()
        ? parsed.description.trim()
        : name;

    return { location: { name, lat, lng, description }, decided: true };
  } catch {
    // 缓存损坏时按“未选择”处理，避免把脏坐标提交给学习通
    return { location: null, decided: false };
  }
};

interface SignState extends StoredSignLocation {
  setLocation: (location: SignLocation) => void;
  /** 明确选择“不使用位置” */
  clearLocation: () => void;
}

const initial = loadStoredLocation();

export const useSignStore = create<SignState>((set) => ({
  ...initial,

  setLocation: (location) => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(location));
    set({ location, decided: true });
  },

  clearLocation: () => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify({ disabled: true }));
    set({ location: null, decided: true });
  },
}));
