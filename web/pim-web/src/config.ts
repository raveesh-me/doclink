export interface RuntimeConfig {
  doclink: string;
  pim: string;
}

declare global {
  interface Window {
    __DOCLINK_CONFIG__?: Partial<RuntimeConfig>;
  }
}

const defaults: RuntimeConfig = {
  doclink: "http://localhost:8080",
  pim: "http://localhost:8081",
};

export const config: RuntimeConfig = { ...defaults, ...(window.__DOCLINK_CONFIG__ ?? {}) };
