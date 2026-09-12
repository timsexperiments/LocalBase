import type { RuntimeModality } from "./modality";

export const DEFAULT_MAX_WAITING_INFERENCES = 16;
export const DEFAULT_INFERENCE_QUEUE_WAIT_MS = 60_000;

export class InferenceQueueCapacityError extends Error {
  constructor() {
    super("Inference queue capacity exceeded.");
    this.name = "InferenceQueueCapacityError";
  }
}
export class InferenceQueueTimeoutError extends Error {
  constructor(readonly queueWaitMs?: number) {
    super("Inference queue wait timed out.");
    this.name = "InferenceQueueTimeoutError";
  }
}
export class InferenceQueueUnavailableError extends Error {
  constructor(message = "Inference queue is unavailable.") {
    super(message);
    this.name = "InferenceQueueUnavailableError";
  }
}
export class InferenceQueueAbortedError extends Error {
  constructor() {
    super("Request aborted while waiting for inference admission.");
    this.name = "InferenceQueueAbortedError";
  }
}

export type InferenceQueueSnapshot = Readonly<{
  waiting: number;
  active: number;
  capacity: number;
  maxWaitMs: number;
  accepting: boolean;
}>;
export type InferencePermitSnapshot = Readonly<{
  active: number;
  slots: number;
  waiting: number;
}>;
export type QueuedLease<Value> = Readonly<{
  value: Value;
  queueWaitMs: number;
  permitSnapshot: InferencePermitSnapshot;
  release: () => void;
}>;
export type InferenceDispatchLease = Readonly<{
  signal: AbortSignal;
  start: () => void;
  throwIfCancelled: () => void;
  queueWaitMs: () => number;
}>;
type Pending<Value> = {
  modelId: string;
  signal?: AbortSignal;
  dispatch: (lease: InferenceDispatchLease) => Promise<Value>;
  resolve: (lease: QueuedLease<Value>) => void;
  reject: (error: Error) => void;
  timer?: ReturnType<typeof setTimeout>;
  removeAbort?: () => void;
  enqueuedAt: number;
  queueWaitMs?: number;
  cancellation: AbortController;
  cancellationError?: Error;
};

/** Owns bounded FIFO permits for one inference modality. */
export class InferenceQueue<Value> {
  private readonly pending: Pending<Value>[] = [];
  private readonly inFlight = new Set<Pending<Value>>();
  private active = 0;
  private activeModel: string | undefined;
  private slots = 1;
  private dispatching = false;
  private closedError: Error | undefined;

  constructor(
    readonly modality: RuntimeModality,
    private readonly options: Readonly<{
      maxWaiting?: number;
      waitMs?: number;
      isAdmitted: (value: Value) => boolean;
      resolvedSlots: (value: Value) => number;
      whenSlotsResolved?: (value: Value) => Promise<void>;
      discard?: (value: Value) => void;
      now?: () => number;
    }>,
  ) {}

  snapshot(): InferenceQueueSnapshot {
    return Object.freeze({
      waiting: this.pending.length,
      active: this.active,
      capacity: this.options.maxWaiting ?? DEFAULT_MAX_WAITING_INFERENCES,
      maxWaitMs: this.options.waitMs ?? DEFAULT_INFERENCE_QUEUE_WAIT_MS,
      accepting: this.closedError === undefined,
    });
  }

  acquire(
    modelId: string,
    dispatch: (lease: InferenceDispatchLease) => Promise<Value>,
    signal?: AbortSignal,
  ): Promise<QueuedLease<Value>> {
    if (signal?.aborted)
      return Promise.reject(new InferenceQueueAbortedError());
    if (this.closedError) return Promise.reject(this.closedError);
    if (
      this.pending.length >=
      (this.options.maxWaiting ?? DEFAULT_MAX_WAITING_INFERENCES)
    )
      return Promise.reject(new InferenceQueueCapacityError());
    return new Promise((resolve, reject) => {
      const item: Pending<Value> = {
        modelId,
        signal,
        dispatch,
        resolve,
        reject,
        enqueuedAt: this.now(),
        cancellation: new AbortController(),
      };
      item.timer = setTimeout(
        () =>
          this.remove(
            item,
            new InferenceQueueTimeoutError(
              Math.max(0, this.now() - item.enqueuedAt),
            ),
          ),
        this.options.waitMs ?? DEFAULT_INFERENCE_QUEUE_WAIT_MS,
      );
      const abort = () => this.remove(item, new InferenceQueueAbortedError());
      signal?.addEventListener("abort", abort, { once: true });
      item.removeAbort = () => signal?.removeEventListener("abort", abort);
      this.pending.push(item);
      void this.pump();
    });
  }

