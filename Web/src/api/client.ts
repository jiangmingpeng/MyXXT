import axios from 'axios';
import type { ApiResponse } from '../types';
import { useAuthStore } from '../store/auth';
import config from '../../config.yaml';

const client = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL || config.api?.base_url || '/api',
  timeout: config.api?.timeout || 10000,
});

/**
 * 签到类接口的单独超时。
 *
 * 一次二维码签到在后端要串行完成「预签到（preSign/analysis/analysis2）→
 * 验证码识别 → stuSignajax」，经过代理往返后正常耗时就有 6~15s。
 * 用默认的 10s 会出现「后端还在正常执行、前端已经报 timeout of 10000ms exceeded」，
 * 用户重试还会用掉同一张只能用一次的 enc。
 */
export const SIGN_TIMEOUT_MS = (() => {
  const raw = Number(config.api?.sign_timeout);
  if (!Number.isFinite(raw) || raw <= 0) return 25000;
  return Math.min(120000, Math.max(10000, Math.floor(raw)));
})();

client.interceptors.request.use((config) => {
  const token = useAuthStore.getState().token;
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

client.interceptors.response.use(
  (response) => {
    const res = response.data as ApiResponse<any>;
    if (res.code !== 0) {
      return Promise.reject(new Error(res.message || '操作失败'));
    }
    return response;
  },
  (error) => {
    if (error.response?.status === 401) {
      useAuthStore.getState().logout();
      window.location.href = '/#/login';
      return Promise.reject(new Error(error.response?.data?.message || '登录已失效，请重新登录'));
    }
    if (error.response?.data?.message) {
      return Promise.reject(new Error(error.response.data.message));
    }
    return Promise.reject(error);
  }
);

export default client;
