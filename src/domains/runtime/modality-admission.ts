import type { RuntimeModality } from "./modality";

export type ModalityAdmission<Value> = Readonly<{
  modality: RuntimeModality;
  value: Value;
  onDetach: (callback: () => void) => void;
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
  private cancellationCallbackInvoked = false;
  private readonly cancellationCallbacks = new Set<() => void>();
  private readonly pendingCancellationCallbacks = new Set<() => void>();

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
    let onDetach: (() => void) | undefined;
    return {
      modality: this.modality,
      value,
      onDetach: (callback) => {
        if (released || onDetach) return;
        onDetach = callback;
        this.cancellationCallbacks.add(callback);
        if (!responseStarted) {
          this.pendingCancellationCallbacks.add(callback);
        }
      },
      markResponseStarted: () => {
        if (responseStarted) return;
        responseStarted = true;
        if (onDetach) {
          this.pendingCancellationCallbacks.delete(onDetach);
        }
      },
      cancel: () => {
        if (released) return;
        released = true;
        this.cancellationRequested = true;
        this.release(onDetach);
      },
      release: () => {
        if (released) return;
        released = true;
        this.release(onDetach);
      },
    };
  }

  private release(callback: (() => void) | undefined): void {
    if (callback) this.pendingCancellationCallbacks.delete(callback);
    this.active -= 1;
    if (this.active !== 0) return;

    const cancellation = this.cancellationRequested
      ? this.takeCancellationCallback(this.cancellationCallbacks)
      : undefined;
    this.cancellationRequested = false;
    this.cancellationCallbacks.clear();
    this.pendingCancellationCallbacks.clear();
    this.resolveIdle?.();
    this.resolveIdle = undefined;
    try {
      cancellation?.();
    } finally {
      this.cancellationCallbackInvoked = false;
    }
  }

  private cancelPending(): void {
    const cancellation = this.takeCancellationCallback(
      this.pendingCancellationCallbacks,
    );
    cancellation?.();
  }

  private takeCancellationCallback(
    callbacks: ReadonlySet<() => void>,
  ): (() => void) | undefined {
    if (this.cancellationCallbackInvoked) return undefined;
    const callback = callbacks.values().next().value;
    if (callback) this.cancellationCallbackInvoked = true;
    return callback;
  }

  private drainLeases(cancelPending: boolean): Promise<void> {
    this.detach();
    if (cancelPending) this.cancelPending();
    return this.idle;
  }

  drain(): Promise<void> {
    return this.drainLeases(true);
  }

  drainWithoutCancellation(): Promise<void> {
    return this.drainLeases(false);
  }
}
