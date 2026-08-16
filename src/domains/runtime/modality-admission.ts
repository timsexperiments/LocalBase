import type { RuntimeModality } from "./modality";

export type ModalityAdmission<Value> = Readonly<{
  modality: RuntimeModality;
  value: Value;
  onPendingDetach: (callback: () => void) => void;
  onIdleCancellation: (callback: () => void) => void;
  markResponseStarted: () => void;
  cancel: () => void;
  release: () => void;
}>;

/** Tracks request work admitted to one replaceable backend. */
export class ModalityAdmissionBarrier {
  private accepting: boolean;
  private active = 0;
  private idle = Promise.resolve();
  private resolveIdle: (() => void) | undefined;
  private cancellationRequested = false;
  private pendingDetachCallbackInvoked = false;
  private readonly idleCancellationCallbacks = new Set<() => void>();
  private readonly pendingDetachCallbacks = new Set<() => void>();

  constructor(
    private readonly modality: RuntimeModality,
    configured: boolean,
  ) {
    this.accepting = configured;
  }

  attach(): void {
    this.accepting = true;
  }

  detach(): void {
    this.accepting = false;
  }

  detachIfIdle(): boolean {
    if (!this.accepting || this.active !== 0) return false;
    this.accepting = false;
    return true;
  }

  acquire<Value>(value: Value): ModalityAdmission<Value> | undefined {
    if (!this.accepting) return undefined;
    if (this.active === 0) {
      this.idle = new Promise<void>((resolve) => {
        this.resolveIdle = resolve;
      });
    }
    this.active += 1;
    let released = false;
    let responseStarted = false;
    let onPendingDetach: (() => void) | undefined;
    let onIdleCancellation: (() => void) | undefined;
    return {
      modality: this.modality,
      value,
      onPendingDetach: (callback) => {
        if (released || responseStarted || onPendingDetach) return;
        onPendingDetach = callback;
        this.pendingDetachCallbacks.add(callback);
      },
      onIdleCancellation: (callback) => {
        if (released || onIdleCancellation) return;
        onIdleCancellation = callback;
        this.idleCancellationCallbacks.add(callback);
      },
      markResponseStarted: () => {
        if (responseStarted) return;
        responseStarted = true;
        if (onPendingDetach) {
          this.pendingDetachCallbacks.delete(onPendingDetach);
        }
      },
      cancel: () => {
        if (released) return;
        released = true;
        this.cancellationRequested = true;
        this.release(onPendingDetach);
      },
      release: () => {
        if (released) return;
        released = true;
        this.release(onPendingDetach);
      },
    };
  }

  private release(callback: (() => void) | undefined): void {
    if (callback) this.pendingDetachCallbacks.delete(callback);
    this.active -= 1;
    if (this.active !== 0) return;

    const cancellation = this.cancellationRequested
      ? this.idleCancellationCallbacks.values().next().value
      : undefined;
    if (cancellation) this.detach();
    this.cancellationRequested = false;
    this.idleCancellationCallbacks.clear();
    this.pendingDetachCallbacks.clear();
    this.resolveIdle?.();
    this.resolveIdle = undefined;
    try {
      cancellation?.();
    } finally {
      this.pendingDetachCallbackInvoked = false;
    }
  }

  private detachPending(): void {
    if (this.pendingDetachCallbackInvoked) return;
    const callback = this.pendingDetachCallbacks.values().next().value;
    if (!callback) return;
    this.pendingDetachCallbackInvoked = true;
    callback();
  }

  private drainLeases(detachPending: boolean): Promise<void> {
    this.detach();
    if (detachPending) this.detachPending();
    return this.idle;
  }

  drain(): Promise<void> {
    return this.drainLeases(true);
  }

  drainWithoutCancellation(): Promise<void> {
    return this.drainLeases(false);
  }
}
