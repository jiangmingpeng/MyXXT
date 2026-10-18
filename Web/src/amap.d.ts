export {};

declare global {
  interface Window {
    /**
     * 高德 JS API 2.0 安全密钥配置。
     * 2021-12 之后申请的 Key 必须在加载 SDK 前设置 securityJsCode。
     */
    _AMapSecurityConfig?: {
      securityJsCode?: string;
    };
  }
}
