import type { RuntimeModality } from "./modality";

export type ModalityAdmission<Value> = Readonly<{
  modality: RuntimeModality;
  value: Value;
  onDetach: (callback: () => void) => void;
  markResponseStarted: () => void;
  release: () => void;
}>;

/** Tracks request work admitted to one replaceable backend. */
export class ModalityAdmissionBarrier {
  private accepting: boolean;
  private active = 0;
  private idle = Promise.resolve();
  private resolveIdle: (() => void) | undefined;
  private readonly pendingResponses = new Set<() => void>();

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
        if (released || responseStarted) return;
        onDetach = callback;
        this.pendingResponses.add(callback);
      },
      markResponseStarted: () => {
        if (responseStarted) return;
        responseStarted = true;
        if (onDetach) this.pendingResponses.delete(onDetach);
      },
      release: () => {
        if (released) return;
        released = true;
        if (onDetach) this.pendingResponses.delete(onDetach);
        this.active -= 1;
        if (this.active === 0) {
          this.resolveIdle?.();
          this.resolveIdle = undefined;
        }
      },
    };
  }

  private drainLeases(cancelPending: boolean): Promise<void> {
    this.detach();
    if (cancelPending) {
      for (const cancelPendingResponse of this.pendingResponses) {
        cancelPendingResponse();
      }
    }
    return this.idle;
  }

  drain(): Promise<void> {
    return this.drainLeases(true);
  }

  drainWithoutCancellation(): Promise<void> {
    return this.drainLeases(false);
  }
}