  rejectPending(error = new InferenceQueueUnavailableError()): void {
    const rejected = this.pending.splice(0);
    rejected.push(...this.inFlight);
    this.inFlight.clear();
    for (const item of rejected) {
      if (item.timer) clearTimeout(item.timer);
      item.removeAbort?.();
      item.cancellationError = error;
      item.cancellation.abort(error);
      item.reject(error);
    }
  }
  close(
    error = new InferenceQueueUnavailableError("Gateway is shutting down."),
  ): void {
    this.closedError = error;
    this.rejectPending(error);
  }
  private now(): number {
    return this.options.now?.() ?? performance.now();
  }
  private remove(item: Pending<Value>, error: Error): void {
    const index = this.pending.indexOf(item);
    if (index >= 0) this.pending.splice(index, 1);
    else if (!this.inFlight.delete(item)) return;
    if (item.timer) clearTimeout(item.timer);
    item.removeAbort?.();
    item.cancellationError = error;
    item.cancellation.abort(error);
    item.reject(error);
    if (this.canDispatchHead()) void this.pump();
  }

  private canDispatchHead(): boolean {
    if (this.closedError) return false;
    const head = this.pending[0];
    if (!head || this.active >= this.slots) return false;
    return this.activeModel === undefined || this.activeModel === head.modelId;
  }

  private async pump(): Promise<void> {
    if (this.dispatching) return;
    this.dispatching = true;
    try {
      while (true) {
        if (!this.canDispatchHead()) return;
        const item = this.pending[0];
        if (!item) return;
        if (item.signal?.aborted) {
          item.reject(new InferenceQueueAbortedError());
          continue;
        }
        try {
          const throwIfCancelled = () => {
            if (item.cancellationError) throw item.cancellationError;
          };
          const start = () => {
            throwIfCancelled();
            if (this.pending[0] !== item || this.closedError)
              throw new InferenceQueueUnavailableError(
                "Inference admission is no longer owned by the queue.",
              );
            if (item.queueWaitMs !== undefined) return;
            this.pending.shift();
            this.inFlight.add(item);
            item.queueWaitMs = Math.max(0, this.now() - item.enqueuedAt);
            if (item.timer) clearTimeout(item.timer);
          };
          const value = await item.dispatch(
            Object.freeze({
              signal: item.cancellation.signal,
              start,
              throwIfCancelled,
              queueWaitMs: () => {
                if (item.queueWaitMs === undefined)
                  throw new Error("Inference dispatch has not started.");
                return item.queueWaitMs;
              },
            }),
          );
          if (!this.inFlight.delete(item)) {
            this.options.discard?.(value);
            continue;
          }
          item.removeAbort?.();
          const queueWaitMs = item.queueWaitMs;
          if (queueWaitMs === undefined)
            throw new Error("Inference dispatch completed before start.");
          if (!this.options.isAdmitted(value)) {
            item.resolve({
              value,
              queueWaitMs,
              permitSnapshot: Object.freeze({
                active: this.active,
                slots: this.slots,
                waiting: this.pending.length,
              }),
              release() {},
            });
            continue;
          }
          this.activeModel = item.modelId;
          this.active += 1;
          this.slots = Math.max(
            1,
            this.modality === "llm" ? this.options.resolvedSlots(value) : 1,
          );
          void this.options.whenSlotsResolved?.(value).then(
            () => {
              if (this.activeModel !== item.modelId) return;
              this.slots = Math.max(1, this.options.resolvedSlots(value));
              void this.pump();
            },
            () => {},
          );
          let released = false;
          item.resolve(
            Object.freeze({
              value,
              queueWaitMs,
              permitSnapshot: Object.freeze({
                active: this.active,
                slots: this.slots,
                waiting: this.pending.length,
              }),
              release: () => {
                if (released) return;
                released = true;
                this.active -= 1;
                if (this.active === 0) {
                  this.activeModel = undefined;
                  this.slots = 1;
                }
                void this.pump();
              },
            }),
          );
        } catch (error) {
          const pendingIndex = this.pending.indexOf(item);
          if (pendingIndex >= 0) this.pending.splice(pendingIndex, 1);
          else if (!this.inFlight.delete(item)) continue;
          if (item.timer) clearTimeout(item.timer);
          item.removeAbort?.();
          if (this.active === 0) this.activeModel = undefined;
          item.reject(
            error instanceof Error ? error : new Error(String(error)),
          );
        }
      }
    } finally {
      this.dispatching = false;
      if (this.canDispatchHead()) void this.pump();
    }
  }
}
