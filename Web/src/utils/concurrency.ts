import appConfig from '../../config.yaml';

/**
 * 批量请求的"减速器"。
 *
 * 为什么需要它：
 * 之前的批量签到用的是 `Promise.all(uids.map(...))`，会让 10 个账号的请求
 * 在同一毫秒全部发出。在学习通看来，这就是"同一出口 IP 在 1ms 内收到 10 个
 * 不同 UID 的签到请求"——教科书级的脚本特征，比加密参数更容易被识别。
 *
 * 这里做三件事：
 *   1. 限制同时"在飞"的请求数（默认 2，与后端 antiban.max_concurrency 对齐）；
 *   2. 每个请求启动前随机等待一段时间，把请求摊开到几百毫秒的区间里；
 *   3. 支持 AbortSignal，用户点"停止"时立刻退出。
 */

export interface ConcurrencyOptions {
  /** 同时进行的任务数上限，默认 2。 */
  concurrency?: number;
  /** 每个任务启动前的随机等待区间（毫秒），默认 [150, 900]。 */
  staggerMs?: [number, number];
  /** 外部取消信号。 */
  signal?: AbortSignal;
}

export const DEFAULT_SIGN_CONCURRENCY = 2;
export const DEFAULT_STAGGER_MS: [number, number] = [150, 900];

function clampInt(value: unknown, fallback: number, min: number, max: number): number {
  const n = Number(value);
  if (!Number.isFinite(n)) return fallback;
  return Math.min(max, Math.max(min, Math.floor(n)));
}

interface SignConfigSection {
  max_concurrency?: unknown;
  stagger_min_ms?: unknown;
  stagger_max_ms?: unknown;
}

const signConfig: SignConfigSection = (appConfig as { sign?: SignConfigSection })?.sign ?? {};

/** 批量签到并发上限，来自 Web/config.yaml 的 sign.max_concurrency。 */
export const SIGN_CONCURRENCY = clampInt(signConfig.max_concurrency, DEFAULT_SIGN_CONCURRENCY, 1, 8);

/** 每个账号启动前的随机等待区间（毫秒），来自 sign.stagger_min_ms / stagger_max_ms。 */
export const SIGN_STAGGER_MS: [number, number] = [
  clampInt(signConfig.stagger_min_ms, DEFAULT_STAGGER_MS[0], 0, 10_000),
  clampInt(signConfig.stagger_max_ms, DEFAULT_STAGGER_MS[1], 0, 10_000),
];

export function randomBetween(min: number, max: number): number {
  if (max <= min) return min;
  return min + Math.floor(Math.random() * (max - min + 1));
}

export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (ms <= 0) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      signal?.removeEventListener('abort', onAbort);
      reject(new DOMException('Aborted', 'AbortError'));
    };
    if (signal?.aborted) {
      clearTimeout(timer);
      resolve();
      return;
    }
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

/**
 * 以受限并发执行 worker，并在每次启动前加入随机抖动。
 *
 * 注意：worker 内部抛出的异常不会中断其它任务（与 Promise.all 的语义不同），
 * 这样单个账号失败不会影响其余同学——批量签到场景下这是期望行为。
 */
export async function mapWithConcurrency<T>(
  items: readonly T[],
  worker: (item: T, index: number) => Promise<void>,
  options: ConcurrencyOptions = {},
): Promise<void> {
  const concurrency = Math.max(1, options.concurrency ?? DEFAULT_SIGN_CONCURRENCY);
  const [staggerMin, staggerMax] = options.staggerMs ?? DEFAULT_STAGGER_MS;
  const signal = options.signal;

  const queue = items.map((item, index) => ({ item, index }));
  if (queue.length === 0) return;

  const workerCount = Math.min(concurrency, queue.length);

  const runners = Array.from({ length: workerCount }, async () => {
    for (;;) {
      if (signal?.aborted) return;
      const next = queue.shift();
      if (!next) return;

      try {
        await sleep(randomBetween(staggerMin, staggerMax), signal);
      } catch {
        return;
      }
      if (signal?.aborted) return;

      try {
        await worker(next.item, next.index);
      } catch (err) {
        // 单个任务失败不阻塞其余任务；错误由 worker 自己展示给用户。
        console.error('[concurrency] worker failed', err);
      }
    }
  });

  await Promise.all(runners);
}
